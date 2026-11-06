// Package authz asks org-api who a caller is and what they may do
// (contracts/identity-and-authz.md). A product never validates a credential itself and never
// decides a permission itself: on every request it forwards the caller's credential to org-api's
// permissions endpoint and believes the answer. Nothing is cached, anywhere: a revoked key, a
// removed member or a detached policy takes effect on the very next request.
package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resourceurl"
)

// Credential is the one customer credential a request carries (a session cookie, an API key, an
// OAuth access token or an EZGH SSO resource token), forwarded to org-api alone, with the request's X-Forwarded-For.
type Credential struct {
	// Header is "Authorization" or "Cookie"; Value is what's forwarded.
	Header, Value string
	// ForwardedFor is the request's X-Forwarded-For, unchanged ("" without one). org-api passes it
	// to auth-api, whose rate limit then counts the end client (its rightmost entry), never this
	// service (contracts/api-conventions.md, "Client IP").
	ForwardedFor string
	// RequestTarget is the request's raw request-target (r.RequestURI, as received, before the
	// router decodes it), kept only with an EZGH SSO resource token: Check builds requestUrl from
	// it and the Client's PublicBaseURL. Empty for every other credential.
	RequestTarget string
	// PublicBase, when set, replaces the Client's PublicBaseURL for this request: a product served
	// on a per-directory host (MCP Central's https://<alias>.mcp.ezgh.app/central…) sets it to
	// https://<the alias it resolved for this request>.<its domain> with WithPublicBase. Never set
	// it from Host or X-Forwarded-* without resolving the alias (contracts/api-conventions.md,
	// "Canonical resource URLs").
	PublicBase string
	// RequestURL, when set, is an EZGH SSO resource token's requestUrl as is, for a token bound to
	// a fixed audience label rather than the URL it's presented at (MCP Central's portal:
	// https://mcpcentral.ezghcloud.com/v1/organizations/<orgId>/portal). It must be canonical.
	RequestURL string
	// FromProductCookie is set when the token came from a product's own edge cookie
	// (ProductCookie), not an Authorization header: a browser attaches it, so cookie writes need
	// the Origin check. It's forwarded as Authorization: Bearer like any token.
	FromProductCookie bool
}

// WithRequestURL is the credential with a fixed requestUrl.
func (c Credential) WithRequestURL(u string) Credential {
	c.RequestURL = u
	return c
}

// WithPublicBase is the credential with the request's public origin set, for a product served on
// a per-directory host.
func (c Credential) WithPublicBase(origin string) Credential {
	c.PublicBase = origin
	return c
}

// requestURL is an EZGH SSO resource token's requestUrl: its PublicBase, else the Client's
// PublicBaseURL, plus its raw request-target. ok is false for a path with no canonical form; err
// is set for a base that isn't an https origin (a bug or a misconfiguration).
func (c *Client) requestURL(cred Credential) (string, bool, error) {
	if cred.RequestURL != "" {
		if canonical, ok := resourceurl.Canonical(cred.RequestURL); !ok || canonical != cred.RequestURL {
			return "", false, fmt.Errorf("the fixed requestUrl %q isn't a canonical resource URL", cred.RequestURL)
		}
		return cred.RequestURL, true, nil
	}
	base := c.PublicBaseURL
	if cred.PublicBase != "" {
		base = cred.PublicBase
	}
	if _, ok := resourceurl.Origin(base); !ok {
		return "", false, fmt.Errorf("the public base URL %q isn't an https origin; an ezgh_sat_ token needs it for requestUrl", base)
	}
	u, ok := resourceurl.RequestURL(base, cred.RequestTarget)
	return u, ok, nil
}

// IsBearer reports whether the caller sent an Authorization header (an API key, an OAuth access
// token or an EZGH SSO resource token), which a browser never attaches on its own, so it skips the Origin check.
// A token from a product's edge cookie is forwarded the same way but isn't one: a browser attaches it.
func (c Credential) IsBearer() bool { return c.authorization() && !c.FromProductCookie }

// authorization reports whether the credential is forwarded as an Authorization header.
func (c Credential) authorization() bool { return c.Header == "Authorization" }

