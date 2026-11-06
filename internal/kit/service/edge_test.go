package service_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// Edge-host endpoints and components (contracts/registration.md, "Edge endpoints"): MCP
// Central's gateways at https://<alias>.mcp.ezgh.app/central…, served by its api component, and
// its portal on the same host, served by its edge component.

const (
	edgeOrgID = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	gatewayIn = "/central"
)

func edgeManifest() *manifest.Manifest {
	m := testManifest()
	m.Edge = &manifest.EdgeHost{Domain: "mcp.ezgh.app", Port: 4701}
	m.Endpoints = append(m.Endpoints,
		// A read without a trail, so the tests that need no database can answer it.
		manifest.Endpoint{Method: "GET", Path: "/central", Host: manifest.HostEdge, Auth: "caller", Action: "widgets.gateways.connect", Trail: nil},
		manifest.Endpoint{Method: "POST", Path: "/central/:gatewaySlug", Host: manifest.HostEdge, Auth: "caller", Action: "widgets.gateways.connect", IdempotencyException: "computation", Trail: &manifest.Trail{EventName: "UseGateway"}},
		manifest.Endpoint{Method: "GET", Path: "/v1/me", Host: manifest.HostEdge, Component: manifest.ComponentEdge, Auth: "caller", Action: "widgets.gateways.connect", Trail: nil},
	)
	return m
}

// resolver is a fake directory-by-alias lookup: acme is current, acme-corp a retired alias of
// acme, broken a lookup that fails, anything else unknown.
type resolver struct {
	mu    sync.Mutex
	asked []string
}

func (r *resolver) resolve(req *http.Request, alias string) (service.EdgeTenant, error) {
	r.mu.Lock()
	r.asked = append(r.asked, alias)
	r.mu.Unlock()
	switch alias {
	case "acme":
		return service.EdgeTenant{OrganizationID: edgeOrgID, Alias: "acme"}, nil
	case "acme-corp":
		if req.Method != "GET" && req.Method != "POST" {
			return service.EdgeTenant{}, httpapi.NotFound("Organization")
		}
		return service.EdgeTenant{OrganizationID: edgeOrgID, Alias: "acme"}, nil
	case "broken":
		return service.EdgeTenant{}, errors.New("sso-api didn't answer")
	case "bogus":
		return service.EdgeTenant{OrganizationID: "not-a-uuid", Alias: "bogus"}, nil
	}
	return service.EdgeTenant{}, httpapi.NotFound("Organization")
}

type edgeSeen struct {
	organizationID, alias, publicBase string
}

// edgeService builds component's handler over an org-api fake, recording what each handler saw.
func edgeService(t *testing.T, component string, opts func(*service.Options)) (http.Handler, *fakeOrgAPI, *resolver, *[]edgeSeen) {
	t.Helper()
	org := &fakeOrgAPI{}
	orgServer := httptest.NewServer(org)
	t.Cleanup(orgServer.Close)
	r := &resolver{}
	m := edgeManifest()
	o := service.Options{
		Manifest: m, OrgAPI: &authz.Client{BaseURL: orgServer.URL, PublicBaseURL: "https://widgets.ezghcloud.com"},
		Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m),
		OrgResolver: r.resolve,
	}
	if opts != nil {
		opts(&o)
	}
	s := service.New(o)
	var seen []edgeSeen
	read := func(c *service.Call) error {
		if _, err := c.Check([]string{c.Endpoint.Action}, ""); err != nil {
			return err
		}
		seen = append(seen, edgeSeen{c.OrganizationID, c.Alias(), c.PublicBase()})
		c.JSON(200, map[string]any{"ok": true})
		return nil
	}
	switch component {
	case manifest.ComponentEdge:
		s.Handle("GET", "/v1/me", read)
	default:
		s.Handle("GET", "/v1/organizations/:orgId/things", read)
		s.Handle("POST", "/v1/organizations/:orgId/things", read)
		s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
		s.Handle("GET", "/central", read)
		s.Handle("POST", "/central/:gatewaySlug", read)
	}
	h, err := s.Handler(component)
	if err != nil {
		t.Fatal(err)
	}
	return h, org, r, &seen
}

