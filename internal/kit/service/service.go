// Package service is the HTTP side of a product: routing from the manifest, the checks every
// public request goes through, Trails recording, idempotency keys, health and readiness, access
// logs, and graceful shutdown.
//
// Every route is declared in the manifest, and the manifest's endpoints are the Trails catalog:
// Handler refuses to build if a route has no manifest entry or an entry has no route, so nothing
// ships unlogged by accident (contracts/trails.md, "The catalog").
//
// A public request goes through, in order (contracts/identity-and-authz.md, "Checking a
// request"): the credential (401, not recorded), the Origin check for cookie writes (403, not
// recorded), malformed IDs in the path (404, not recorded), the body's size (413), and then the
// product's handler, which authorizes through org-api with Call.Authorize before anything else.
// From org-api's 200 on, the call is attributable and is recorded in Trails whatever happens:
// a success in the transaction of its change (Call.Transact), a failure after the rollback.
package service

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/credential"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/cursor"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/logging"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/openapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
)

// Handler serves one endpoint. It returns nil once it has responded (Call.JSON, Call.Transact),
// ErrReplayed as Call.Replay or Call.AuthorizeMissing returned it, or the error the caller should
// get.
type Handler func(c *Call) error

// Options configure a Service.
type Options struct {
	Manifest *manifest.Manifest
	DB       *db.DB
	OrgAPI   *authz.Client
	Cursors  *cursor.Signer
	// Origins is CORS_ORIGINS: the CORS allowlist and the Origin check for cookie writes.
	Origins httpapi.Origins
	Logger  *slog.Logger
	// Region is the region this deployment serves; Regions the regions its resource names may carry
	// (config.Platform's, by default only Region).
	Region  string
	Regions []string
	// InternalToken is the shared X-Internal-Token accepted on /internal routes today.
	InternalToken string
	// Credentials, when set, also accepts the service's own bearer credentials on /internal.
	Credentials *credential.Store
	// Checks are extra readiness checks. A failing one is reported as "degraded" but keeps /ready
	// at 200: what it checks (Redis) fails open.
	Checks map[string]func(context.Context) error
	// Quotas checks each endpoint's quotas with quotas-api once the caller is authorized
	// (contracts/quotas.md). Nil is the local fallback: allocation quotas at the manifest's
	// defaults, everything else unlimited.
	Quotas *quota.Enforcer
	// ReplayOperation renders an operation as it is now, for replaying an asynchronous request's
	// Idempotency-Key. Package operations sets it.
	ReplayOperation func(ctx context.Context, q db.Querier, organizationID, operationID string) (any, error)
	// OpenAPIOverlay is the product's openapi.overlay.json: what the manifest can't say about its
	// customer endpoints (package openapi). The service serves the document built from the two
	// at GET /openapi.json, and refuses to start when they differ.
	OpenAPIOverlay []byte
	// OrgResolver names the organization of an edge-host request from its host's alias, live
	// (contracts/registration.md, "Edge endpoints"). Required to serve edge-host endpoints.
	OrgResolver OrgResolver
	// EdgeDomain is the edge host's domain, https://<alias>.<EdgeDomain>: the manifest's
	// edge.domain unless set (locally, mcp.ezgh.localhost for mcp.ezgh.app, say).
	EdgeDomain string
}

// EdgeTenant is what an OrgResolver answers for an edge-host request.
type EdgeTenant struct {
	// OrganizationID is the organization the alias names: the Call's organization.
	OrganizationID string
	// Alias is the directory's current alias. When it isn't the alias the request came in on (a
	// retired alias still inside its redirect window), the kit answers 308 to the same path and
	// query on https://<Alias>.<edge domain> and runs nothing else.
	Alias string
}

// OrgResolver names the organization of an edge-host request whose host is
// https://<alias>.<edge domain>, reading it live on every request (MCP Central asks sso-api's
// GET /internal/v1/directory-aliases/:alias). It answers an *httpapi.Error to refuse: 404 for
// an alias no directory has, or for a retired alias the product won't redirect on this request
// (MCP Central's portal answers 404 to anything but GET); any other error is 503. The kit then
// sets the Call's organization and gives the caller's credential the public base
// https://<alias>.<edge domain> (authz.Credential.WithPublicBase), so requestUrl is the URL a
// token for this tenant is bound to. It runs before any credential is read, so an alias that
// doesn't resolve is answered before one is used.
type OrgResolver func(r *http.Request, alias string) (EdgeTenant, error)

