package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/delegation"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/instant"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/logging"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resource"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resourceurl"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

// Call is one request to one endpoint.
type Call struct {
	Service   *Service
	Endpoint  manifest.Endpoint
	R         *http.Request
	W         http.ResponseWriter
	Ctx       context.Context
	RequestID string
	// Received is when the request arrived: the Trails eventTime.
	Received time.Time
	// OrganizationID is the path's :orgId (public routes).
	OrganizationID string
	// ProjectID is the project the call is about, for Trails: the path's :projectId, or set by
	// the handler (an operation's project).
	ProjectID *string
	// ServiceCaller is who called an /internal route.
	ServiceCaller string

	// On an edge host: the request's alias and its public origin, https://<alias>.<edge domain>.
	alias      string
	publicBase string
	// A JSON-RPC endpoint's call: its method (SetRPCMethod, else the body's), and the error a
	// JSON-RPC answer carried with HTTP 200 (SetTrailError).
	rpcMethod  string
	trailError *httpapi.Error

	private                bool
	stream                 bool
	idempotencyFingerprint string
	internalCall           bool
	requestParameters      map[string]any
	params                 []string
	body                   []byte
	bodyValue              any
	credential             authz.Credential
	invocationToken        string
	perm                   *authz.Permissions
	resources              []trails.Resource
	recorded               bool
	// handedOff: the success event was written elsewhere (Recorded), so nothing the kit does
	// afterwards, a rolled-back transaction included, records the call again.
	handedOff bool
	responded bool
	// Additional is the Trails event's additionalEventData.
	Additional map[string]any
	// What a read responded, for its Trails event.
	lastStatus int
	lastBody   any
	// Quotas: the endpoint's plan, and what its check granted (released after the response).
	quotaPlan     *quota.Plan
	quotasChecked bool
	grant         *quota.Grant
	// [being built] A delegated call's reference (Delegated) and its header exactly as received;
	// an edge's last batch answer, which Delegation builds its reference from.
	delegated       *delegation.Reference
	delegatedHeader string
	lastBatch       *authz.BatchAnswer
	// delegationRefused: a delegated check didn't vouch for the customer (org-api refused, or a
	// check wasn't delegable), so the call's failure isn't recorded even if an earlier check
	// attributed it.
	delegationRefused bool
	// endpointAllowed: a check allowed the endpoint's own action (a batch: every check, one of them
	// the endpoint's action), so a replay may answer the stored response (Replay).
	endpointAllowed bool
	// inTx: the call is inside Transact's transaction, where Replay can't look the key up again;
	// inPrepare: inside TransactPrepared's prepare, which runs only once the key was looked up and
	// claimed.
	inTx      bool
	inPrepare bool
	// replayLooked: a quota check already looked for a stored response (replayBeforeQuotas) and
	// found none, so a second check doesn't look again.
	replayLooked bool
}

// Param is a path parameter by its manifest name (orgId, widgetId).
func (c *Call) Param(name string) string { return c.R.PathValue(name) }

// Body is the raw request body (at most 1 MiB).
func (c *Call) Body() []byte { return c.body }

// SetRequestParameters replaces automatic Trails request parameters with explicit metadata.
// Without it an event keeps path and query parameters and only the body's field names, their
// values hidden (nothing from a refused call's body); call it to record a body's values.
// Only pass safe operational fields, never document contents, filenames or extracted text.
// The catalog's sensitive fields and the normal secret-key redaction still apply.
func (c *Call) SetRequestParameters(parameters map[string]any) {
	c.requestParameters = maps.Clone(parameters)
	if c.requestParameters == nil {
		c.requestParameters = map[string]any{}
	}
}

// SetIdempotencyInput fingerprints explicit upload metadata, including its content digest.
// Call after authorization and streaming, before Transact. Never pass document bytes or
// extracted text. Only the hash is retained; metadata is not added to Trails. Responses
// persisted by Transact must likewise contain safe resource metadata, not document contents.
func (c *Call) SetIdempotencyInput(value any) error {
	if !c.stream || c.responded {
		return httpapi.Internal(errors.New("idempotency input requires an active HandleStream call"))
	}
	b, err := canonical.Marshal(map[string]any{"method": c.R.Method, "path": c.R.URL.Path, "body": value})
	if err != nil {
		return httpapi.Internal(errors.New("invalid stream idempotency metadata"))
	}
	c.idempotencyFingerprint = canonical.SHA256Hex(b)
	return nil
}

// SetRPCMethod names the JSON-RPC method the call is, for an endpoint whose trail is per JSON-RPC
// method (contracts/trails.md, "JSON-RPC endpoints"). The kit reads the method from the request
// body itself when it reads the body; a HandlePrivate handler, whose body the kit never reads,
// must call it once it has parsed the request, or the call records the trail's "otherwise".
func (c *Call) SetRPCMethod(method string) { c.rpcMethod = method }

// SetTrailError records a JSON-RPC error answered with HTTP 200 (a tool error: MCP Central's
// isError results) as the call's errorCode and errorMessage, exactly what the client got, so the
// event of a call that failed says so although its HTTP status didn't. It takes effect when the
// call is recorded (Transact, or after the response for a read); a later HTTP failure's error
// wins.
func (c *Call) SetTrailError(code, message string) {
	c.trailError = &httpapi.Error{Code: code, Message: message}
}

// trail is the catalog entry this call records, or nil: the endpoint's trail, or for a trail per
// JSON-RPC method the entry of the call's method (SetRPCMethod, else the body's "method").
func (c *Call) trail() *manifest.Trail {
	t := c.Endpoint.Trail
	if !t.RPC() {
		return t
	}
	method := c.rpcMethod
	if method == "" {
		if body, ok := c.bodyValue.(map[string]any); ok {
			method, _ = body["method"].(string)
		}
	}
	return t.ForMethod(method)
}

// Permissions is org-api's answer, once the call is authorized (nil before).
func (c *Call) Permissions() *authz.Permissions { return c.perm }

// Principal is who's calling, once authorized.
func (c *Call) Principal() authz.Principal {
	if c.perm == nil {
		return authz.Principal{}
	}
	return c.perm.Principal
}

// Alias is an edge-host request's alias, the one its host names ("" on the API host).
func (c *Call) Alias() string { return c.alias }

// PublicBase is the request's public origin: on an edge host https://<alias>.<edge domain>, else
// the configured public base URL (authz.Client.PublicBaseURL). A product builds the URLs it
// publishes for this request from it (MCP Central's protected-resource metadata), never from Host.
func (c *Call) PublicBase() string {
	if c.publicBase != "" {
		return c.publicBase
	}
	if c.Service.OrgAPI != nil {
		return c.Service.OrgAPI.PublicBaseURL
	}
	return ""
}

// resolveEdge names an edge-host request's organization from its host's alias, through the
// product's OrgResolver (contracts/registration.md, "Edge endpoints"). redirected is true once it
// has answered 308 for a retired alias.
func (c *Call) resolveEdge() (redirected bool, err error) {
	s := c.Service
	domain := s.edgeDomain()
	alias, ok := edgeAlias(c.R.Host, domain)
	if !ok {
		return false, httpapi.NotFound("Organization")
	}
	if s.OrgResolver == nil {
		return false, httpapi.Internal(errors.New("service: an edge-host endpoint without Options.OrgResolver"))
	}
	tenant, err := s.OrgResolver(c.R, alias)
	if err != nil {
		var e *httpapi.Error
		if errors.As(err, &e) {
			return false, e
		}
		return false, httpapi.Unavailable("The organization can't be looked up right now; try again shortly", err)
	}
	if !ids.IsUUID(tenant.OrganizationID) || !dnsLabel.MatchString(tenant.Alias) {
		return false, httpapi.Unavailable("The organization can't be looked up right now; try again shortly", fmt.Errorf("service: OrgResolver answered %+v for %q", tenant, alias))
	}
	if tenant.Alias != alias {
		// A retired alias: the same path and query on the current alias's host, whatever the method.
		c.responded = true
		c.W.Header().Set("Location", "https://"+tenant.Alias+"."+domain+c.R.URL.RequestURI())
		c.W.WriteHeader(http.StatusPermanentRedirect)
		return true, nil
	}
	c.OrganizationID = tenant.OrganizationID
	c.alias = alias
	c.publicBase = "https://" + alias + "." + domain
	return false, nil
}