func edgeDo(h http.Handler, method, host, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var bearer = map[string]string{"Authorization": "Bearer " + ssoToken}

// The organization comes from the host's alias, and the credential's public base from the alias
// too: org-api gets requestUrl https://acme.mcp.ezgh.app/central…, whatever Host or
// X-Forwarded-Host claim, and the organization the alias names in its path.
func TestEdgeHostNamesTheOrganization(t *testing.T) {
	h, org, r, seen := edgeService(t, manifest.ComponentAPI, nil)
	headers := map[string]string{"Authorization": "Bearer " + ssoToken, "X-Forwarded-Host": "globex.mcp.ezgh.app"}
	for _, c := range []struct{ method, host, target, requestURL string }{
		{"GET", "acme.mcp.ezgh.app", "/central", "https://acme.mcp.ezgh.app/central"},
		{"GET", "ACME.mcp.ezgh.app:443", "/central?x=1", "https://acme.mcp.ezgh.app/central"},
	} {
		rec := edgeDo(h, c.method, c.host, c.target, headers)
		if rec.Code != 200 {
			t.Fatalf("%s %s: %d %s", c.method, c.target, rec.Code, rec.Body)
		}
		q := org.queries[len(org.queries)-1]
		if q.Get("requestUrl") != c.requestURL {
			t.Errorf("requestUrl %q, want %q", q.Get("requestUrl"), c.requestURL)
		}
	}
	for _, q := range org.queries {
		if q.Get("actions") != "widgets.gateways.connect" {
			t.Errorf("checked %v", q)
		}
	}
	want := edgeSeen{edgeOrgID, "acme", "https://acme.mcp.ezgh.app"}
	if len(*seen) != 2 || (*seen)[0] != want || (*seen)[1] != want {
		t.Errorf("handlers saw %+v", *seen)
	}
	if strings.Join(r.asked, ",") != "acme,acme" {
		t.Errorf("resolver asked %v", r.asked)
	}
}

// The alias is resolved before any credential is read: an unknown alias or a host outside the edge
// domain is 404, a failed lookup 503, and org-api is never asked.
func TestEdgeHostRefusals(t *testing.T) {
	h, org, _, seen := edgeService(t, manifest.ComponentAPI, nil)
	for _, c := range []struct {
		host   string
		status int
		code   string
	}{
		{"globex.mcp.ezgh.app", 404, "not_found"},
		{"mcp.ezgh.app", 404, "not_found"},
		{"a.b.mcp.ezgh.app", 404, "not_found"},
		{"acme.mcp.ezgh.app.evil.example", 404, "not_found"},
		{"widgets.ezghcloud.com", 404, "not_found"},
		{"broken.mcp.ezgh.app", 503, "unavailable"},
		{"bogus.mcp.ezgh.app", 503, "unavailable"},
	} {
		rec := edgeDo(h, "GET", c.host, gatewayIn, bearer)
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), `"`+c.code+`"`) {
			t.Errorf("%s: %d %s", c.host, rec.Code, rec.Body)
		}
	}
	// No credential at all: the alias resolves, then 401 without asking org-api.
	if rec := edgeDo(h, "POST", "acme.mcp.ezgh.app", "/central/support-bots", nil); rec.Code != 401 {
		t.Errorf("no credential: %d", rec.Code)
	}
	if org.calls != 0 || len(*seen) != 0 {
		t.Errorf("org-api asked %d times, handlers ran %d times", org.calls, len(*seen))
	}
}

// A retired alias inside its window is redirected, 308, to the same path and query on the current
// alias's host, whatever the method, before anything else runs; or refused, when the resolver
// says so.
func TestEdgeHostRedirectsRetiredAliases(t *testing.T) {
	h, org, _, seen := edgeService(t, manifest.ComponentAPI, nil)
	for _, method := range []string{"POST", "GET"} {
		target := "/central/support-bots?x=%2F"
		if method == "GET" {
			target = "/central"
		}
		rec := edgeDo(h, method, "acme-corp.mcp.ezgh.app", target, bearer)
		if rec.Code != 308 || rec.Header().Get("Location") != "https://acme.mcp.ezgh.app"+target {
			t.Errorf("%s: %d %q", method, rec.Code, rec.Header().Get("Location"))
		}
	}
	if org.calls != 0 || len(*seen) != 0 {
		t.Errorf("org-api asked %d times, handlers ran %d times", org.calls, len(*seen))
	}
}

