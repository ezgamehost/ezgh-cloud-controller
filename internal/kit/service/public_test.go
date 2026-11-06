package service_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

// Public endpoints (contracts/registration.md, "Public endpoints"): unauthenticated GETs with
// trail null, which a redirect handler may still authenticate and record itself.

func publicManifest() *manifest.Manifest {
	m := edgeManifest()
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "GET", Path: "/v1/oauth/callback", Auth: "none", Public: manifest.PublicRedirect},
		manifest.Endpoint{Method: "GET", Path: "/.well-known/jwks.json", Auth: "none", Public: manifest.PublicMetadata},
		manifest.Endpoint{Method: "GET", Path: "/.well-known/oauth-protected-resource", Host: manifest.HostEdge, Auth: "none", Public: manifest.PublicMetadata},
	)
	return m
}

type publicSeen struct {
	authErr  *httpapi.Error
	event    *trails.Event
	eventErr error
}

// publicService serves the api component with a callback that authenticates, sets the
// organization from its "state", checks, and builds the event it would record.
func publicService(t *testing.T, db *kitdb.DB, record func(*service.Call, *trails.Event) error) (http.Handler, *fakeOrgAPI, *[]publicSeen) {
	t.Helper()
	org := &fakeOrgAPI{}
	orgServer := httptest.NewServer(org)
	t.Cleanup(orgServer.Close)
	m := publicManifest()
	s := service.New(service.Options{
		Manifest: m, DB: db, OrgAPI: &authz.Client{BaseURL: orgServer.URL, PublicBaseURL: "https://widgets.ezghcloud.com"},
		Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m), OrgResolver: (&resolver{}).resolve,
	})
	for _, r := range []string{"GET /v1/organizations/:orgId/things", "POST /v1/organizations/:orgId/things", "GET /internal/v1/things", "GET /central", "POST /central/:gatewaySlug"} {
		method, path, _ := strings.Cut(r, " ")
		s.Handle(method, path, func(c *service.Call) error {
			if err := c.Authenticate(); err == nil {
				t.Errorf("%s: Authenticate on a route the kit authenticates", r)
			}
			return nil
		})
	}
	var seen []publicSeen
	s.Handle("GET", "/v1/oauth/callback", func(c *service.Call) error {
		var p publicSeen
		defer func() { seen = append(seen, p) }()
		if err := c.Authenticate(); err != nil {
			p.authErr = httpapi.ErrorOf(err)
			c.W.Header().Set("Location", "https://console.ezghcloud.com/sign-in")
			c.W.WriteHeader(http.StatusSeeOther)
			return nil
		}
		// The flow's state names the organization and project; the path doesn't.
		c.OrganizationID = edgeOrgID
		project := "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
		c.ProjectID = &project
		if _, err := c.Check([]string{"widgets.connections.connect"}, ""); err != nil {
			return err
		}
		c.SetRequestParameters(map[string]any{"serverId": "0199a3f3-1a2b-7c3d-8e4f-5a6b7c8d9e0f", "accessToken": "never-recorded-token"})
		p.event, p.eventErr = c.TrailEvent("ConnectServer")
		if record != nil && p.eventErr == nil {
			if err := record(c, p.event); err != nil {
				return err
			}
		}
		c.W.Header().Set("Location", "https://console.ezghcloud.com/mcp-central")
		c.W.WriteHeader(http.StatusSeeOther)
		return nil
	})
	s.Handle("GET", "/.well-known/jwks.json", func(c *service.Call) error {
		c.JSON(200, map[string]any{"keys": []any{}})
		return nil
	})
	s.Handle("GET", "/.well-known/oauth-protected-resource", func(c *service.Call) error {
		c.JSON(200, map[string]any{"resource": c.PublicBase() + "/central", "organizationId": c.OrganizationID})
		return nil
	})
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h, org, &seen
}