// readCredential takes the caller's credential as the endpoint accepts it, with the edge host's
// public base.
func (c *Call) readCredential() error {
	var opts []authz.CredentialOption
	cookie := c.Endpoint.Credentials == manifest.CredentialsCookie
	switch {
	case cookie:
		edge := c.Service.Manifest.Edge
		if edge == nil || edge.Cookie == nil {
			return httpapi.Internal(errors.New("service: credentials cookie without the manifest's edge.cookie"))
		}
		opts = append(opts, authz.ProductCookie(edge.Cookie.Name))
	case c.Endpoint.Credentials == manifest.CredentialsBearer || c.Endpoint.OnEdgeHost():
		// The platform's session cookie never reaches an edge host (contracts/identity-and-authz.md,
		// "Bearer-only endpoints").
		opts = append(opts, authz.BearerOnly())
	}
	cred, err := authz.CredentialFrom(c.R, opts...)
	if err != nil {
		return err
	}
	if c.publicBase != "" {
		cred = cred.WithPublicBase(c.publicBase)
	}
	if cookie {
		// The cookie's token is bound to the product's audience label in the organization the
		// host names (contracts/identity-and-authz.md, "Product cookies").
		base := ""
		if c.Service.OrgAPI != nil {
			base = c.Service.OrgAPI.PublicBaseURL
		}
		origin, ok := resourceurl.Origin(base)
		if !ok || c.OrganizationID == "" {
			return httpapi.Internal(fmt.Errorf("service: a product cookie needs the API's public base URL (%q) and the edge host's organization", base))
		}
		cred = cred.WithRequestURL(origin + "/v1/organizations/" + c.OrganizationID + "/" + c.Service.Manifest.Edge.Cookie.Audience)
	}
	c.credential = cred
	return nil
}

// Authenticate reads the caller's credential on a public endpoint (auth none, contracts/
// registration.md, "Public endpoints"), which the kit doesn't do on its own. A redirect handler
// that acts for the person (MCP Central's upstream OAuth callback re-checking
// mcpcentral.connections.connect) calls it, sets OrganizationID (and ProjectID) from its own state
// when the path names none, then calls Check or CheckBatch as on any route: from org-api's 200 the
// call is attributable. Authenticate's refusal (401: no credential, or not one this endpoint
// takes) is the handler's to answer, since a redirect may send the person to sign in instead.
// The endpoint's trail is null, so the kit records nothing; the handler records the operation it
// completes itself (TrailEvent, then trails.Insert in the transaction of its change).
// askAsCaller refuses to ask org-api or Quotas about the caller on an endpoint that reads no
// caller credential: a receipt-handle endpoint (the handles in the body are the capability) or an
// invocation-token endpoint (the product verifies its own token). Calling Authorize, Check,
// CheckBatch, AuthorizeProject, AuthorizeMissing or a quota check there is the handler's bug.
func (c *Call) askAsCaller(method string) error {
	switch c.Endpoint.Auth {
	case manifest.AuthReceiptHandle, manifest.AuthInvocationToken:
		return httpapi.Internal(fmt.Errorf("service: %s on an auth %s endpoint, which reads no caller credential", method, c.Endpoint.Auth))
	}
	return nil
}

func (c *Call) Authenticate() error {
	if c.Endpoint.Auth != "none" {
		return httpapi.Internal(errors.New("service: Authenticate is for public endpoints; the kit reads the credential on every other route"))
	}
	return c.readCredential()
}

// TrailEvent is a Trails event for an operation a public redirect handler completes as the
// authorized caller (MCP Central's ConnectServer at its OAuth callback): an ApiCall write in the
// call's organization and project, with org-api's userIdentity, the request's address, user agent
// and ID, the resources it touched, and the request parameters set with SetRequestParameters
// (never the redirect's own query, which carries codes and state); everything else null. The
// handler may fill in responseElements, errorCode and errorMessage, then writes it with
// trails.Insert in the transaction of its change. It's an error before the call is attributable.
func (c *Call) TrailEvent(eventName string) (*trails.Event, error) {
	if !c.Attributable() {
		return nil, errors.New("service: TrailEvent needs a caller org-api has vouched for (Check or CheckBatch first)")
	}
	parameters := map[string]any{}
	if len(c.requestParameters) > 0 {
		parameters = trails.RequestParameters(nil, nil, c.requestParameters, nil)
	}
	ev := &trails.Event{
		EventVersion:      "1.0",
		EventID:           ids.NewV7(),
		EventTime:         instant.Format(c.Received),
		EventType:         trails.APICall,
		EventCategory:     "Management",
		EventSource:       c.Service.Manifest.EventSource(),
		EventName:         eventName,
		OrganizationID:    c.OrganizationID,
		ProjectID:         c.ProjectID,
		UserIdentity:      c.perm.UserIdentity,
		RequestParameters: parameters,
		Resources:         slices.Clone(c.resources),
		RequestID:         &c.RequestID,
	}
	if ip := httpapi.ClientIP(c.R); ip != "" {
		ev.SourceIPAddress = &ip
	}
	if ua := c.userAgent(); ua != "" {
		ev.UserAgent = &ua
	}
	if ev.Resources == nil {
		ev.Resources = []trails.Resource{}
	}
	return ev, nil
}

// invocationBearer is what an invocation token may look like: the kit bounds it and leaves its
// meaning to the product.
var invocationBearer = regexp.MustCompile(`^Bearer ([A-Za-z0-9._~-]{1,512})$`)

// prepareInvocation is prepare for an invocation-token endpoint (contracts/registration.md,
// "Invocation-token endpoints"): the bearer and the body; no org-api, no cookie, and no Origin
// check, since a browser never attaches a bearer on its own.
func (c *Call) prepareInvocation() error {
	m := invocationBearer.FindStringSubmatch(c.R.Header.Get("Authorization"))
	if m == nil {
		return httpapi.Unauthenticated("An invocation token is required: Authorization: Bearer <token>")
	}
	c.invocationToken = m[1]
	if c.private {
		return nil
	}
	body, bodyErr := httpapi.ReadBody(c.R, httpapi.MaxBodyBytes)
	if bodyErr != nil {
		return bodyErr
	}
	c.body = body
	return nil
}

// prepareReceipt is prepare for a receipt-handle endpoint (contracts/registration.md,
// "Receipt-handle endpoints"): only the body, whose product-signed handles are the capability. No
// credential is read (not the cookie, not a bearer), no org-api call is made and no Origin is
// checked: nothing ambient authorizes anything here, so a cross-site request gains nothing.
func (c *Call) prepareReceipt() error {
	if c.private {
		return nil
	}
	body, bodyErr := httpapi.ReadBody(c.R, httpapi.MaxBodyBytes)
	if bodyErr != nil {
		return bodyErr
	}
	c.body = body
	return nil
}

// readCallbackBody reads a public callback's body before its handler runs (Body), bounded as on
// every route (over httpapi.MaxBodyBytes is 413). A private handler reads R.Body itself.
func (c *Call) readCallbackBody() error {
	if c.private {
		return nil
	}
	body, bodyErr := httpapi.ReadBody(c.R, httpapi.MaxBodyBytes)
	if bodyErr != nil {
		return bodyErr
	}
	c.body = body
	return nil
}

// InvocationToken is the bearer an auth invocationToken endpoint was called with, unchecked: the
// product verifies it before doing anything (Workflows: the engine owner). Empty on every other
// endpoint.
func (c *Call) InvocationToken() string { return c.invocationToken }

// prepare runs the checks that come before authorization. None of their refusals is recorded:
// nobody is known to be a member of the organization yet.
func (c *Call) prepare() error {
	if err := c.readCredential(); err != nil {
		return err
	}
	cred := c.credential
	// The session cookie reaches every *.ezghcloud.com API from every sibling site, and
	// SameSite=Lax doesn't help between same-site origins, so cookie writes must come from an
	// allowed Origin. A Bearer header (an API key or an OAuth access token) is never attached by a
	// browser on its own.
	switch {
	case cred.FromProductCookie:
		// A product's edge cookie is host-only, but SameSite doesn't separate tenants under one
		// edge domain: a write must come from this very host (contracts/identity-and-authz.md,
		// "Product cookies").
		if httpapi.IsWrite(c.R.Method) && c.R.Header.Get("Origin") != c.publicBase {
			return httpapi.ForbiddenOrigin()
		}
	case !cred.IsBearer() && httpapi.IsWrite(c.R.Method) && c.Service.Origins.Allowed(c.R.Header.Get("Origin")) == "":
		return httpapi.ForbiddenOrigin()
	}
	// A malformed ID is 404, like an unknown one, so IDs can't be probed; a prefixed ID becomes
	// its UUID.
	if err := c.resolvePathIDs(c.OrganizationID); err != nil {
		return err
	}
	if !c.Endpoint.OnEdgeHost() {
		c.OrganizationID = c.R.PathValue("orgId")
	}
	if project := c.R.PathValue("projectId"); project != "" {
		c.ProjectID = &project
	}
	if c.private {
		return nil
	}
	return c.readBody()
}