// The platform's session cookie never reaches an edge host, so an edge-host endpoint takes only a
// bearer credential: cookies alone are 401, and are never forwarded beside a bearer.
func TestEdgeHostIsBearerOnly(t *testing.T) {
	h, org, _, _ := edgeService(t, manifest.ComponentAPI, nil)
	rec := edgeDo(h, "POST", "acme.mcp.ezgh.app", "/central/support-bots", map[string]string{"Cookie": "better-auth.session_token=member", "Origin": "https://acme.mcp.ezgh.app"})
	if rec.Code != 401 || org.calls != 0 {
		t.Fatalf("cookie only: %d, org-api asked %d times", rec.Code, org.calls)
	}
	rec = edgeDo(h, "GET", "acme.mcp.ezgh.app", gatewayIn, map[string]string{"Authorization": "Bearer " + apiKey, "Cookie": "better-auth.session_token=member"})
	if rec.Code != 200 || org.forwarded[0].Get("Cookie") != "" || org.forwarded[0].Get("Authorization") != "Bearer "+apiKey {
		t.Fatalf("%d %v", rec.Code, org.forwarded)
	}
	if q := org.queries[0]; q.Has("requestUrl") {
		t.Errorf("requestUrl sent for an API key: %v", q)
	}
}

// API-host routes never answer on an edge host, and the API host's routes keep their path's
// organization.
func TestAPIRoutesDontAnswerOnTheEdgeHost(t *testing.T) {
	h, org, _, seen := edgeService(t, manifest.ComponentAPI, nil)
	path := "/v1/organizations/" + edgeOrgID + "/things"
	if rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", path, bearer); rec.Code != 404 {
		t.Errorf("on the edge host: %d", rec.Code)
	}
	if org.calls != 0 {
		t.Errorf("org-api asked %d times", org.calls)
	}
	if rec := edgeDo(h, "GET", "widgets.ezghcloud.com", "/health", nil); rec.Code != 200 {
		t.Errorf("health: %d", rec.Code)
	}
	if rec := edgeDo(h, "GET", "widgets.ezghcloud.com", path, map[string]string{"Authorization": "Bearer " + apiKey}); rec.Code != 200 || (*seen)[0].organizationID != edgeOrgID || (*seen)[0].alias != "" || (*seen)[0].publicBase != "https://widgets.ezghcloud.com" {
		t.Errorf("on the API host: %d %+v", rec.Code, *seen)
	}
}

// Options.EdgeDomain replaces the manifest's domain (a local stack's mcp.ezgh.localhost).
func TestEdgeDomainOption(t *testing.T) {
	h, org, _, _ := edgeService(t, manifest.ComponentAPI, func(o *service.Options) { o.EdgeDomain = "MCP.ezgh.localhost" })
	if rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", gatewayIn, bearer); rec.Code != 404 {
		t.Errorf("the manifest's domain: %d", rec.Code)
	}
	if rec := edgeDo(h, "GET", "acme.mcp.ezgh.localhost", gatewayIn, bearer); rec.Code != 200 || org.queries[0].Get("requestUrl") != "https://acme.mcp.ezgh.localhost/central" {
		t.Errorf("the configured domain: %d %v", rec.Code, org.queries)
	}
}

// The edge component serves its own endpoints and the kit's built-ins, and nothing else.
func TestEdgeComponentHandler(t *testing.T) {
	h, org, _, seen := edgeService(t, manifest.ComponentEdge, nil)
	if rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", "/v1/me", bearer); rec.Code != 200 || len(*seen) != 1 || (*seen)[0].alias != "acme" {
		t.Fatalf("%d %+v", rec.Code, *seen)
	}
	if rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", gatewayIn, bearer); rec.Code != 404 {
		t.Errorf("the api component's gateway on edge: %d", rec.Code)
	}
	if rec := edgeDo(h, "GET", "10.42.0.7:4701", "/health", nil); rec.Code != 200 {
		t.Errorf("health on edge: %d", rec.Code)
	}
	if rec := edgeDo(h, "GET", "10.42.0.7:4701", "/openapi.json", nil); rec.Code != 200 {
		t.Errorf("openapi on edge: %d", rec.Code)
	}
	if org.calls != 1 {
		t.Errorf("org-api asked %d times", org.calls)
	}
}