// A redirect handler reads the person's credential, checks it in the organization its state names
// and builds the event of the operation it completes; the kit records nothing for the route.
func TestRedirectHandlerAuthenticatesAndBuildsItsEvent(t *testing.T) {
	h, org, seen := publicService(t, nil, nil)
	rec := edgeDo(h, "GET", "widgets.ezghcloud.com", "/v1/oauth/callback?code=c&state=s", map[string]string{
		"Cookie": "better-auth.session_token=member", "X-Forwarded-For": "203.0.113.9", "User-Agent": "Mozilla/5.0",
	})
	if rec.Code != 303 || rec.Header().Get("Location") != "https://console.ezghcloud.com/mcp-central" {
		t.Fatalf("%d %v %s", rec.Code, rec.Header(), rec.Body)
	}
	if org.calls != 1 || org.forwarded[0].Get("Cookie") != "better-auth.session_token=member" {
		t.Fatalf("org-api: %d %v", org.calls, org.forwarded)
	}
	p := (*seen)[0]
	if p.eventErr != nil {
		t.Fatal(p.eventErr)
	}
	e := p.event
	if e.EventName != "ConnectServer" || e.EventType != "ApiCall" || e.EventCategory != "Management" || e.ReadOnly || e.OrganizationID != edgeOrgID ||
		e.ProjectID == nil || e.EventSource != "widgets.ezghcloud.com" || *e.SourceIPAddress != "203.0.113.9" || *e.UserAgent != "Mozilla/5.0" || *e.RequestID == "" {
		t.Errorf("%+v", e)
	}
	if string(e.UserIdentity) != identity || len(e.Resources) != 1 || e.Resources[0].Type != "Organization" {
		t.Errorf("identity %s resources %+v", e.UserIdentity, e.Resources)
	}
	if e.RequestParameters["accessToken"] != trails.Hidden || e.RequestParameters["serverId"] == nil || e.RequestParameters["state"] != nil {
		t.Errorf("parameters %v", e.RequestParameters)
	}
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "never-recorded-token") {
		t.Errorf("a secret in the event: %s", raw)
	}
}

// Without a credential, Authenticate refuses and the handler answers as it likes (here, to sign
// in); nothing asks org-api.
func TestRedirectHandlerWithoutACredential(t *testing.T) {
	h, org, seen := publicService(t, nil, nil)
	rec := edgeDo(h, "GET", "widgets.ezghcloud.com", "/v1/oauth/callback?code=c&state=s", nil)
	if rec.Code != 303 || rec.Header().Get("Location") != "https://console.ezghcloud.com/sign-in" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if p := (*seen)[0]; p.authErr == nil || p.authErr.Status != 401 || org.calls != 0 {
		t.Errorf("%+v, org-api asked %d times", p, org.calls)
	}
}

// Public metadata answers without a credential; on an edge host its alias still has to resolve
// (no PRM for a host with no gateways), and the handler builds URLs from the resolved base.
func TestPublicMetadata(t *testing.T) {
	h, org, _ := publicService(t, nil, nil)
	if rec := edgeDo(h, "GET", "widgets.ezghcloud.com", "/.well-known/jwks.json", nil); rec.Code != 200 {
		t.Errorf("jwks: %d", rec.Code)
	}
	rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", "/.well-known/oauth-protected-resource", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"resource":"https://acme.mcp.ezgh.app/central"`) || !strings.Contains(rec.Body.String(), edgeOrgID) {
		t.Errorf("prm: %d %s", rec.Code, rec.Body)
	}
	if rec := edgeDo(h, "GET", "globex.mcp.ezgh.app", "/.well-known/oauth-protected-resource", nil); rec.Code != 404 {
		t.Errorf("prm for an unknown alias: %d", rec.Code)
	}
	if rec := edgeDo(h, "GET", "acme-corp.mcp.ezgh.app", "/.well-known/oauth-protected-resource", nil); rec.Code != 308 {
		t.Errorf("prm for a retired alias: %d", rec.Code)
	}
	if org.calls != 0 {
		t.Errorf("org-api asked %d times", org.calls)
	}
	// Authenticate on the kit's own routes is refused (the handlers above check it).
	edgeDo(h, "GET", "acme.mcp.ezgh.app", "/central", bearer)
}

// The event a redirect handler records is in the outbox, in its organization, once (database:
// CI).
func TestRedirectHandlerRecordsItsEvent(t *testing.T) {
	db := testdb.New(t)
	h, _, _ := publicService(t, db.DB, func(c *service.Call, e *trails.Event) error {
		return c.Service.DB.RunTx(c.Ctx, nil, func(tx *kitdb.Tx) error { return trails.Insert(c.Ctx, tx, e) })
	})
	if rec := edgeDo(h, "GET", "widgets.ezghcloud.com", "/v1/oauth/callback?code=c&state=s", map[string]string{"Cookie": "better-auth.session_token=member"}); rec.Code != 303 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var name, organization string
	var count int
	if err := db.Pool.QueryRow(t.Context(), `SELECT count(*), min(event->>'eventName'), min(event->>'organizationId') FROM audit_outbox`).Scan(&count, &name, &organization); err != nil {
		t.Fatal(err)
	}
	if count != 1 || name != "ConnectServer" || organization != edgeOrgID {
		t.Errorf("%d %s %s", count, name, organization)
	}
}