// readBody reads the body (at most httpapi.MaxBodyBytes) and its JSON value, for the event.
func (c *Call) readBody() error {
	body, bodyErr := httpapi.ReadBody(c.R, httpapi.MaxBodyBytes)
	if bodyErr != nil {
		return bodyErr
	}
	c.body = body
	if len(body) > 0 {
		c.bodyValue, _ = canonical.Decode(body)
	}
	return nil
}

// ---- Authorization ------------------------------------------------------------------------

// Authorize asks org-api whether the caller may do action on resourceName ("" for the
// organization), forwarding only their credential. From org-api's 200 on, the call is
// attributable and recorded; a refused action is 403 access_denied, recorded with the error.
//
// Once allowed, the endpoint's quotas are checked with Quotas (CheckQuotas): one call, before the
// handler changes anything; a denied quota is 429 quota_exceeded, recorded like any failure. A
// retry with a stored Idempotency-Key response gets that response first, and Authorize returns
// ErrReplayed, which the handler returns as is (replayBeforeQuotas). A handler that answers
// through its own response writer (sso-stack's unsealing writer) installs it in c.W before
// Authorize, so the replay goes through it too.
func (c *Call) Authorize(action, resourceName string) error {
	return c.authorize(action, resourceName, true)
}

// authorize is Authorize; consume is false where the request is answered 404 whatever org-api
// says (a missing project or resource), which consumes no quota.
func (c *Call) authorize(action, resourceName string, consume bool) error {
	perm, err := c.Check([]string{action}, resourceName)
	if err != nil {
		return err
	}
	if !perm.Allows(action) {
		return httpapi.AccessDenied(action, c.callerUsedAPIKey())
	}
	if consume {
		return c.checkQuotas(resourceName)
	}
	return nil
}

// ---- Quotas ---------------------------------------------------------------------------------

// CheckQuotas makes the endpoint's one check with Quotas, if it has quotas and hasn't made it yet:
// for a handler that authorizes with Check rather than Authorize. Authorize calls it itself.
func (c *Call) CheckQuotas() error {
	if err := c.askAsCaller("CheckQuotas"); err != nil {
		return err
	}
	return c.checkQuotas("")
}

func (c *Call) checkQuotas(resourceName string) error {
	if c.quotasChecked || c.quotaPlan == nil {
		return nil
	}
	if err := c.replayBeforeQuotas(); err != nil {
		return err
	}
	c.quotasChecked = true
	grant, err := c.Service.Quotas.Check(c.Ctx, c.quotaPlan, c.quotaRequest(resourceName))
	if err != nil {
		return err
	}
	c.grant = grant
	return nil
}

// replayBeforeQuotas answers a retry's stored Idempotency-Key response (Replay, returning
// ErrReplayed) before a quota check can count it or refuse it: a retry of a create that took the
// last of a quota gets its stored 201, not 429 (contracts/api-conventions.md, "Idempotency keys").
// Authorize, AuthorizeProject, CheckQuotas and CheckQuotaItems call it, so whichever way a handler
// checks (Check, CheckCreate or CheckBatch, then CheckQuotas) the replay comes first. It doesn't
// look inside TransactPrepared (its prepare and fn run only once the key was looked up), or on a
// stream before its idempotency input is set (SetIdempotencyInput: a streamed upload's
// fingerprint needs its bytes, which come after the quota check).
func (c *Call) replayBeforeQuotas() error {
	if c.inTx || c.inPrepare || c.replayLooked || (c.stream && c.idempotencyFingerprint == "") {
		return nil
	}
	// Replay looks only once a check allowed the endpoint's action (and then sets replayLooked); until
	// then, a later check may.
	return c.Replay()
}

func (c *Call) quotaRequest(resourceName string) quota.Request {
	r := quota.Request{OrganizationID: c.OrganizationID, RequestID: c.RequestID, Param: c.R.PathValue}
	if c.ProjectID != nil {
		r.ProjectID = *c.ProjectID
	}
	if c.perm != nil {
		r.OrganizationName = c.perm.Organization.ResourceName
	}
	if name, ok := resource.Parse(resourceName, c.Service.Regions); ok && len(name.Chain) >= 2 && strings.HasPrefix(name.Chain[1], "prj_") {
		r.ProjectName = resource.Project(name.Chain[0], name.Chain[1])
		if len(name.Chain) > 2 {
			r.ResourceName = resourceName
		}
	}
	return r
}

// CheckQuotaItems checks quotas whose amount depends on the request (a batch create of count
// instances), which the manifest doesn't list: after authorization, before the change. Its leases
// are released with the endpoint's, after the response.
func (c *Call) CheckQuotaItems(items []quota.PlanItem) error {
	if err := c.askAsCaller("CheckQuotaItems"); err != nil {
		return err
	}
	if len(items) > 0 {
		if err := c.replayBeforeQuotas(); err != nil {
			return err
		}
	}
	grant, err := c.Service.Quotas.CheckItems(c.Ctx, c.quotaRequest(""), items)
	if err != nil {
		return err
	}
	if c.grant == nil {
		c.grant = grant
	} else {
		c.grant.Merge(grant)
	}
	return nil
}

// QuotaLimit is an allocation quota's value for scopeKey, to count against in the change's
// transaction (quota.Reserve): from the endpoint's check when it asked, else resolved now. When
// Quotas didn't answer it's quota.Unlimited: count, without refusing.
func (c *Call) QuotaLimit(quotaID, scopeKey string) quota.Limit {
	if c.grant != nil {
		if limit, ok := c.grant.Limit(quotaID, scopeKey); ok {
			return limit
		}
	}
	return c.Service.Quotas.Resolve(c.Ctx, c.OrganizationID, []quota.Item{{Quota: quotaID, ScopeKey: scopeKey}})[0]
}

// QuotaGrant is what the endpoint's check granted (nil before it, or without quotas): its leases,
// for long work that must keep them (Grant.KeepAlive).
func (c *Call) QuotaGrant() *quota.Grant { return c.grant }

// Check asks org-api about several actions at once, when an endpoint's rule depends on more than
// one, and leaves the decision to the caller. Like Authorize, it makes the call attributable.
func (c *Call) Check(actions []string, resourceName string) (*authz.Permissions, error) {
	if err := c.askAsCaller("Check"); err != nil {
		return nil, err
	}
	perm, err := c.check(actions, resourceName)
	if err != nil {
		return nil, err
	}
	if resourceName != "" && resourceName != perm.Organization.ResourceName {
		c.AddResource(resourceName)
	}
	return perm, nil
}

// check asks org-api (or, on a delegated call, its delegated check) and attributes the call.
func (c *Call) check(actions []string, resourceName string) (*authz.Permissions, error) {
	var perm *authz.Permissions
	if c.delegated != nil {
		var err error
		if perm, err = c.checkDelegatedActions(actions, resourceName); err != nil {
			return nil, err
		}
	} else {
		var herr *httpapi.Error
		if perm, herr = c.Service.OrgAPI.Check(c.Ctx, c.credential, c.OrganizationID, actions, resourceName); herr != nil {
			return nil, herr
		}
	}
	c.attribute(perm)
	if c.Endpoint.Action != "" && perm.Allows(c.Endpoint.Action) {
		c.endpointAllowed = true
	}
	return perm, nil
}

// CheckCreate is Check for a create checked on the name its new resource will have (a planned
// resource name): the planned resource isn't named in the call's Trails event, since a refused or
// failed create never made it (the handler names it with AddResource once it's created); its
// project is, for a resource in a project, so the event names the project the create was about
// (contracts/trails.md, "The event").
func (c *Call) CheckCreate(actions []string, planned string) (*authz.Permissions, error) {
	if err := c.askAsCaller("CheckCreate"); err != nil {
		return nil, err
	}
	perm, err := c.check(actions, planned)
	if err != nil {
		return nil, err
	}
	if project, ok := resource.ProjectOf(planned); ok {
		c.AddResource(project)
	}
	return perm, nil
}