// Service routes and checks a product's requests.
type Service struct {
	Options
	routes map[string]route
	// openAPI is the document served at GET /openapi.json, built by Handler.
	openAPI []byte
	// pathIDs are the …Id path parameters that also take a prefixed ID (PathIDs).
	pathIDs map[string]PathID
}

type route struct {
	private  bool
	stream   bool
	endpoint manifest.Endpoint
	handler  Handler
	params   []string
	// quotas is what one request consumes (nil: no call to Quotas).
	quotas *quota.Plan
}

// New makes a service; register routes with Handle, then build the handler with Handler.
func New(o Options) *Service {
	if o.Quotas == nil {
		o.Quotas = quota.NewEnforcer(quota.Options{Manifest: o.Manifest, Region: o.Region, Logger: o.Logger})
	}
	s := &Service{Options: o, routes: map[string]route{}}
	s.Handle("GET", "/health", s.health)
	s.Handle("GET", "/ready", s.ready)
	s.Handle("GET", openapi.Path, s.serveOpenAPI)
	return s
}

var paramPattern = regexp.MustCompile(`:([A-Za-z0-9]+)`)

// Handle registers the handler for a manifest endpoint, by its method and manifest path
// (/v1/organizations/:orgId/...).
func (s *Service) Handle(method, path string, h Handler) {
	var params []string
	for _, m := range paramPattern.FindAllStringSubmatch(path, -1) {
		params = append(params, m[1])
	}
	s.routes[method+" "+path] = route{handler: h, params: params}
}

// HandlePrivate registers a sensitive streaming endpoint. The handler must authorize before
// reading R.Body, impose its own size/time limits, and put only safe metadata in errors,
// SetRequestParameters, Additional and response identifiers. Body() is empty on these routes.
// Automatic request parameters are disabled; the manifest still supplies the Trails catalog.
func (s *Service) HandlePrivate(method, path string, h Handler) {
	s.Handle(method, path, h)
	key := method + " " + path
	r := s.routes[key]
	r.private = true
	s.routes[key] = r
}

// HandleStream registers a private upload with metadata-only idempotency. Authorize before
// reading R.Body, then hash/upload outside Transact, set the metadata fingerprint with
// SetIdempotencyInput, and persist only a safe metadata response through Transact.
func (s *Service) HandleStream(method, path string, h Handler) {
	s.HandlePrivate(method, path, h)
	key := method + " " + path
	r := s.routes[key]
	r.stream = true
	s.routes[key] = r
}