// IsAPIKey reports whether the caller used an API key (so a refusal says "This API key isn't
// allowed to …"). An OAuth access token acts as its user, so its refusals say "You're not
// allowed to …", as for their session.
func (c Credential) IsAPIKey() bool {
	return c.authorization() && apiKeyHeader.MatchString(c.Value)
}

// IsOAuth reports whether the caller used a person's OAuth access token.
func (c Credential) IsOAuth() bool {
	return c.authorization() && oauthAccessTokenHeader.MatchString(c.Value)
}

// IsSSO reports whether the caller used an EZGH SSO resource token (`Bearer ezgh_sat_…`), the
// one credential Check sends requestUrl with.
func (c Credential) IsSSO() bool {
	return c.authorization() && ssoResourceTokenHeader.MatchString(c.Value)
}

// Customer credentials in the Authorization header, by exact match (contracts/identity-and-authz.md
// 1.5, "Telling the credentials apart"). The prefixes can't be confused: an API key's 40 characters
// after `ezgh_` are all base62, so it never has a second `_`.
var (
	apiKeyHeader           = regexp.MustCompile(`^Bearer ezgh_[A-Za-z0-9]{40}$`)
	oauthAccessTokenHeader = regexp.MustCompile(`^Bearer ezgh_at_[A-Za-z0-9]{43}$`)
	ssoResourceTokenHeader = regexp.MustCompile(`^Bearer ezgh_sat_[A-Za-z0-9]{43}$`)
	// [being built] A project ingest key, which only an ingest host accepts ("Project ingest keys").
	ingestKeyHeader = regexp.MustCompile(`^Bearer ezgh_ik_[A-Za-z0-9]{43}$`)
	// [being built] A product data-plane key, which only its product's data host accepts ("Product
	// data-plane keys"): Feature Flags' server (ezgh_fs_) and client (ezgh_fc_) SDK keys. No kit
	// parser accepts one; the product's data server resolves it itself.
	productDataKeyHeader = regexp.MustCompile(`^Bearer ezgh_f[sc]_[A-Za-z0-9]{43}$`)
)

// The kinds CustomerCredentialKind and Classify tell apart.
const (
	KindAPIKey   = "apiKey"
	KindOAuth    = "oauth"
	KindSSOOAuth = "ssoOauth"
	// KindIngestKey is a project ingest key (`Bearer ezgh_ik_` + 43 base62). Only Classify returns
	// it: product routes refuse it, so CustomerCredentialKind says "".
	KindIngestKey = "ingestKey"
)

// CustomerCredentialKind classifies an Authorization header for a product route: KindAPIKey
// (`Bearer ezgh_` + 40 base62), KindOAuth (`Bearer ezgh_at_` + 43 base62), KindSSOOAuth
// (`Bearer ezgh_sat_` + 43 base62), or "" for anything else (a refresh token `ezgh_rt_…`, a
// project ingest key `ezgh_ik_…`, a service credential, another scheme, a malformed key), which
// is refused.
func CustomerCredentialKind(header string) string {
	switch {
	case apiKeyHeader.MatchString(header):
		return KindAPIKey
	case oauthAccessTokenHeader.MatchString(header):
		return KindOAuth
	case ssoResourceTokenHeader.MatchString(header):
		return KindSSOOAuth
	}
	return ""
}

// Classify is CustomerCredentialKind plus the one credential only an ingest host takes:
// KindIngestKey for `Bearer ezgh_ik_` + 43 base62, the product kinds as CustomerCredentialKind
// says, and "" for anything else.
func Classify(header string) string {
	if ingestKeyHeader.MatchString(header) {
		return KindIngestKey
	}
	return CustomerCredentialKind(header)
}