// CheckBatch asks org-api about several resources in one call (contracts/identity-and-authz.md,
// "Batch checks"), as the caller, with the product's ok_ credential: MCP Central's tools/list and
// tools/call. Like Check, from org-api's answer the call is attributable, its principal and
// userIdentity those the answer names, and the organization is the first of its resources; the
// checked resources aren't added (a product adds those it touched, AddResource). The caller's
// credential carries the request's public base, so an EZGH SSO token's requestUrl is the
// edge-host URL on an edge host. Each result's Allowed is the only pass: nil and false don't. It
// consumes no quota: call CheckQuotas when the endpoint has some.
func (c *Call) CheckBatch(checks []authz.BatchCheck) (*authz.BatchAnswer, error) {
	if err := c.askAsCaller("CheckBatch"); err != nil {
		return nil, err
	}
	if c.delegated != nil {
		answer, err := c.checkDelegated(checks)
		if err == nil {
			c.noteBatch(answer)
		}
		return answer, err
	}
	answer, err := c.Service.OrgAPI.CheckBatch(c.Ctx, c.credential, c.OrganizationID, checks)
	if err != nil {
		return nil, err
	}
	c.attribute(&authz.Permissions{Organization: answer.Organization, Principal: answer.Principal, APIKeyID: answer.APIKeyID, UserIdentity: answer.UserIdentity})
	c.lastBatch = answer
	c.noteBatch(answer)
	return answer, nil
}

// noteBatch marks the endpoint's action allowed when every check of a batch passed and one of
// them was the endpoint's action (Replay).
func (c *Call) noteBatch(answer *authz.BatchAnswer) {
	if c.Endpoint.Action == "" || answer == nil || len(answer.Results) == 0 {
		return
	}
	own := false
	for _, r := range answer.Results {
		if !r.Allowed() {
			return
		}
		own = own || r.Action == c.Endpoint.Action
	}
	if own {
		c.endpointAllowed = true
	}
}

// Credential is the caller's credential as the kit read it, with an edge host's public base
// applied: for a product that calls org-api through package authz itself, or builds a credential
// for another public base (WithPublicBase). Empty on a public endpoint until Authenticate.
func (c *Call) Credential() authz.Credential { return c.credential }

func (c *Call) attribute(perm *authz.Permissions) {
	c.perm = perm
	apiKeyID := ""
	if perm.APIKeyID != nil {
		apiKeyID = *perm.APIKeyID
	}
	logging.SetCaller(c.Ctx, logging.Caller{Type: perm.Principal.Type, ID: perm.Principal.ID, APIKeyID: apiKeyID, OrganizationID: perm.Organization.ID})
	if len(c.resources) == 0 || c.resources[0].ResourceName != perm.Organization.ResourceName {
		c.resources = append([]trails.Resource{{ResourceName: perm.Organization.ResourceName, Type: "Organization"}}, c.resources...)
	}
}

// AddResource adds a resource the call touched to its Trails event.
func (c *Call) AddResource(name string) {
	for _, r := range c.resources {
		if r.ResourceName == name {
			return
		}
	}
	c.resources = append(c.resources, trails.Resource{ResourceName: name, Type: c.typeOf(name)})
}

// typeOf is a resource's Trails type: the service's own manifest type, else the platform's
// registered type for its prefix (another product's network interface, volume or image;
// contracts/resources.md, "Resource types"), else Unknown.
func (c *Call) typeOf(name string) string {
	slug := resource.LastSlug(name)
	if len(slug) > 3 && slug[3] == '_' {
		if t, ok := c.Service.Manifest.ResourceTypeByPrefix(slug[:3]); ok {
			return resource.TypeName(t.Name)
		}
	}
	return resource.TypeOf(name)
}

// Scope is where a project-scoped call acts.
type Scope struct {
	Organization authz.DirectoryOrganization
	Project      authz.DirectoryProject
	// ProjectName is the project's resource name, ezgh::org_…:prj_….
	ProjectName string
}

// AuthorizeProject resolves the path's organization and project through org-api's directory
// (slugs aren't known otherwise) and checks action on the project. A project that doesn't
// exist, belongs to another organization or was deleted must look the same as a missing
// organization to outsiders, so membership is decided first: org-api's 404 is "Organization not
// found", and otherwise it's "Project not found" whether or not the action was allowed.
// AuthorizeProjectOnly checks access without consuming quotas. Use with TransactPrepared.
func (c *Call) AuthorizeProjectOnly(action string) (*Scope, error) {
	return c.authorizeProject(action, false)
}

func (c *Call) AuthorizeProject(action string) (*Scope, error) {
	return c.authorizeProject(action, true)
}

func (c *Call) authorizeProject(action string, consume bool) (*Scope, error) {
	if err := c.askAsCaller("AuthorizeProject"); err != nil {
		return nil, err
	}
	if c.ProjectID == nil {
		return nil, httpapi.Internal(errors.New("AuthorizeProject on a route without :projectId"))
	}
	dir, err := c.Service.OrgAPI.Lookup(c.Ctx, []string{c.OrganizationID}, []string{*c.ProjectID})
	if err != nil {
		return nil, httpapi.Unavailable("The directory is unavailable; try again shortly", err)
	}
	org, known := dir.Organizations[c.OrganizationID]
	if !known || org.State == authz.StateDeleted {
		// Nobody is a member of an organization that doesn't exist: the same 404 org-api gives.
		return nil, httpapi.NotFound("Organization")
	}
	project, live := dir.Project(c.OrganizationID, *c.ProjectID)
	if !live {
		if err := c.authorize(action, "", false); err != nil && !isAccessDenied(err) {
			return nil, err
		}
		return nil, httpapi.NotFound("Project")
	}
	name := resource.Project(org.Slug, project.Slug)
	if err := c.authorize(action, name, consume); err != nil {
		return nil, err
	}
	return &Scope{Organization: org, Project: project, ProjectName: name}, nil
}

// AuthorizeMissing is for a target the product didn't find (scoped to the path's organization
// and project): it checks action on the nearest parent that exists, then answers 404 for what
// ("Widget") whether or not the action was allowed. org-api's 404 stays "Organization not
// found", so a missing resource looks the same as a missing organization.
//
// A retry of a request that removed its target (a DELETE, with the same Idempotency-Key and the
// same request) finds it missing: when action is the endpoint's own and the caller may do it on
// that parent (and the call isn't inside Transact's transaction), AuthorizeMissing
// answers the stored response instead (Replay) and returns ErrReplayed, which the handler returns
// as is (contracts/api-conventions.md, "Idempotency keys"). A stream route before its
// SetIdempotencyInput has no fingerprint to replay by, so its retry is 404.
func (c *Call) AuthorizeMissing(action, what string) error {
	var err error
	if c.ProjectID != nil {
		_, err = c.authorizeProject(action, false)
	} else {
		err = c.authorize(action, "", false)
	}
	switch {
	case err == nil && !c.inTx && (!c.stream || c.idempotencyFingerprint != ""):
		if err := c.Replay(); err != nil {
			return err
		}
	case err == nil:
	case !isAccessDenied(err):
		return err
	}
	return httpapi.NotFound(what)
}

func isAccessDenied(err error) bool {
	var e *httpapi.Error
	return errors.As(err, &e) && e.Code == "access_denied"
}

// ---- Responding -----------------------------------------------------------------------------

// JSON responds with body. For a read the Trails event is recorded afterwards; a write responds
// through Transact instead, so its event commits with its change.
func (c *Call) JSON(status int, body any) {
	if c.unauthorizedDelegated(status) {
		c.fail(errDelegatedUnauthorized)
		return
	}
	c.responded = true
	httpapi.WriteJSON(c.W, status, body)
	c.lastBody, c.lastStatus = body, status
}