// Handler checks the routes against the manifest (the Trails catalog) and builds the HTTP
// handler. Any difference is an error: the service refuses to start.
//
// component names the Deployment this handler is for (manifest.ComponentAPI, the default and "",
// manifest.ComponentEdge or manifest.ComponentLive): it serves, and checks, only that component's
// endpoints, plus the kit's GET /health, /ready and /openapi.json, which every component serves.
// A handler registered for another component's endpoint is an error too, and another component's
// routes answer 404 here. Serving an edge-host endpoint needs Options.OrgResolver.
//
// The live component (contracts/registration.md, "Live endpoints") serves the live host's
// streams, <namespace>-live.ezghcloud.com, with the API host's rules: the platform's credentials
// (the session cookie, API keys and OAuth access tokens), the CORS allowlist and the Origin check
// on cookie writes, and each endpoint's timeoutSeconds. EZGH SSO resource tokens (ezgh_sat_) aren't
// accepted on live hosts yet: sso-edge binds them only to https://<namespace>.ezghcloud.com
// resources and org-api compares requestUrl with the resource exactly, so they fail closed (401). Its Deployment serves it with ServeWith,
// keeping the default shutdown drain when its streams resume.
func (s *Service) Handler(component ...string) (http.Handler, error) {
	name := manifest.ComponentAPI
	switch {
	case len(component) > 1:
		return nil, fmt.Errorf("service: Handler takes one component, not %v", component)
	case len(component) == 1 && component[0] != "":
		name = component[0]
	}
	if name != manifest.ComponentAPI && name != manifest.ComponentEdge && name != manifest.ComponentLive {
		return nil, fmt.Errorf("service: component %q isn't api, edge or live", name)
	}
	var problems []string
	declared := map[string]bool{}
	servesEdge := false
	for _, e := range s.Manifest.Endpoints {
		key := e.Method + " " + e.Path
		declared[key] = true
		if e.OnIngestHost() {
			// The product's ingest server serves it (manifest.CheckIngestRoutes), never this API host.
			if _, ok := s.routes[key]; ok {
				problems = append(problems, fmt.Sprintf("%s is an ingest-host endpoint: the ingest server serves it, not this API", key))
				delete(s.routes, key)
			}
			continue
		}
		if e.OnRegistryHost() {
			// The product's registry server serves it (manifest.CheckRegistryRoutes), never this API host.
			if _, ok := s.routes[key]; ok {
				problems = append(problems, fmt.Sprintf("%s is a registry-host endpoint: the registry server serves it, not this API", key))
				delete(s.routes, key)
			}
			continue
		}
		if e.OnGitHost() {
			// The product's Git server serves it (manifest.CheckGitRoutes), never this API host.
			if _, ok := s.routes[key]; ok {
				problems = append(problems, fmt.Sprintf("%s is a Git-host endpoint: the Git server serves it, not this API", key))
				delete(s.routes, key)
			}
			continue
		}
		if e.OnDataHost() {
			// The product's data server serves it (manifest.CheckDataRoutes), never this API host.
			if _, ok := s.routes[key]; ok {
				problems = append(problems, fmt.Sprintf("%s is a data-host endpoint: the data server serves it, not this API", key))
				delete(s.routes, key)
			}
			continue
		}
		if e.OnPKIHost() {
			// The product's PKI servers serve it (manifest.CheckPKIRoutes), never this API host.
			if _, ok := s.routes[key]; ok {
				problems = append(problems, fmt.Sprintf("%s is a PKI-host endpoint: the product's PKI server serves it, not this API", key))
				delete(s.routes, key)
			}
			continue
		}
		if !e.Builtin() && e.ComponentName() != name {
			if _, ok := s.routes[key]; ok {
				problems = append(problems, fmt.Sprintf("%s is component %s's, not %s's", key, e.ComponentName(), name))
				delete(s.routes, key)
			}
			continue
		}
		servesEdge = servesEdge || e.OnEdgeHost()
		r, ok := s.routes[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s is in the manifest but has no handler", key))
			continue
		}
		if r.private && !r.stream && e.IdempotencyKey {
			problems = append(problems, key+" is private and cannot persist idempotency request/response data")
		}
		r.endpoint = e
		r.quotas = s.Quotas.Plan(e.Method, e.Path)
		s.routes[key] = r
	}
	for key := range s.routes {
		if !declared[key] {
			problems = append(problems, fmt.Sprintf("%s has a handler but no manifest entry (every route needs a Trails catalog entry, or trail: null)", key))
		}
	}
	if servesEdge && s.OrgResolver == nil {
		problems = append(problems, "the "+name+" component serves edge-host endpoints, which need Options.OrgResolver")
	}
	if servesEdge && s.edgeDomain() == "" {
		problems = append(problems, "the "+name+" component serves edge-host endpoints, but the manifest has no edge.domain")
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("the routes and the manifest's endpoints differ:\n  %s", strings.Join(problems, "\n  "))
	}
	// The routes are the manifest's endpoints; the document must describe exactly its customer
	// endpoints.
	prefixes := map[string]string{}
	for param, d := range s.pathIDs {
		prefixes[param] = d.Prefix
	}
	doc, err := openapi.BuildWith(s.Manifest, s.OpenAPIOverlay, prefixes)
	if err != nil {
		return nil, err
	}
	s.openAPI = doc

	mux := http.NewServeMux()
	for key, r := range s.routes {
		method, path, _ := strings.Cut(key, " ")
		pattern := method + " " + paramPattern.ReplaceAllString(path, "{$1}")
		mux.Handle(pattern, s.serve(r))
	}
	// Under an OpenAI base, the router's own 404 (an unknown path, or a method the path doesn't
	// take) is OpenAI's body too, as the surface's every failure is.
	openAI := slices.ContainsFunc(s.Manifest.Endpoints, func(e manifest.Endpoint) bool { return e.ErrorModel == manifest.ErrorModelOpenAI })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logging.SetRoute(r.Context(), "unmatched")
		w.Header().Set("X-Request-Id", httpapi.RequestID(r))
		if openAI && openAIBase.MatchString(r.URL.Path) {
			httpapi.WriteOpenAIError(w, httpapi.NotFound("Route"))
			return
		}
		httpapi.WriteError(w, httpapi.NotFound("Route"))
	})
	// X-Request-Id outermost, so preflights and the router's own 404s and 405s carry it too.
	platform := httpapi.CORSExcept(s.Origins, noCORS, mux)
	anyOrigin := httpapi.CORSAnyOrigin(mux)
	// nosniff and HSTS on every response too (contracts/api-conventions.md, "Headers").
	return httpapi.WithSecurityHeaders(httpapi.WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if anyOriginCORS(r) {
			anyOrigin.ServeHTTP(w, r)
			return
		}
		platform.ServeHTTP(w, r)
	}))), nil
}