// IngestKeyFrom is an ingest host's credential check (contracts/identity-and-authz.md, "Project
// ingest keys"): values are the request's authorization values, the HTTP header's or gRPC
// metadata's "authorization", and must be exactly one project ingest key. Nothing else is
// accepted: no cookie, API key or access token. It returns the key without "Bearer ", for
// org-api's resolve; anything else is 401 without asking anyone.
func IngestKeyFrom(values []string) (string, *httpapi.Error) {
	if len(values) != 1 || Classify(values[0]) != KindIngestKey {
		return "", httpapi.Unauthenticated("Send a project ingest key: authorization: Bearer ezgh_ik_…")
	}
	return strings.TrimPrefix(values[0], "Bearer "), nil
}

// IngestCredentialFrom is an ingest host's credential check once it takes API keys too
// (contracts/identity-and-authz.md, "Accepted on an ingest host"): values must be exactly one
// project ingest key or API key. It returns the credential without "Bearer " and whether it's an
// API key, which goes to org-api's ingest-credentials/resolve and is never cached; anything else
// is 401 without asking anyone.
func IngestCredentialFrom(values []string) (token string, isAPIKey bool, e *httpapi.Error) {
	if len(values) == 1 {
		switch kind := Classify(values[0]); kind {
		case KindIngestKey, KindAPIKey:
			return strings.TrimPrefix(values[0], "Bearer "), kind == KindAPIKey, nil
		}
	}
	return "", false, httpapi.Unauthenticated("Send a project ingest key or an API key: authorization: Bearer ezgh_…")
}

// CredentialOption narrows what CredentialFrom accepts on one route.
type CredentialOption func(*credentialOptions)

type credentialOptions struct {
	bearerOnly bool
	cookie     string
}

// BearerOnly accepts only an Authorization header (an API key, an OAuth access token or an EZGH SSO
// resource token): cookies are never read, so a request with only cookies is 401. For endpoints
// that aren't a browser API (an MCP endpoint), and for every endpoint on an edge host, which the
// platform's session cookie never reaches (contracts/identity-and-authz.md, "Bearer-only
// endpoints").
func BearerOnly() CredentialOption { return func(o *credentialOptions) { o.bearerOnly = true } }

// ProductCookie takes the caller's credential only from the product's own edge cookie, name (a
// __Host- cookie on the edge host, MCP Central's __Host-mcp_portal), holding an EZGH SSO resource
// token (contracts/identity-and-authz.md, "Product cookies"): every other cookie and the
// Authorization header are ignored. No such cookie, or one that isn't an ezgh_sat_ token, is 401.
// The token is forwarded as Authorization: Bearer, marked FromProductCookie for the Origin check.
func ProductCookie(name string) CredentialOption {
	return func(o *credentialOptions) { o.cookie = name }
}

var ssoResourceToken = regexp.MustCompile(`^ezgh_sat_[A-Za-z0-9]{43}$`)