// Stream records an attributable success durably before sending headers or bytes. It is
// for private downloads without database mutations; successful writes use Transact.
// A failed writer (including a panic) aborts the transport without logging its contents.
func (c *Call) Stream(status int, headers http.Header, write func(io.Writer) error) error {
	if !c.private || c.responded || status < 200 || status >= 300 || write == nil || c.Endpoint.IdempotencyKey {
		return httpapi.Internal(errors.New("invalid private streaming response"))
	}
	if c.unauthorizedDelegated(status) {
		return errDelegatedUnauthorized
	}
	if !c.handedOff && !c.internalCall && c.trail() != nil && c.Attributable() && !c.recorded && !c.refusalsOnly() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Ctx), 5*time.Second)
		defer cancel()
		if err := trails.InsertAlone(ctx, c.Service.DB, c.event(status, nil, nil)); err != nil {
			return httpapi.Unavailable("The audit record could not be persisted; try again shortly", nil)
		}
		c.recorded = true
	}
	c.responded = true
	c.lastStatus = status
	defer func() {
		if recover() != nil {
			panic(http.ErrAbortHandler)
		}
	}()
	for name, values := range headers {
		c.W.Header()[name] = slices.Clone(values)
	}
	c.W.WriteHeader(status)
	if err := write(c.W); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

// Response is what a write answers, after its transaction commits.
type Response struct {
	ETag   string
	Status int
	Body   any
	// Location is the new resource (201) or the operation (202), path-absolute.
	Location string
	// RetryAfter is the suggested poll interval on a 202.
	RetryAfter int
	// OperationID marks an asynchronous answer: an Idempotency-Key replay answers this
	// operation as it is now.
	OperationID string
	// ReplayBody, when set, is what an Idempotency-Key replay answers instead of Body: a response
	// that carries a one-time secret (Workflows' CreateApp and RotateAppSecret) stores the body
	// without it (the secret's field null), so the secret never lands in idempotency_keys
	// (contracts/api-conventions.md, "Idempotency keys").
	ReplayBody any

	// resources is what a stored response's call named in its Trails event (lookupKey), for its
	// replay's event.
	resources []trails.Resource
}

// Transact runs a write: one short transaction holding the change, its Trails event, its usage
// rows and its idempotency record, opened after authorization (no transaction is ever held across
// a call to org-api). fn returns the response, or an error that rolls everything back; the
// failure is then recorded in a transaction of its own.
//
// With an Idempotency-Key (on endpoints that honor one), a retry of the same request gets the
// stored response with Idempotent-Replayed: true (an asynchronous one: its operation as it is
// now); the same key with a different request is 409 idempotency_conflict; while the first is
// still running, 409 idempotency_in_progress. Only successes are stored, so a request that
// failed can be retried with the same key.
func (c *Call) Transact(fn func(tx *db.Tx) (*Response, error)) error {
	return c.TransactPrepared(nil, fn)
}

// TransactPrepared is Transact with prepare, a step run before the transaction (a quota call, a
// call to another service), holding no database connection. With an Idempotency-Key it first
// claims the key in a short transaction of its own (idempotency_claims, claimKey): a second
// request with the key meanwhile is 409 idempotency_in_progress, a replay skips prepare, and the
// claim is deleted when the request ends. prepare runs under c.Ctx with a deadline below the
// claim's lease (the endpoint's timeoutSeconds, or 60 s), so it can't outlive the claim.
func (c *Call) TransactPrepared(prepare func() error, fn func(tx *db.Tx) (*Response, error)) error {
	if c.stream && c.Endpoint.IdempotencyKey && c.idempotencyFingerprint == "" {
		return httpapi.Internal(errors.New("stream idempotency input must be set before Transact"))
	}
	key, err := c.idempotencyKey()
	if err != nil {
		return err
	}
	// No connection is held between the transactions: prepare may take as long as a network call
	// does. With a key, the claim keeps a second request out while prepare runs.
	var resp *Response
	var token string
	replayed := false
	if prepare != nil {
		if key != "" {
			err := c.Service.DB.RunTx(c.Ctx, nil, func(tx *db.Tx) error {
				stored, err := c.lookupKey(tx, key)
				if err != nil || stored != nil {
					resp, replayed = stored, stored != nil
					if replayed {
						return c.recordReplay(tx, resp)
					}
					return err
				}
				token, err = c.claimKey(tx, key)
				return err
			})
			if err != nil {
				c.recorded = false
				return databaseError(err)
			}
			if token != "" {
				defer c.releaseKey(key, token)
				ctx, cancel := context.WithTimeout(c.Ctx, c.claimTimeout())
				outer := c.Ctx
				c.Ctx = ctx
				c.inPrepare = true
				err := prepare()
				c.inPrepare = false
				c.Ctx = outer
				cancel()
				if err != nil {
					return err
				}
			}
		} else if err := prepare(); err != nil {
			return err
		}
	}
	if !replayed {
		c.inTx = true
		err = c.Service.DB.RunTx(c.Ctx, nil, func(tx *db.Tx) error {
			if key != "" {
				stored, err := c.lookupKey(tx, key)
				if err != nil {
					return err
				}
				if stored != nil {
					resp, replayed = stored, true
					return c.recordReplay(tx, resp)
				}
				// Only a request with prepare looks at claims (one without never touches the table,
				// so a role not yet granted it still works); the fingerprint catches a key reused
				// across endpoints.
				if prepare != nil {
					if err := c.checkNoOtherClaim(tx, key, token); err != nil {
						return err
					}
				}
			}
			var err error
			if resp, err = fn(tx); err != nil {
				return err
			}
			if key != "" {
				if err := c.storeKey(tx, key, resp); err != nil {
					return err
				}
			}
			return c.record(tx, resp.Status, resp.Body)
		})
		c.inTx = false
		if err != nil {
			// Whatever record wrote rolled back with the transaction (fn failed, an ent operation in
			// it rolled back, or the commit itself failed), so fail records the failure on its own,
			// unless the call's event was handed off and committed elsewhere (handedOff stays set).
			c.recorded = false
			return databaseError(err)
		}
	}
	c.answer(resp, replayed)
	return nil
}

// answer writes a write's response, after its transaction committed.
func (c *Call) answer(resp *Response, replayed bool) {
	h := c.W.Header()
	if resp.ETag != "" {
		h.Set("ETag", resp.ETag)
	}
	if resp.Location != "" {
		h.Set("Location", resp.Location)
	}
	if resp.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(resp.RetryAfter))
	}
	if replayed {
		h.Set("Idempotent-Replayed", "true")
	}
	c.responded = true
	httpapi.WriteJSON(c.W, resp.Status, resp.Body)
}

// ErrReplayed is what Replay and AuthorizeMissing return once they have answered the request's
// stored Idempotency-Key response: the handler returns it as is, and the kit adds nothing (the
// replay is recorded as its own call).
var ErrReplayed = errors.New("service: answered with the stored Idempotency-Key response")

// Replay answers a retry of a completed request before the handler's own checks can refuse it:
// with an Idempotency-Key whose stored response is this request's (the same method, path and
// body), it answers that response, as Transact would, with Idempotent-Replayed: true, records the
// call, and returns ErrReplayed. Without a key, on an endpoint that takes none, or with no stored
// response yet, it returns nil and the handler goes on; the same key with a different request is
// 409 idempotency_conflict, and while the first request's transaction is still open, 409
// idempotency_in_progress.
//
// Call it once a check has allowed the endpoint's own action (Authorize, AuthorizeProject, Check
// or a batch whose every check passed): before that, or for a caller who may not, it does
// nothing. Call it before any check that the first request changes the answer to
// (contracts/api-conventions.md, "Idempotency keys"): a stop's "the instance isn't running", a
// delete's "not found" (AuthorizeMissing calls it itself), and before TransactPrepared (a live
// claim, even this request's own, is 409 idempotency_in_progress); never inside Transact's fn.
// A quota check replays first on its own (Authorize, AuthorizeProject, CheckQuotas and
// CheckQuotaItems call it before asking Quotas), so a retry at the limit still replays; a
// product's own rate limit goes after Replay. Transact still replays on its own; Replay only lets
// a retry get there.
func (c *Call) Replay() error {
	key, err := c.idempotencyKey()
	if err != nil || key == "" {
		return err
	}
	if c.inTx {
		return httpapi.Internal(errors.New("service: Replay inside Transact; call it before"))
	}
	if c.inPrepare {
		// TransactPrepared looked the key up and found nothing before claiming it for this request.
		return nil
	}
	if !c.endpointAllowed {
		// Only a caller allowed the endpoint's own action gets the stored response.
		return nil
	}
	if c.stream && c.idempotencyFingerprint == "" {
		return httpapi.Internal(errors.New("stream idempotency input must be set before Replay"))
	}
	var resp *Response
	err = c.Service.DB.RunTx(c.Ctx, nil, func(tx *db.Tx) error {
		stored, err := c.lookupKey(tx, key)
		if err != nil {
			return err
		}
		if stored == nil {
			// The first request may still be in its prepare step, holding a claim but no lock.
			if ok, err := claimsReachable(c.Ctx, tx); err != nil || !ok {
				return err
			}
			return c.checkNoOtherClaim(tx, key, "")
		}
		resp = stored
		return c.recordReplay(tx, resp)
	})
	if err != nil {
		c.recorded = false
		return databaseError(err)
	}
	if resp == nil {
		// Nothing stored: a quota check after this needn't look again (replayBeforeQuotas).
		c.replayLooked = true
		return nil
	}
	c.answer(resp, true)
	return ErrReplayed
}