// anyOriginCORS: receipt-handle endpoints read no ambient credential (the handles in the body are
// the capability), so they answer CORS for any origin, without credentials: a console page polling
// a queue can acknowledge what it received.
func anyOriginCORS(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, manifest.ReceiptPathPrefix)
}

// openAIBase is a request path under a project's OpenAI-compatible base (manifest.ErrorModelOpenAI).
var openAIBase = regexp.MustCompile(`^/v1/organizations/[^/]+/projects/[^/]+/openai/v1/`)

// noCORS: invocation-token endpoints and public callbacks are called by servers, never by a
// browser page, so they answer no CORS at all, preflights included.
func noCORS(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, manifest.InvocationPathPrefix) || strings.HasPrefix(r.URL.Path, manifest.CallbackPathPrefix)
}

// statusWriter remembers the status for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

// Unwrap lets ResponseController reach transport controls such as read deadlines.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (s *Service) serve(r route) http.Handler {
	e := r.endpoint
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		logging.SetRoute(req.Context(), logging.RouteOf(req.Pattern))
		started := time.Now()
		if e.TimeoutSeconds > 0 {
			// A long endpoint (MCP Central's tool calls): its own read and write deadlines in place
			// of the server's, and a request context that ends with them.
			deadline := started.Add(time.Duration(e.TimeoutSeconds) * time.Second)
			rc := http.NewResponseController(w)
			if err := errors.Join(rc.SetReadDeadline(deadline), rc.SetWriteDeadline(deadline)); err != nil {
				s.Logger.Warn("can't extend the request's deadlines to the endpoint's timeoutSeconds", "route", e.Path, "error", err.Error())
			}
			ctx, cancel := context.WithDeadline(req.Context(), deadline)
			defer cancel()
			req = req.WithContext(ctx)
		}
		sw := &statusWriter{ResponseWriter: w}
		c := &Call{
			Service:   s,
			Endpoint:  e,
			R:         req,
			W:         sw,
			Ctx:       req.Context(),
			RequestID: httpapi.RequestID(req),
			Received:  started.UTC(),
			params:    r.params,
			private:   r.private,
			stream:    r.stream,
			// X-Internal-Token never marks a delegated call: it's the customer's, recorded.
			internalCall: !e.Delegated() && s.validInternalToken(req),
			quotaPlan:    r.quotas,
		}
		if c.internalCall {
			logging.SetCaller(req.Context(), logging.Caller{Type: "internal"})
		}
		sw.Header().Set("X-Request-Id", c.RequestID)
		defer func() {
			// Leases are given back after the response, whatever it was.
			if c.grant != nil {
				c.grant.Release()
			}
		}()
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				if c.private {
					s.Logger.ErrorContext(req.Context(), "panic", "route", e.Path)
					c.fail(httpapi.Internal(nil))
				} else {
					s.Logger.ErrorContext(req.Context(), "panic", "route", e.Path, "panic", fmt.Sprint(p))
					c.fail(httpapi.Internal(fmt.Errorf("panic: %v", p)))
				}
			}
		}()

		var err error
		if e.OnEdgeHost() {
			// The host's alias names the organization, before any credential is read.
			redirected, resolveErr := c.resolveEdge()
			if redirected {
				return
			}
			err = resolveErr
		} else if !e.Builtin() && s.onEdgeHost(req) {
			// An API-host route never answers on an edge host.
			err = httpapi.NotFound("Route")
		}
		if err == nil {
			switch e.Auth {
			case "none":
				if e.Public == manifest.PublicCallback {
					err = c.readCallbackBody()
				}
				if err == nil {
					err = r.handler(c)
				}
			case "service":
				if err = s.checkService(c); err == nil {
					err = r.handler(c)
				}
			case manifest.AuthInvocationToken:
				if err = c.prepareInvocation(); err == nil {
					err = r.handler(c)
				}
			case manifest.AuthReceiptHandle:
				if err = c.prepareReceipt(); err == nil {
					err = r.handler(c)
				}
			case manifest.AuthDelegated:
				if err = c.prepareDelegated(); err == nil {
					err = r.handler(c)
				}
			default:
				if err = c.prepare(); err == nil {
					err = r.handler(c)
				}
			}
		}
		if errors.Is(err, ErrReplayed) && c.responded {
			// Replay answered and recorded the call.
			return
		}
		if err != nil {
			c.fail(err)
			return
		}
		c.finish()
	})
}