// CredentialFrom picks the caller's credential: an API key (Authorization: Bearer ezgh_…) or a
// person's OAuth access token (Authorization: Bearer ezgh_at_…) wins, forwarded exactly as it
// came, as does an EZGH SSO resource token (Bearer ezgh_sat_…); the cookie is ignored. Otherwise
// better-auth's session cookies, and only those.
// Every other cookie is dropped before calling org-api, as org-api drops them before calling
// auth-api. No credential at all, or an Authorization header that's no customer credential (a
// refresh token, a service credential on a public route, say), is 401 without asking anyone and
// never falls back to the cookie. The request's X-Forwarded-For goes along with either, and with an
// EZGH SSO resource token, its raw request-target (never Host or X-Forwarded-*), for requestUrl.
// Options narrow it for one route (BearerOnly).
func CredentialFrom(r *http.Request, opts ...CredentialOption) (Credential, *httpapi.Error) {
	var o credentialOptions
	for _, opt := range opts {
		opt(&o)
	}
	xff := forwardedFor(r)
	if o.cookie != "" {
		var values []string
		for _, c := range r.Cookies() {
			if c.Name == o.cookie {
				values = append(values, c.Value)
			}
		}
		if len(values) != 1 || !ssoResourceToken.MatchString(values[0]) {
			return Credential{}, httpapi.Unauthenticated("Sign in")
		}
		return Credential{Header: "Authorization", Value: "Bearer " + values[0], ForwardedFor: xff, RequestTarget: r.RequestURI, FromProductCookie: true}, nil
	}
	if values := r.Header.Values("Authorization"); len(values) > 0 {
		if len(values) == 1 && Classify(values[0]) == KindIngestKey {
			// [being built] No product route takes a project ingest key, and none falls back to
			// the cookie for one.
			return Credential{}, httpapi.Unauthenticated("A project ingest key only sends telemetry to an ingest host; here, send an API key or access token (Authorization: Bearer ezgh_…), or sign in")
		}
		if len(values) == 1 && productDataKeyHeader.MatchString(values[0]) {
			// [being built] A product data-plane key (an SDK key) works only on its product's data
			// host; no product route takes one, and none falls back to the cookie for one.
			return Credential{}, httpapi.Unauthenticated("An SDK key is a data-plane key for its product's data host (such as flags.ezghcloud.com); here, send an API key or access token (Authorization: Bearer ezgh_…), or sign in")
		}
		if len(values) != 1 || CustomerCredentialKind(values[0]) == "" {
			return Credential{}, httpapi.Unauthenticated("Send a valid API key or access token (Authorization: Bearer ezgh_…), or sign in")
		}
		return bearerCredential(r, values[0]), nil
	}
	if o.bearerOnly {
		return Credential{}, httpapi.Unauthenticated("Send an API key or access token: Authorization: Bearer ezgh_…")
	}
	var kept []string
	for _, c := range r.Cookies() {
		if strings.HasPrefix(c.Name, "better-auth.") || strings.HasPrefix(c.Name, "__Secure-better-auth.") {
			kept = append(kept, c.Name+"="+c.Value)
		}
	}
	if len(kept) == 0 {
		return Credential{}, httpapi.Unauthenticated("Sign in, or send an API key or access token (Authorization: Bearer ezgh_…)")
	}
	return Credential{Header: "Cookie", Value: strings.Join(kept, "; "), ForwardedFor: xff}, nil
}

// bearerCredential is the credential an accepted Authorization header (a customer credential,
// "Bearer …") forwards: the header as it came, the request's X-Forwarded-For and, with an EZGH SSO
// resource token, its raw request-target. CredentialFrom and RegistryCredentialFrom both build it.
func bearerCredential(r *http.Request, header string) Credential {
	cred := Credential{Header: "Authorization", Value: header, ForwardedFor: forwardedFor(r)}
	if cred.IsSSO() {
		cred.RequestTarget = r.RequestURI
	}
	return cred
}

// forwardedFor is the request's X-Forwarded-For, unchanged ("" without one).
func forwardedFor(r *http.Request) string {
	v := strings.Join(r.Header.Values("X-Forwarded-For"), ", ")
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return v
}

// Client calls org-api.
type Client struct {
	// BaseURL is org-api, in-cluster: http://org-api.login-stack.svc.cluster.local:4100.
	BaseURL string
	// InternalToken authenticates the directory lookup (X-Internal-Token today; a dk_ credential
	// is proposed).
	InternalToken string
	// PublicBaseURL is the product's public base URL, an https origin (config's PUBLIC_BASE_URL,
	// https://<namespace>.ezghcloud.com in production). With an EZGH SSO resource token, Check
	// sends org-api requestUrl: this origin (or the credential's PublicBase) plus the request's raw
	// path (contracts/identity-and-authz.md, "Resource binding").
	PublicBaseURL string
	// ServiceCredential is the product's own org-api credential, ok_<16>.<secret> (config's
	// ORG_API_SERVICE_CREDENTIAL), bound to its namespace. [being built] CheckBatch sends it; the
	// namespace's registration must declare permissions.batch. Empty: CheckBatch is unavailable.
	ServiceCredential string
	// Logger, when set, gets a warning for each failed call of a CheckBatch whose answer stands
	// (its checks fail closed); nil logs nothing. Whole failures are the returned error's Cause.
	Logger *slog.Logger
	// HTTP makes the calls; nil is a client with a 5-second timeout.
	HTTP *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// Organization is the permissions answer's organization. State is proposed (absent today).
type Organization struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Slug         string  `json:"slug"`
	ResourceName string  `json:"resourceName"`
	IsRoot       bool    `json:"isRoot"`
	State        *string `json:"state"`
}