// databaseError keeps the product's own errors and turns the database's into 503 (it's down, or
// it aborted the transaction for contention: a deadlock or a serialization failure) or 500
// (anything else is a bug).
func databaseError(err error) error {
	var e *httpapi.Error
	if errors.As(err, &e) {
		return e
	}
	if db.Unavailable(err) {
		return httpapi.Unavailable("The database is unavailable; try again shortly", err)
	}
	// A deadlock or serialization failure: the transaction rolled back whole, so the request can be
	// retried. The kit doesn't retry it itself: the handler's fn may have called other services,
	// which a second run would call again.
	if db.IsContention(err) {
		e := httpapi.Unavailable("The database is busy; try again shortly", err)
		e.RetryAfter = 1
		return e
	}
	return httpapi.Internal(err)
}

var idempotencyKeyForm = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)

func (c *Call) idempotencyKey() (string, error) {
	if !c.Endpoint.IdempotencyKey {
		return "", nil
	}
	values := c.R.Header.Values("Idempotency-Key")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) > 1 || !idempotencyKeyForm.MatchString(values[0]) {
		return "", httpapi.InvalidRequest("Request validation failed", httpapi.Issue{Path: "Idempotency-Key", Message: "must be 1-200 printable ASCII characters"})
	}
	return values[0], nil
}

// fingerprint is the SHA-256 of the canonical JSON of {method, path, body}.
func (c *Call) fingerprint() (string, error) {
	if c.stream {
		if c.idempotencyFingerprint == "" {
			return "", errors.New("stream idempotency input is missing")
		}
		return c.idempotencyFingerprint, nil
	}
	b, err := canonical.Marshal(map[string]any{"method": c.R.Method, "path": c.R.URL.Path, "body": c.bodyValue})
	if err != nil {
		return "", err
	}
	return canonical.SHA256Hex(b), nil
}

// idempotencyLockSpace keeps these advisory locks apart from any other use of them.
const idempotencyLockSpace = 0x69646b // "idk"

// claimLeaseSlack is how long past the endpoint's own timeout a claim outlives a request that
// never released it (a crash): a request can't still be running then.
const claimLeaseSlack = 60 * time.Second

// claimKey claims key for this request in tx, after lookupKey found no stored response, and
// returns the claim's token; another request's live claim is 409 idempotency_in_progress. A claim
// lapses at the endpoint's timeout (the server's 60 s by default) plus claimLeaseSlack.
func (c *Call) claimKey(tx *db.Tx, key string) (string, error) {
	timeout := c.claimTimeout()
	token := ids.NewV7()
	var got string
	err := tx.QueryRowContext(c.Ctx, `
		INSERT INTO idempotency_claims (organization_id, key, token, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		ON CONFLICT (organization_id, key) DO UPDATE SET token = excluded.token, expires_at = excluded.expires_at
		WHERE idempotency_claims.expires_at < now()
		RETURNING token::text`, c.OrganizationID, key, token, (timeout + claimLeaseSlack).Seconds()).Scan(&got)
	if db.IsNoRows(err) {
		e := httpapi.Conflict("idempotency_in_progress", "A request with this Idempotency-Key is still being processed")
		e.RetryAfter = 1
		return "", e
	}
	return got, err
}

// checkNoOtherClaim is 409 idempotency_in_progress while another request holds a live claim on
// key (its prepare is running), so a request that runs no prepare can't complete the key under it.
func (c *Call) checkNoOtherClaim(tx *db.Tx, key, token string) error {
	var other bool
	if err := tx.QueryRowContext(c.Ctx, `SELECT EXISTS (SELECT 1 FROM idempotency_claims
		WHERE organization_id = $1 AND key = $2 AND expires_at > now() AND token::text <> $3)`, c.OrganizationID, key, token).Scan(&other); err != nil {
		return err
	}
	if other {
		e := httpapi.Conflict("idempotency_in_progress", "A request with this Idempotency-Key is still being processed")
		e.RetryAfter = 1
		return e
	}
	return nil
}

// claimTimeout is how long a keyed prepare may run: the endpoint's timeoutSeconds, or 60 s. The
// claim lasts claimLeaseSlack longer.
func (c *Call) claimTimeout() time.Duration {
	if c.Endpoint.TimeoutSeconds > 0 {
		return time.Duration(c.Endpoint.TimeoutSeconds) * time.Second
	}
	return 60 * time.Second
}

// claimsReachable reports whether this role can delete from idempotency_claims: false when the
// table isn't there or the product hasn't granted it (kit README, the afde1fa sync note), so the
// purges skip it instead of failing, and without aborting the caller's transaction.
func claimsReachable(ctx context.Context, q db.Querier) (bool, error) {
	var ok bool
	err := q.QueryRowContext(ctx, `SELECT COALESCE(has_table_privilege(to_regclass('idempotency_claims'), 'DELETE'), false)`).Scan(&ok)
	return ok, err
}

// releaseKey deletes this request's claim when it ends, whatever happened, even once the
// request's context is done.
func (c *Call) releaseKey(key, token string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Ctx), 5*time.Second)
	defer cancel()
	if _, err := c.Service.DB.ExecContext(ctx, `DELETE FROM idempotency_claims WHERE organization_id = $1 AND key = $2 AND token = $3`, c.OrganizationID, key, token); err != nil {
		c.Service.Logger.WarnContext(c.Ctx, "releasing an Idempotency-Key claim; it lapses on its own", "error", err.Error())
	}
}

func (c *Call) lookupKey(tx *db.Tx, key string) (*Response, error) {
	fp, err := c.fingerprint()
	if err != nil {
		return nil, httpapi.Internal(err)
	}
	// The first request with this key holds the lock until it commits or rolls back; a second
	// one arriving meanwhile is told to come back rather than made to wait.
	var locked bool
	if err := tx.QueryRowContext(c.Ctx, `SELECT pg_try_advisory_xact_lock($1, hashtext($2 || ':' || $3))`, idempotencyLockSpace, c.OrganizationID, key).Scan(&locked); err != nil {
		return nil, err
	}
	if !locked {
		e := httpapi.Conflict("idempotency_in_progress", "A request with this Idempotency-Key is still being processed")
		e.RetryAfter = 1
		return nil, e
	}
	var stored struct {
		fingerprint string
		status      int
		body        []byte
		location    *string
		operationID *string
		etag        string
		resources   *string
	}
	err = tx.QueryRowContext(c.Ctx, `
		SELECT fingerprint, status, body::text, location, operation_id::text, etag, resources::text FROM idempotency_keys
		WHERE organization_id = $1 AND key = $2`, c.OrganizationID, key).
		Scan(&stored.fingerprint, &stored.status, &stored.body, &stored.location, &stored.operationID, &stored.etag, &stored.resources)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if stored.fingerprint != fp {
		return nil, httpapi.Conflict("idempotency_conflict", "This Idempotency-Key was used for a different request")
	}
	resp := &Response{ETag: stored.etag, Status: stored.status, Body: json.RawMessage(stored.body)}
	if stored.resources != nil {
		// A record this kit wrote; one it can't read only names fewer resources.
		_ = json.Unmarshal([]byte(*stored.resources), &resp.resources)
	}
	if stored.location != nil {
		resp.Location = *stored.location
	}
	if stored.operationID != nil {
		resp.OperationID = *stored.operationID
		resp.RetryAfter = 2
		if c.Service.ReplayOperation != nil {
			current, err := c.Service.ReplayOperation(c.Ctx, tx, c.OrganizationID, *stored.operationID)
			if err != nil {
				return nil, err
			}
			if current != nil {
				resp.Body = refreshOperation(json.RawMessage(stored.body), *stored.operationID, current)
			}
		}
	}
	return resp, nil
}