// edgeDomain is the edge host's domain, lowercased: Options.EdgeDomain, else the manifest's.
func (s *Service) edgeDomain() string {
	if s.EdgeDomain != "" {
		return strings.ToLower(s.EdgeDomain)
	}
	if s.Manifest.Edge != nil {
		return s.Manifest.Edge.Domain
	}
	return ""
}

// onEdgeHost reports whether a request came in on one of the edge host's names.
func (s *Service) onEdgeHost(r *http.Request) bool {
	_, ok := edgeAlias(r.Host, s.edgeDomain())
	return ok
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// edgeAlias is the alias of host, <alias>.<domain> (any port dropped, case ignored); false when
// host isn't one of the edge host's names.
func edgeAlias(host, domain string) (string, bool) {
	if domain == "" {
		return "", false
	}
	host = strings.ToLower(host)
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host, "]") {
		host = host[:i]
	}
	alias, found := strings.CutSuffix(host, "."+domain)
	if !found || !dnsLabel.MatchString(alias) {
		return "", false
	}
	return alias, true
}

// validInternalToken recognizes only the exact configured service marker. It suppresses
// duplicate customer audit events, never the caller's authentication or authorization.
func (s *Service) validInternalToken(r *http.Request) bool {
	token := r.Header.Get("X-Internal-Token")
	if token == "" || s.InternalToken == "" {
		return false
	}
	given, want := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(s.InternalToken))
	return subtle.ConstantTimeCompare(given[:], want[:]) == 1
}

// checkService authenticates an /internal call: the shared X-Internal-Token today, or one of
// this service's own bearer credentials. A customer credential is never accepted here.
func (s *Service) checkService(c *Call) error {
	if s.validInternalToken(c.R) {
		c.ServiceCaller = "internal-token"
		return nil
	}
	if len(c.R.Header.Values("Authorization")) > 1 {
		return httpapi.Unauthenticated("A service credential is required")
	}
	if s.Credentials != nil && strings.HasPrefix(c.R.Header.Get("Authorization"), "Bearer ") {
		caller, err := s.Credentials.Verify(c.Ctx, c.R.Header.Get("Authorization"))
		if err == nil {
			c.ServiceCaller = caller
			return nil
		}
		if !errors.Is(err, credential.ErrInvalid) {
			return httpapi.Unavailable("Credentials can't be checked right now", err)
		}
	}
	return httpapi.Unauthenticated("A service credential is required")
}

func (s *Service) health(c *Call) error {
	c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	return nil
}

// serveOpenAPI is GET /openapi.json: the customer API's OpenAPI 3.1 document, unauthenticated.
func (s *Service) serveOpenAPI(c *Call) error {
	c.responded = true
	c.W.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.W.Header().Set("Cache-Control", "public, max-age=300")
	c.W.WriteHeader(http.StatusOK)
	_, _ = c.W.Write(s.openAPI)
	return nil
}