// Principal is who asked: a user, a bot (also when a directory user acts as one in a federated
// console session), or a directory user (`ssoUser`, through an `ezgh_sat_` token).
type Principal struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	// [being built] Slug is the principal's prefixed slug when it has one (a bot's bot_…), as
	// org-api's answers name it, for a product to store and show; "" for a user, an SSO user, or an
	// older org-api that leaves it out. Compare principals with Same, never ==.
	Slug string `json:"slug,omitempty"`
}

// Same reports whether p and q are one principal (type and ID; the slug is only a name).
func (p Principal) Same(q Principal) bool { return p.Type == q.Type && p.ID == q.ID }

// Permissions is org-api's 200 answer, read tolerantly: org-api may add fields. APIKeyID is nil
// for a session or an OAuth access token; UserIdentity's sessionContext.credential says which
// (`session`, `apiKey`, `oauth` with `clientId`, `ssoOauth`, or `federation` with
// `sourceIdentity`: the directory user acting as the bot). A temporary API key is `apiKey` with
// `directoryId` and `sourceIdentity`: the directory user who made it, acting as the bot. An OIDC
// API key [being built] is `apiKey` with a `sourceIdentity` of type `OidcSubject` (the provider and
// the token's sub) and no `directoryId`; an instance key's is `ComputeInstance`, and a Kubernetes
// key's [being built] `KubernetesPod` or `KubernetesCluster` (principalId the cluster's UUID).
type Permissions struct {
	Organization Organization `json:"organization"`
	Principal    Principal    `json:"principal"`
	APIKeyID     *string      `json:"apiKeyId"`
	// UserIdentity is recorded in Trails byte for byte, so it's kept raw.
	UserIdentity json.RawMessage `json:"userIdentity"`
	Resource     string          `json:"resource"`
	Allowed      []string        `json:"allowed"`
}

// KubernetesCluster is the cluster UUID of a Kubernetes cluster Bot's key [being built]: the
// sourceIdentity's principalId when its type is KubernetesCluster, else "". A product reads it only
// to admit the Kubernetes cluster tags (contracts/resources.md), never to authorize.
func (p *Permissions) KubernetesCluster() string {
	var u struct {
		SessionContext *struct {
			SourceIdentity *struct {
				Type        string `json:"type"`
				PrincipalID string `json:"principalId"`
			} `json:"sourceIdentity"`
		} `json:"sessionContext"`
	}
	if json.Unmarshal(p.UserIdentity, &u) != nil || u.SessionContext == nil || u.SessionContext.SourceIdentity == nil ||
		u.SessionContext.SourceIdentity.Type != "KubernetesCluster" {
		return ""
	}
	return u.SessionContext.SourceIdentity.PrincipalID
}

// Allows reports whether action came back in allowed.
func (p *Permissions) Allows(action string) bool {
	for _, a := range p.Allowed {
		if a == action {
			return true
		}
	}
	return false
}