// refreshOperation is an asynchronous request's replay body: the operation as it is now. When the
// stored body is the operation with more beside it (Block Storage answers a create's operation
// with the new volume as volume), the replay keeps those other fields as they were stored and
// refreshes the operation's own, so a retry gets the same shape the first answer had.
func refreshOperation(stored json.RawMessage, operationID string, current any) any {
	var body map[string]json.RawMessage
	if json.Unmarshal(stored, &body) != nil {
		return current
	}
	var id string
	if json.Unmarshal(body["id"], &id) != nil || id != operationID {
		return current
	}
	b, err := json.Marshal(current)
	if err != nil {
		return current
	}
	var now map[string]json.RawMessage
	if json.Unmarshal(b, &now) != nil {
		return current
	}
	extra := false
	for k := range body {
		if _, ok := now[k]; !ok {
			extra = true
			break
		}
	}
	if !extra {
		return current
	}
	maps.Copy(body, now)
	return body
}

func (c *Call) storeKey(tx *db.Tx, key string, resp *Response) error {
	fp, err := c.fingerprint()
	if err != nil {
		return httpapi.Internal(err)
	}
	replay := resp.Body
	if resp.ReplayBody != nil {
		replay = resp.ReplayBody
	}
	body, err := httpapi.Marshal(replay)
	if err != nil {
		return httpapi.Internal(err)
	}
	var location, operationID, resources *string
	if resp.Location != "" {
		location = &resp.Location
	}
	if resp.OperationID != "" {
		operationID = &resp.OperationID
	}
	// The resources the call's event names (the response's too, as record adds them; at most the
	// 50 an event keeps), so a replay names them even once they're gone (a DELETE's target). A copy:
	// should this transaction fail, the failure's event names only what the call had named.
	kept := c.resources
	c.resources = slices.Clone(kept)
	c.addResourcesOf(resp.Body)
	named := c.resources
	c.resources = kept
	if len(named) > trails.MaxResources {
		named = named[:trails.MaxResources]
	}
	if len(named) > 0 {
		b, err := json.Marshal(named)
		if err != nil {
			return httpapi.Internal(err)
		}
		text := string(b)
		resources = &text
	}
	_, err = tx.ExecContext(c.Ctx, `
		INSERT INTO idempotency_keys (organization_id, key, fingerprint, status, body, location, operation_id, etag, resources)
		VALUES ($1, $2, $3, $4, $5::text::jsonb, $6, $7, $8, $9::text::jsonb)`,
		c.OrganizationID, key, fp, resp.Status, string(body), location, operationID, resp.ETag, resources)
	return err
}

// ---- Trails ---------------------------------------------------------------------------------

// userAgent is the event's userAgent: the request's, or on a delegated call the calling service's
// host (contracts/trails.md, "Delegated calls").
func (c *Call) userAgent() string {
	if c.delegated != nil {
		return trails.DelegatedUserAgent(c.delegated.Caller())
	}
	return c.R.UserAgent()
}

// Attributable reports whether org-api has vouched for the caller, so the call is recorded.
func (c *Call) Attributable() bool { return c.perm != nil }

func (c *Call) event(status int, body any, failure *httpapi.Error) *trails.Event {
	entry := c.trail()
	if failure == nil && c.trailError != nil {
		failure = c.trailError
	}
	// The OpenAI surface's monthly limit answers OpenAI's own insufficient_quota, which its clients
	// branch on; Trails records it as the platform's quota_exceeded (contracts/api-conventions.md,
	// "OpenAI-compatible surface").
	if failure != nil && failure.Code == httpapi.OpenAIInsufficientQuota && c.Endpoint.ErrorModel == manifest.ErrorModelOpenAI {
		recorded := *failure
		recorded.Code = "quota_exceeded"
		failure = &recorded
	}
	// GET (and HEAD, which the router serves as GET), and computation POSTs, which keep nothing
	// (contracts/trails.md, "The event"); not a JSON-RPC endpoint's, whose methods may act.
	readOnly := c.R.Method == http.MethodHead || c.Endpoint.ReadOnly()
	parameters := c.requestParameters
	if parameters == nil && c.private {
		parameters = map[string]any{}
	}
	if parameters == nil {
		path := map[string]string{}
		for _, p := range c.params {
			path[p] = c.R.PathValue(p)
		}
		query := map[string]any{}
		for k, v := range c.R.URL.Query() {
			if len(v) == 1 {
				query[k] = v[0]
			} else {
				items := make([]any, len(v))
				for i, s := range v {
					items[i] = s
				}
				query[k] = items
			}
		}
		body := c.bodyValue
		if failure != nil || status >= 400 {
			// A refused body may be anything the caller sent (an unknown field, a misnamed secret).
			body = nil
		}
		parameters = trails.DefaultRequestParameters(path, query, body, entry.Sensitive)
	} else if len(parameters) > 0 {
		parameters = trails.RequestParameters(nil, nil, parameters, entry.Sensitive)
	}
	ev := &trails.Event{
		EventVersion:      "1.0",
		EventID:           ids.NewV7(),
		EventTime:         instant.Format(c.Received),
		EventType:         trails.APICall,
		EventCategory:     entry.Category(),
		EventSource:       c.Service.Manifest.EventSource(),
		EventName:         entry.EventName,
		ReadOnly:          readOnly,
		OrganizationID:    c.OrganizationID,
		ProjectID:         c.ProjectID,
		UserIdentity:      c.perm.UserIdentity,
		RequestParameters: parameters,
		Resources:         c.resources,
		RequestID:         &c.RequestID,
	}
	if ip := httpapi.ClientIP(c.R); ip != "" {
		ev.SourceIPAddress = &ip
	}
	if ua := c.userAgent(); ua != "" {
		ev.UserAgent = &ua
	}
	if ev.Resources == nil {
		ev.Resources = []trails.Resource{}
	}
	if len(c.Additional) > 0 {
		ev.AdditionalEventData = c.Additional
	}
	if failure != nil {
		ev.ErrorCode, ev.ErrorMessage = &failure.Code, &failure.Message
	} else if c.R.Method != http.MethodGet && c.R.Method != http.MethodHead && status < 300 {
		// A computation POST is readOnly but keeps the identifiers of its successful answer, as
		// before it was: only GETs record none.
		generic, err := toGeneric(body)
		if err == nil {
			if c.private {
				ev.ResponseElements = privateResponseElements(generic)
			} else {
				ev.ResponseElements = trails.ResponseElements(generic, entry.ResponseElements)
			}
		}
	}
	return ev
}