func (s *Service) ready(c *Call) error {
	ctx, cancel := context.WithTimeout(c.Ctx, 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		s.Logger.Warn("not ready", "error", err.Error())
		return httpapi.Unavailable("The database isn't reachable", err)
	}
	body := map[string]any{"status": "ok"}
	names := make([]string, 0, len(s.Checks))
	for name := range s.Checks {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := s.Checks[name](ctx); err != nil {
			body[name] = "degraded"
		} else {
			body[name] = "ok"
		}
	}
	c.JSON(http.StatusOK, body)
	return nil
}

// QuotaUsage serves GET /internal/v1/quotas/usage?organizationId=: the organization's allocation
// counters, rows at 0 included, for quotas-api to show beside their values (contracts/quotas.md,
// "Allocation usage for display"). Register it for the manifest's endpoint:
//
//	s.Handle("GET", "/internal/v1/quotas/usage", s.QuotaUsage)
func (s *Service) QuotaUsage(c *Call) error {
	q := c.R.URL.Query()
	var issues []httpapi.Issue
	for key := range q {
		if key != "organizationId" {
			issues = append(issues, httpapi.Issue{Path: key, Message: "isn't a parameter of this endpoint"})
		}
	}
	org := q.Get("organizationId")
	if len(q["organizationId"]) != 1 || !ids.IsUUID(org) {
		issues = append(issues, httpapi.Issue{Path: "organizationId", Message: "must be an organization's ID"})
	}
	if len(issues) > 0 {
		sort.Slice(issues, func(i, j int) bool { return issues[i].Path < issues[j].Path })
		return httpapi.Invalid(issues)
	}
	usage, err := quota.UsageOf(c.Ctx, s.DB, org)
	if err != nil {
		return databaseError(err)
	}
	c.JSON(http.StatusOK, map[string]any{"usage": usage})
	return nil
}

// Serve runs handler on addr until ctx is done, then stops taking requests and lets the ones in
// flight finish (up to 20 seconds): ServeWith's defaults.
func Serve(ctx context.Context, addr string, handler http.Handler, logger *slog.Logger) error {
	return ServeWith(ctx, addr, handler, logger, ServeOptions{})
}

// ServeOptions tune ServeWith; zero values are Serve's defaults.
type ServeOptions struct {
	// ShutdownDrain is how long requests in flight may run on once ctx is done (config's
	// SHUTDOWN_DRAIN_SECONDS); 0 is 20 seconds. A product with long endpoints (timeoutSeconds) sets
	// it to their longest, and its pods' terminationGracePeriodSeconds above it.
	ShutdownDrain time.Duration
	// WriteTimeout is how long a request may take to answer, unless its endpoint declares
	// timeoutSeconds; 0 is 60 seconds. ReadTimeout is how long reading it may take; 0 is 30 seconds.
	WriteTimeout time.Duration
	ReadTimeout  time.Duration
}

// DefaultShutdownDrain is ServeWith's drain when ServeOptions doesn't set one.
const DefaultShutdownDrain = 20 * time.Second

// ServeWith is Serve with options: a longer drain for long requests, or other timeouts.
func ServeWith(ctx context.Context, addr string, handler http.Handler, logger *slog.Logger, o ServeOptions) error {
	if o.ShutdownDrain <= 0 {
		o.ShutdownDrain = DefaultShutdownDrain
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = 60 * time.Second
	}
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 30 * time.Second
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.AccessLog(logger, handler),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       o.ReadTimeout,
		WriteTimeout:      o.WriteTimeout,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- server.ListenAndServe() }()
	logger.Info("listening", "addr", addr)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down: draining requests", "drainSeconds", o.ShutdownDrain.Seconds())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), o.ShutdownDrain)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// pathLabel is how a malformed path ID is reported: orgId → "Organization", widgetId → "Widget".
func pathLabel(param string) string {
	if param == "orgId" {
		return "Organization"
	}
	name := strings.TrimSuffix(param, "Id")
	if name == "" {
		return "Resource"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// isIDParam reports whether a path parameter is an ID (so must be a UUID, or a prefixed ID
// resolved to one: resolvePathIDs).
func isIDParam(param string) bool { return strings.HasSuffix(param, "Id") }