// Handler(component) checks only that component's routes: another component's endpoint without a
// handler is fine, a handler for one is refused, a missing one of its own is refused, and edge-host
// endpoints need an OrgResolver.
func TestHandlerChecksOneComponent(t *testing.T) {
	m := edgeManifest()
	build := func(component string, resolve service.OrgResolver, routes ...string) error {
		s := service.New(service.Options{Manifest: m, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m), OrgResolver: resolve})
		for _, r := range routes {
			method, path, _ := strings.Cut(r, " ")
			s.Handle(method, path, func(*service.Call) error { return nil })
		}
		_, err := s.Handler(component)
		return err
	}
	r := (&resolver{}).resolve
	api := []string{"GET /v1/organizations/:orgId/things", "POST /v1/organizations/:orgId/things", "GET /internal/v1/things", "GET /central", "POST /central/:gatewaySlug"}
	if err := build("", r, api...); err != nil {
		t.Errorf("api by default: %v", err)
	}
	if err := build("edge", r, "GET /v1/me"); err != nil {
		t.Errorf("edge: %v", err)
	}
	if err := build("api", r, append(api, "GET /v1/me")...); err == nil || !strings.Contains(err.Error(), "GET /v1/me is component edge's, not api's") {
		t.Errorf("another component's handler: %v", err)
	}
	if err := build("edge", r); err == nil || !strings.Contains(err.Error(), "GET /v1/me is in the manifest but has no handler") {
		t.Errorf("a missing handler: %v", err)
	}
	if err := build("api", nil, api...); err == nil || !strings.Contains(err.Error(), "need Options.OrgResolver") {
		t.Errorf("no resolver: %v", err)
	}
	if err := build("relay", r); err == nil {
		t.Error("an unknown component")
	}
	s := service.New(service.Options{Manifest: m, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
	if _, err := s.Handler("api", "edge"); err == nil {
		t.Error("two components")
	}
	// A manifest without edge endpoints needs no resolver: every existing product.
	plain := testManifest()
	s = service.New(service.Options{Manifest: plain, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(plain)})
	for _, r := range []string{"GET /v1/organizations/:orgId/things", "POST /v1/organizations/:orgId/things", "GET /internal/v1/things"} {
		method, path, _ := strings.Cut(r, " ")
		s.Handle(method, path, func(*service.Call) error { return nil })
	}
	if _, err := s.Handler(); err != nil {
		t.Errorf("without edge endpoints: %v", err)
	}
}

// A write on an edge host is recorded in the organization the alias named, with the identity
// org-api answered, like any write (database: CI).
func TestEdgeWriteIsRecordedInTheAliasOrganization(t *testing.T) {
	h := &harness{db: testdb.New(t), org: &fakeOrgAPI{}}
	orgServer := httptest.NewServer(h.org)
	t.Cleanup(orgServer.Close)
	m := edgeManifest()
	s := service.New(service.Options{
		Manifest: m, DB: h.db.DB, OrgAPI: &authz.Client{BaseURL: orgServer.URL, PublicBaseURL: "https://widgets.ezghcloud.com"},
		Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m), OrgResolver: (&resolver{}).resolve,
	})
	for _, r := range []string{"GET /v1/organizations/:orgId/things", "POST /v1/organizations/:orgId/things", "GET /internal/v1/things", "GET /central"} {
		method, path, _ := strings.Cut(r, " ")
		s.Handle(method, path, func(*service.Call) error { return nil })
	}
	s.Handle("POST", "/central/:gatewaySlug", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			return &service.Response{Status: 200, Body: map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}}}, nil
		})
	})
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	if rec := edgeDo(handler, "POST", "acme.mcp.ezgh.app", "/central/support-bots", map[string]string{"Authorization": "Bearer " + apiKey}); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	events := h.events(t)
	if len(events) != 1 || events[0].EventName != "UseGateway" || events[0].Request["gatewaySlug"] != "support-bots" {
		t.Fatalf("%+v", events)
	}
	var organization string
	if err := h.db.Pool.QueryRow(t.Context(), `SELECT event->>'organizationId' FROM audit_outbox`).Scan(&organization); err != nil || organization != edgeOrgID {
		t.Errorf("organizationId %q %v", organization, err)
	}
}