// Private outputs can use name for a filename. Keep only opaque platform identifiers,
// including one-level response wrappers; catalog extras cannot reintroduce document data.
func privateResponseElements(body any) map[string]any {
	obj, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	pick := func(value any) map[string]any {
		m, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		out := map[string]any{}
		for _, key := range []string{"id", "slug", "resourceName"} {
			if v, ok := m[key].(string); ok {
				out[key] = v
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	if out := pick(obj); out != nil {
		return out
	}
	out := map[string]any{}
	for key, value := range obj {
		if nested := pick(value); nested != nil {
			out[key] = nested
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func toGeneric(v any) (any, error) {
	if raw, ok := v.(json.RawMessage); ok {
		return canonical.Decode(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canonical.Decode(b)
}

// refusalsOnly: the endpoint records only its failures (manifest.RecordRefusals; contracts/trails.md,
// "The run-operation volume exception"). The kit records a failure when the handler returns it;
// a handler on such an endpoint must return its errors, never write them itself (c.JSON with an
// error status, httpapi.WriteError or WriteOpenAIError), or the refusal is never recorded.
func (c *Call) refusalsOnly() bool {
	t := c.trail()
	return t != nil && t.Record == manifest.RecordRefusals
}

// HandOffEvent is this call's success event, built as the kit would record it, for a handler
// whose change commits in another service's transaction (Workflows: the engine commits CancelRun
// with its run and inserts the event with trails.Insert). It records nothing itself; once the
// other side has committed, call Recorded so the kit doesn't record the call again. It's nil, with
// no error, for a call the kit wouldn't record (a valid internal call), and an error before
// org-api has vouched for the caller or on an endpoint that records no successes.
func (c *Call) HandOffEvent(status int, body any) (*trails.Event, error) {
	if c.trail() == nil || c.refusalsOnly() || !c.Attributable() {
		return nil, errors.New("service: HandOffEvent needs an endpoint that records successes and a caller org-api has vouched for")
	}
	if c.internalCall {
		return nil, nil
	}
	c.addResourcesOf(body)
	return c.event(status, body, nil), nil
}

// Recorded marks the call's success event as written elsewhere (HandOffEvent): the kit won't
// record the call, nor a failure after it, including in Transact or TransactPrepared (an
// Idempotency-Key store after the hand-off). Hand the event off in TransactPrepared's prepare,
// which a replay never runs, so a replay is recorded by the kit as its own call.
func (c *Call) Recorded() { c.handedOff = true }

// record writes the success event in tx, the transaction of the change.
func (c *Call) record(tx *db.Tx, status int, body any) error {
	if c.unauthorizedDelegated(status) {
		// Roll the change back: a delegated call is the customer's only once org-api said so.
		return errDelegatedUnauthorized
	}
	if c.handedOff || c.recorded || c.internalCall || c.trail() == nil || !c.Attributable() || c.refusalsOnly() {
		return nil
	}
	c.addResourcesOf(body)
	c.recorded = true
	return trails.Insert(c.Ctx, tx, c.event(status, body, nil))
}

// recordReplay records a replay in tx: the call, naming what the stored response's call named
// besides what this one did (a replayed DELETE names the resource the first one deleted).
func (c *Call) recordReplay(tx *db.Tx, resp *Response) error {
	for _, r := range resp.resources {
		if r.ResourceName == "" || slices.ContainsFunc(c.resources, func(have trails.Resource) bool { return have.ResourceName == r.ResourceName }) {
			continue
		}
		if r.Type == "" {
			r.Type = c.typeOf(r.ResourceName)
		}
		c.resources = append(c.resources, r)
	}
	return c.record(tx, resp.Status, resp.Body)
}

// addResourcesOf adds the resources a successful response names, at its top level or one object
// down (an operation's target), as the TypeScript kit does: a replayed create touched its widget
// as much as the first did.
func (c *Call) addResourcesOf(body any) {
	b, err := json.Marshal(body)
	if err != nil {
		return
	}
	var top map[string]any
	if json.Unmarshal(b, &top) != nil {
		return
	}
	take := func(v any) {
		if m, ok := v.(map[string]any); ok {
			if name, ok := m["resourceName"].(string); ok && name != "" {
				c.AddResource(name)
			}
		}
	}
	take(top)
	for _, key := range slices.Sorted(maps.Keys(top)) {
		take(top[key])
	}
}

// fail answers err and, when the call is attributable, records it in a transaction of its own
// (the change's transaction has rolled back).
func (c *Call) fail(err error) {
	e := httpapi.ErrorOf(err)
	if c.private {
		c.failPrivate(e)
		return
	}
	if e.Status >= 500 {
		c.Service.Logger.ErrorContext(c.Ctx, "request failed", "route", c.Endpoint.Path, "code", e.Code, "error", fmt.Sprint(e.Cause))
	}
	if !c.responded {
		c.responded = true
		c.writeError(e)
	}
	if c.handedOff || c.internalCall || c.trail() == nil || !c.Attributable() || c.recorded || c.delegationRefused {
		return
	}
	c.recorded = true
	ctx := context.WithoutCancel(c.Ctx)
	if err := trails.InsertAlone(ctx, c.Service.DB, c.event(e.Status, nil, e)); err != nil {
		c.Service.Logger.ErrorContext(c.Ctx, "recording a failed call in Trails", "error", err.Error())
	}
}

// writeError answers e in the endpoint's error model: the platform's body, the Prometheus HTTP
// API's (errorModel prometheus) or the OpenAI API's (errorModel openai).
func (c *Call) writeError(e *httpapi.Error) {
	switch c.Endpoint.ErrorModel {
	case manifest.ErrorModelPrometheus:
		httpapi.WritePrometheusError(c.W, e)
	case manifest.ErrorModelOpenAI:
		httpapi.WriteOpenAIError(c.W, e)
	default:
		httpapi.WriteError(c.W, e)
	}
}

// failPrivate persists attributable failures before sending their response. Audit failures
// fail closed, with bounded detached cleanup even when the request context was canceled.
func (c *Call) failPrivate(e *httpapi.Error) {
	if e.Status >= 500 {
		c.Service.Logger.ErrorContext(c.Ctx, "request failed", "route", c.Endpoint.Path, "code", e.Code, "message", e.Message)
	}
	if !c.handedOff && !c.internalCall && c.trail() != nil && c.Attributable() && !c.recorded && !c.delegationRefused {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Ctx), 5*time.Second)
		defer cancel()
		if err := trails.InsertAlone(ctx, c.Service.DB, c.event(e.Status, nil, e)); err != nil {
			c.Service.Logger.ErrorContext(c.Ctx, "recording a failed call in Trails")
			e = httpapi.Unavailable("The audit record could not be persisted; try again shortly", nil)
		} else {
			c.recorded = true
		}
	}
	if !c.responded {
		c.responded = true
		c.writeError(e)
	}
}

// finish records a successful read (writes recorded theirs in their transaction).
func (c *Call) finish() {
	if c.handedOff || c.internalCall || c.trail() == nil || c.recorded || c.Endpoint.Auth == "none" || c.Endpoint.Auth == "service" || c.refusalsOnly() {
		return
	}
	if !c.Attributable() {
		c.Service.Logger.ErrorContext(c.Ctx, "a public call succeeded without being authorized; it isn't recorded", "route", c.Endpoint.Path)
		return
	}
	if c.R.Method != http.MethodGet && !c.Endpoint.ReadOnly() {
		c.Service.Logger.ErrorContext(c.Ctx, "a write responded without Transact, so its Trails event isn't in its transaction", "route", c.Endpoint.Path)
	}
	c.recorded = true
	ctx := context.WithoutCancel(c.Ctx)
	if err := trails.InsertAlone(ctx, c.Service.DB, c.event(c.lastStatus, c.lastBody, nil)); err != nil {
		if c.private {
			c.Service.Logger.ErrorContext(c.Ctx, "recording a call in Trails")
		} else {
			c.Service.Logger.ErrorContext(c.Ctx, "recording a call in Trails", "error", err.Error())
		}
	}
}

// PurgeBatch is how many idempotency keys one of PurgeIdempotencyKeys' statements deletes.
const PurgeBatch = 1000

// PurgeIdempotencyKeys deletes idempotency records older than age, PurgeBatch at a time, and
// answers how many. The contract keeps them at least 24 hours; a retry after that is a new
// request.
func PurgeIdempotencyKeys(ctx context.Context, q db.Querier, age time.Duration) (int64, error) {
	if age < 24*time.Hour {
		return 0, fmt.Errorf("idempotency keys are kept at least 24 hours, not %s", age)
	}
	// In batches of PurgeBatch, oldest first, until one is short: one statement over a busy
	// product's whole day of keys would hold its locks and write its WAL in one go.
	var n int64
	for {
		batch, err := db.RowsAffected(q.ExecContext(ctx, `DELETE FROM idempotency_keys
			WHERE (organization_id, key) IN (
				SELECT organization_id, key FROM idempotency_keys
				 WHERE created_at < now() - make_interval(secs => $1)
				 ORDER BY created_at LIMIT $2)`, age.Seconds(), PurgeBatch))
		if err != nil {
			return 0, err
		}
		n += batch
		if batch < PurgeBatch {
			break
		}
	}
	// Lapsed claims too, when the role can reach them (a role not granted the table skips them).
	if ok, err := claimsReachable(ctx, q); err != nil || !ok {
		return n, err
	}
	if _, err := q.ExecContext(ctx, `DELETE FROM idempotency_claims WHERE expires_at < now()`); err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteIdempotencyKeys deletes every idempotency record of an organization, in tx, the
// transaction that purges the rest of a deleted organization's rows.
func DeleteIdempotencyKeys(ctx context.Context, tx *db.Tx, organizationID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE organization_id = $1`, organizationID); err != nil {
		return err
	}
	// Its claims too, when the role can reach them (a role not granted the table skips them; they
	// lapse within minutes and a purge removes them).
	if ok, err := claimsReachable(ctx, tx); err != nil || !ok {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM idempotency_claims WHERE organization_id = $1`, organizationID)
	return err
}

// IdempotencyKeyExists reports whether the organization has a stored record for an Idempotency-Key,
// that is, whether a request with that key already completed (a replay or a 422 mismatch).
func IdempotencyKeyExists(ctx context.Context, q db.Querier, organizationID, key string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM idempotency_keys WHERE organization_id=$1 AND key=$2)`, organizationID, key).Scan(&exists)
	return exists, err
}