// Check asks org-api GET /v1/organizations/:orgId/permissions?actions=…&resource=… as the caller,
// forwarding only their credential and their X-Forwarded-For. resource "" means the
// organization. With an EZGH SSO resource token (and only then) it also sends requestUrl, the
// request's canonical URL (resourceurl.RequestURL of PublicBaseURL and the credential's
// RequestTarget); a request path with no canonical form is 401 without calling org-api, which
// answers 401 itself unless requestUrl is exactly the token's bound URL. org-api's answers map to
// the caller's error:
//
//	200            the answer (the caller is a member; whether actions are allowed is Allows')
//	401            401 unauthenticated
//	404            404 not_found, "Organization not found": no such organization, or the
//	               caller isn't in it, which must look the same
//	400            500 internal_error: an unregistered action or a malformed resource name, a bug
//	429            429 too_many_requests with org-api's Retry-After: the caller is over
//	               auth-api's rate limit
//	anything else  503 unavailable with Retry-After. Never fail open, never 500.
func (c *Client) Check(ctx context.Context, cred Credential, orgID string, actions []string, resource string) (*Permissions, *httpapi.Error) {
	q := url.Values{}
	if len(actions) > 0 {
		q.Set("actions", strings.Join(actions, ","))
	}
	if resource != "" {
		q.Set("resource", resource)
	}
	if cred.IsSSO() {
		requestURL, ok, err := c.requestURL(cred)
		if err != nil {
			// Config refuses to start without one: this is a Client built without it.
			return nil, httpapi.Internal(err)
		}
		if !ok {
			// A path with no canonical form can't be at any URL a token is bound to.
			return nil, httpapi.Unauthenticated("This access token isn't valid for this URL")
		}
		q.Set("requestUrl", requestURL)
	}
	u := c.BaseURL + "/v1/organizations/" + url.PathEscape(orgID) + "/permissions"
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, httpapi.Internal(err)
	}
	req.Header.Set(cred.Header, cred.Value)
	if cred.ForwardedFor != "" {
		req.Header.Set("X-Forwarded-For", cred.ForwardedFor)
	}
	req.Header.Set("Accept", "application/json")
	setRequestID(ctx, req)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, httpapi.Unavailable("Authorization is unavailable; try again shortly", fmt.Errorf("org-api permissions: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		var p Permissions
		if err := json.Unmarshal(body, &p); err != nil || p.Organization.ID == "" || len(p.UserIdentity) == 0 {
			return nil, httpapi.Unavailable("Authorization is unavailable; try again shortly", fmt.Errorf("org-api permissions: unexpected body: %s", truncate(body)))
		}
		return &p, nil
	case http.StatusUnauthorized:
		return nil, httpapi.Unauthenticated("Sign in, or send a valid API key or access token")
	case http.StatusNotFound:
		return nil, httpapi.NotFound("Organization")
	case http.StatusBadRequest:
		return nil, httpapi.Internal(fmt.Errorf("org-api refused the permission check for %s on %q (an unregistered action?): %s", strings.Join(actions, ","), resource, truncate(body)))
	case http.StatusTooManyRequests:
		return nil, httpapi.TooManyRequests("Too many requests; slow down and try again shortly", retryAfter(resp.Header))
	default:
		return nil, httpapi.Unavailable("Authorization is unavailable; try again shortly", fmt.Errorf("org-api permissions answered %d", resp.StatusCode))
	}
}

// retryAfter is how long an upstream's 429 asks us to wait: its Retry-After in whole seconds, else
// better-auth's X-Retry-After, else 1; always 1–3600.
// setRequestID sends the request's ID (httpapi.RequestIDFrom) on an org-api call, so org-api's
// lines and the customer's request share it; none when ctx carries none.
func setRequestID(ctx context.Context, req *http.Request) {
	if id := httpapi.RequestIDFrom(ctx); id != "" {
		req.Header.Set("X-Request-Id", id)
	}
}

func retryAfter(h http.Header) int {
	for _, name := range []string{"Retry-After", "X-Retry-After"} {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Get(name))); err == nil {
			return min(max(n, 1), 3600)
		}
	}
	return 1
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "…"
	}
	return string(b)
}

// Organization states in the directory.
const (
	StateActive          = "active"
	StateDeletionPending = "deletion_pending"
	StateDeleted         = "deleted"
)

// DirectoryOrganization is an organization the lookup knows.
type DirectoryOrganization struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	State string `json:"state"`
}

// DirectoryProject is a project the lookup knows; DeletedAt is its tombstone.
type DirectoryProject struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organizationId"`
	Slug           string     `json:"slug"`
	DeletedAt      *time.Time `json:"deletedAt"`
}

// Directory is one lookup's answer, by ID. Unknown IDs are simply absent. It's used for the
// request (or sweep) that asked, never kept.
type Directory struct {
	Organizations map[string]DirectoryOrganization
	Projects      map[string]DirectoryProject
}

// MaxLookupIDs is the most IDs of each kind one lookup takes.
const MaxLookupIDs = 1000

