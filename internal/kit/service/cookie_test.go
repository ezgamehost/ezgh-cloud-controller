package service_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// A product's own edge cookie (contracts/identity-and-authz.md, "Product cookies"; row 11): the
// portal's __Host-mcp_portal, taken only on the routes that declare it, forwarded as a bearer
// credential with the portal's audience label as requestUrl, with an exact Origin check on writes;
// and bearer-only routes (row 3) refuse cookies.

const portalLabel = "https://widgets.ezghcloud.com/v1/organizations/" + edgeOrgID + "/portal"

func cookieManifest() *manifest.Manifest {
	m := edgeManifest()
	m.Edge.Cookie = &manifest.EdgeCookie{Name: "__Host-mcp_portal", Audience: "portal"}
	for i, e := range m.Endpoints {
		if e.Path == "/v1/me" {
			m.Endpoints[i].Credentials = manifest.CredentialsCookie
		}
		if e.Path == "/v1/organizations/:orgId/things" && e.Method == "GET" {
			m.Endpoints[i].Credentials = manifest.CredentialsBearer
		}
	}
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "POST", Path: "/v1/personal-servers", Host: manifest.HostEdge, Component: manifest.ComponentEdge, Auth: "caller", Credentials: manifest.CredentialsCookie, Action: "widgets.personalServers.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CreatePersonalServer"}},
		manifest.Endpoint{Method: "GET", Path: "/oauth/callback", Host: manifest.HostEdge, Component: manifest.ComponentEdge, Auth: "none", Public: manifest.PublicRedirect, Credentials: manifest.CredentialsCookie},
	)
	return m
}

func cookieService(t *testing.T, component string) (http.Handler, *fakeOrgAPI) {
	t.Helper()
	org := &fakeOrgAPI{}
	server := httptest.NewServer(org)
	t.Cleanup(server.Close)
	m := cookieManifest()
	s := service.New(service.Options{
		Manifest: m, OrgAPI: &authz.Client{BaseURL: server.URL, PublicBaseURL: "https://widgets.ezghcloud.com"},
		Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m), OrgResolver: (&resolver{}).resolve,
	})
	read := func(c *service.Call) error {
		if _, err := c.Check([]string{"widgets.gateways.connect"}, ""); err != nil {
			return err
		}
		c.JSON(200, map[string]any{"ok": true})
		return nil
	}
	if component == manifest.ComponentEdge {
		s.Handle("GET", "/v1/me", read)
		// Answers before asking org-api, so a write reaches it without a database.
		s.Handle("POST", "/v1/personal-servers", func(*service.Call) error { return httpapi.NotFound("Project") })
		s.Handle("GET", "/oauth/callback", func(c *service.Call) error {
			if err := c.Authenticate(); err != nil {
				return err
			}
			return read(c)
		})
	} else {
		s.Handle("GET", "/v1/organizations/:orgId/things", read)
		for _, r := range []string{"POST /v1/organizations/:orgId/things", "GET /internal/v1/things", "GET /central", "POST /central/:gatewaySlug"} {
			method, path, _ := strings.Cut(r, " ")
			s.Handle(method, path, func(*service.Call) error { return nil })
		}
	}
	h, err := s.Handler(component)
	if err != nil {
		t.Fatal(err)
	}
	return h, org
}

var portalCookie = "__Host-mcp_portal=" + ssoToken

// The portal's routes take its cookie, and only it: forwarded as Authorization: Bearer, with the
// audience label as requestUrl, never the URL the request is at.
func TestPortalCookie(t *testing.T) {
	h, org := cookieService(t, manifest.ComponentEdge)
	for _, target := range []string{"/v1/me", "/oauth/callback?code=c&state=s"} {
		rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", target, map[string]string{"Cookie": "theme=dark; " + portalCookie, "Authorization": "Bearer " + apiKey})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", target, rec.Code, rec.Body)
		}
		forwarded, q := org.forwarded[len(org.forwarded)-1], org.queries[len(org.queries)-1]
		if forwarded.Get("Authorization") != "Bearer "+ssoToken || forwarded.Get("Cookie") != "" || q.Get("requestUrl") != portalLabel {
			t.Errorf("%s: %v %v", target, forwarded, q)
		}
	}
	calls := org.calls
	for _, headers := range []map[string]string{
		{"Authorization": "Bearer " + ssoToken},
		{"Cookie": "better-auth.session_token=member"},
		{"Cookie": "__Host-mcp_portal=" + apiKey},
		{},
	} {
		if rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", "/v1/me", headers); rec.Code != 401 {
			t.Errorf("%v: %d", headers, rec.Code)
		}
	}
	if org.calls != calls {
		t.Errorf("org-api asked %d more times", org.calls-calls)
	}
}

// A cookie write must come from the request's own host, exactly: not another tenant's, not the
// console, not none.
func TestPortalCookieWritesCheckOrigin(t *testing.T) {
	h, org := cookieService(t, manifest.ComponentEdge)
	for origin, want := range map[string]int{
		"https://acme.mcp.ezgh.app":   404, // through to the handler
		"https://globex.mcp.ezgh.app": 403,
		"https://ACME.mcp.ezgh.app":   403,
		"http://acme.mcp.ezgh.app":    403,
		goodOrigin:                    403,
		"null":                        403,
		"":                            403,
	} {
		headers := map[string]string{"Cookie": portalCookie}
		if origin != "" {
			headers["Origin"] = origin
		}
		rec := edgeDo(h, "POST", "acme.mcp.ezgh.app", "/v1/personal-servers", headers)
		if rec.Code != want || (want == 403 && !strings.Contains(rec.Body.String(), "forbidden_origin")) {
			t.Errorf("Origin %q: %d %s", origin, rec.Code, rec.Body)
		}
	}
	if org.calls != 0 {
		t.Errorf("org-api asked %d times", org.calls)
	}
}

// credentials: bearer on the API host refuses the session cookie (an MCP endpoint, row 3).
func TestBearerOnlyOnTheAPIHost(t *testing.T) {
	h, org := cookieService(t, manifest.ComponentAPI)
	path := "/v1/organizations/" + edgeOrgID + "/things"
	if rec := edgeDo(h, "GET", "widgets.ezghcloud.com", path, map[string]string{"Cookie": "better-auth.session_token=member"}); rec.Code != 401 || org.calls != 0 {
		t.Fatalf("cookie: %d, org-api asked %d times", rec.Code, org.calls)
	}
	if rec := edgeDo(h, "GET", "widgets.ezghcloud.com", path, map[string]string{"Authorization": "Bearer " + apiKey}); rec.Code != 200 {
		t.Fatalf("bearer: %d", rec.Code)
	}
}