// Lookup asks org-api's POST /internal/v1/directory/lookup what these organizations and
// projects are (contracts/resources.md, "The directory lookup"). Any failure is an error; the
// caller answers 503.
func (c *Client) Lookup(ctx context.Context, organizationIDs, projectIDs []string) (*Directory, error) {
	return c.lookup(ctx, organizationIDs, projectIDs, nil, nil)
}

// LookupSlugs is Lookup by prefixed IDs (org_…, prj_…), in the request's organizationSlugs and
// projectSlugs: the answer, by UUID, is the same. Unknown prefixed IDs are absent.
func (c *Client) LookupSlugs(ctx context.Context, organizationSlugs, projectSlugs []string) (*Directory, error) {
	return c.lookup(ctx, nil, nil, nonNil(organizationSlugs), nonNil(projectSlugs))
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// lookup sends the slug fields only when they're not nil, so a lookup by IDs is the request
// org-api has always taken.
func (c *Client) lookup(ctx context.Context, organizationIDs, projectIDs, organizationSlugs, projectSlugs []string) (*Directory, error) {
	if len(organizationIDs) > MaxLookupIDs || len(projectIDs) > MaxLookupIDs || len(organizationSlugs) > MaxLookupIDs || len(projectSlugs) > MaxLookupIDs {
		return nil, fmt.Errorf("directory lookup: at most %d IDs of each kind", MaxLookupIDs)
	}
	request := map[string][]string{"organizationIds": nonNil(organizationIDs), "projectIds": nonNil(projectIDs)}
	if organizationSlugs != nil {
		request["organizationSlugs"], request["projectSlugs"] = organizationSlugs, nonNil(projectSlugs)
	}
	body, _ := json.Marshal(request)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/internal/v1/directory/lookup", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Internal-Token", c.InternalToken)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, fmt.Errorf("directory lookup: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("directory lookup answered %d: %s", resp.StatusCode, truncate(raw))
	}
	var answer struct {
		Organizations []DirectoryOrganization `json:"organizations"`
		Projects      []DirectoryProject      `json:"projects"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("directory lookup: unexpected body: %w", err)
	}
	if answer.Organizations == nil || answer.Projects == nil {
		return nil, fmt.Errorf("directory lookup: organizations and projects arrays are required")
	}
	d := &Directory{Organizations: map[string]DirectoryOrganization{}, Projects: map[string]DirectoryProject{}}
	for _, o := range answer.Organizations {
		// Until organization states exist, every organization the lookup knows is active.
		if o.State == "" {
			o.State = StateActive
		}
		if !ids.IsUUID(o.ID) || !ids.IsSlug(o.Slug) || !strings.HasPrefix(o.Slug, "org_") || (!slices.Contains(organizationIDs, o.ID) && !slices.Contains(organizationSlugs, o.Slug)) || (o.State != StateActive && o.State != StateDeletionPending && o.State != StateDeleted) {
			return nil, fmt.Errorf("directory lookup: invalid organization")
		}
		if _, exists := d.Organizations[o.ID]; exists {
			return nil, fmt.Errorf("directory lookup: duplicate organization")
		}
		d.Organizations[o.ID] = o
	}
	for _, p := range answer.Projects {
		if !ids.IsUUID(p.ID) || !ids.IsUUID(p.OrganizationID) || !ids.IsSlug(p.Slug) || !strings.HasPrefix(p.Slug, "prj_") || (!slices.Contains(projectIDs, p.ID) && !slices.Contains(projectSlugs, p.Slug)) {
			return nil, fmt.Errorf("directory lookup: invalid project")
		}
		if _, exists := d.Projects[p.ID]; exists {
			return nil, fmt.Errorf("directory lookup: duplicate project")
		}
		d.Projects[p.ID] = p
	}
	return d, nil
}

// Project is a project's entry when it's live and in organizationID; a project answered with
// another organization, or with a tombstone, is to the product unknown.
func (d *Directory) Project(organizationID, projectID string) (DirectoryProject, bool) {
	p, ok := d.Projects[projectID]
	if !ok || p.OrganizationID != organizationID || p.DeletedAt != nil {
		return DirectoryProject{}, false
	}
	return p, true
}
