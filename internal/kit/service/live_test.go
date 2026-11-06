package service_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// Live hosts (contracts/registration.md, "Live endpoints"): a product's long-lived streams on
// <namespace>-live.ezghcloud.com, served by its live component with the API host's rules.

const liveTail = "/v1/organizations/:orgId/things/:thingId/tail"

func liveManifest() *manifest.Manifest {
	m := testManifest()
	m.Live = &manifest.LiveHost{Hostname: "widgets-live.ezghcloud.com", Port: 4750}
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{
		Method: "POST", Path: liveTail, Host: manifest.HostLive, Component: manifest.ComponentLive, Auth: "caller",
		Action: "widgets.things.list", IdempotencyException: "computation", TimeoutSeconds: 900, Trail: &manifest.Trail{EventName: "TailThing"},
	})
	return m
}

type liveSeen struct {
	organizationID string
	cookie         bool
	deadline       time.Duration
}

// liveService builds component's handler. The live handler asks org-api through package authz
// (so the call isn't attributable, and nothing is recorded without a database) and streams one
// event.
func liveService(t *testing.T, component string) (http.Handler, *fakeOrgAPI, *[]liveSeen) {
	t.Helper()
	org := &fakeOrgAPI{}
	orgServer := httptest.NewServer(org)
	t.Cleanup(orgServer.Close)
	m := liveManifest()
	s := service.New(service.Options{
		Manifest: m, OrgAPI: &authz.Client{BaseURL: orgServer.URL, PublicBaseURL: "https://widgets-live.ezghcloud.com"},
		Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m),
	})
	var seen []liveSeen
	nothing := func(c *service.Call) error { c.JSON(200, map[string]any{}); return nil }
	switch component {
	case manifest.ComponentLive:
		s.HandlePrivate("POST", liveTail, func(c *service.Call) error {
			if _, err := c.Service.OrgAPI.Check(c.Ctx, c.Credential(), c.OrganizationID, []string{c.Endpoint.Action}, ""); err != nil {
				return err
			}
			deadline, _ := c.Ctx.Deadline()
			seen = append(seen, liveSeen{c.OrganizationID, c.Credential().Header == "Cookie", time.Until(deadline)})
			return c.Stream(200, http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-store"}}, func(w io.Writer) error {
				_, err := io.WriteString(w, "id: 1\ndata: {}\n\nevent: end\ndata: {\"reason\":\"draining\"}\n\n")
				return err
			})
		})
	default:
		s.Handle("GET", "/v1/organizations/:orgId/things", nothing)
		s.Handle("POST", "/v1/organizations/:orgId/things", nothing)
		s.Handle("GET", "/internal/v1/things", nothing)
	}
	h, err := s.Handler(component)
	if err != nil {
		t.Fatal(err)
	}
	return h, org, &seen
}

func liveDo(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(`{"lastEventId":null}`))
	req.Host = "widgets-live.ezghcloud.com"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var liveTailURL = "/v1/organizations/" + orgID + "/things/" + edgeOrgID + "/tail"

// The live component takes the platform's credentials, as the API host does: the session cookie
// (from an allowed Origin), API keys and OAuth access tokens, never bearer-only. Its stream runs
// under the endpoint's timeoutSeconds, not the server's 60 seconds.
func TestLiveComponentTakesThePlatformsCredentials(t *testing.T) {
	h, org, seen := liveService(t, manifest.ComponentLive)
	for _, c := range []struct {
		name    string
		headers map[string]string
		cookie  bool
	}{
		{"session cookie", member, true},
		{"API key", map[string]string{"Authorization": "Bearer " + apiKey}, false},
		{"OAuth access token", map[string]string{"Authorization": "Bearer " + oauthToken}, false},
	} {
		rec := liveDo(h, "POST", liveTailURL, c.headers)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(rec.Body.String(), "event: end") {
			t.Fatalf("%s: %d %q %s", c.name, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
		last := (*seen)[len(*seen)-1]
		if last.organizationID != orgID || last.cookie != c.cookie || last.deadline < 899*time.Second {
			t.Errorf("%s: %+v", c.name, last)
		}
	}
	if org.forwarded[0].Get("Cookie") != "better-auth.session_token=member" || org.forwarded[1].Get("Authorization") != "Bearer "+apiKey {
		t.Errorf("forwarded %v", org.forwarded)
	}
}

// The checks before authorization are the API host's, and none of them asks org-api: no
// credential is 401, a cookie write without an allowed Origin 403, a malformed ID 404.
func TestLiveComponentRefusals(t *testing.T) {
	h, org, seen := liveService(t, manifest.ComponentLive)
	for _, c := range []struct {
		name    string
		target  string
		headers map[string]string
		status  int
	}{
		{"no credential", liveTailURL, nil, 401},
		{"a cookie without Origin", liveTailURL, map[string]string{"Cookie": "better-auth.session_token=member"}, 403},
		{"a cookie from another Origin", liveTailURL, map[string]string{"Cookie": "better-auth.session_token=member", "Origin": "https://evil.example"}, 403},
		{"a malformed ID", "/v1/organizations/" + orgID + "/things/nope/tail", map[string]string{"Authorization": "Bearer " + apiKey}, 404},
	} {
		if rec := liveDo(h, "POST", c.target, c.headers); rec.Code != c.status {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if org.calls != 0 || len(*seen) != 0 {
		t.Errorf("org-api asked %d times, the handler ran %d times", org.calls, len(*seen))
	}
}

// A browser's preflight from an allowed origin passes, and the stream's answer carries the CORS
// headers, Retry-After exposed.
func TestLiveComponentCORS(t *testing.T) {
	h, _, _ := liveService(t, manifest.ComponentLive)
	rec := liveDo(h, "OPTIONS", liveTailURL, map[string]string{"Origin": goodOrigin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type"})
	if rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != goodOrigin || !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Fatalf("preflight %d %v", rec.Code, rec.Header())
	}
	rec = liveDo(h, "OPTIONS", liveTailURL, map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("another origin's preflight %v", rec.Header())
	}
	rec = liveDo(h, "POST", liveTailURL, member)
	if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != goodOrigin || !strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "Retry-After") {
		t.Errorf("stream %d %v", rec.Code, rec.Header())
	}
}

// Each component serves only its own routes: API-host routes answer 404 on the live component,
// and the live route answers 404 on the API component. Both serve the kit's built-ins.
func TestLiveAndAPIComponentsServeOnlyTheirOwn(t *testing.T) {
	live, liveOrg, _ := liveService(t, manifest.ComponentLive)
	api, apiOrg, _ := liveService(t, manifest.ComponentAPI)
	key := map[string]string{"Authorization": "Bearer " + apiKey}
	for _, c := range []struct {
		name           string
		h              http.Handler
		method, target string
	}{
		{"an API-host read on live", live, "GET", "/v1/organizations/" + orgID + "/things"},
		{"an API-host write on live", live, "POST", "/v1/organizations/" + orgID + "/things"},
		{"an internal route on live", live, "GET", "/internal/v1/things"},
		{"the live route on api", api, "POST", liveTailURL},
	} {
		if rec := liveDo(c.h, c.method, c.target, key); rec.Code != 404 {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if liveOrg.calls != 0 || apiOrg.calls != 0 {
		t.Errorf("org-api asked %d and %d times", liveOrg.calls, apiOrg.calls)
	}
	for _, h := range []http.Handler{live, api} {
		for _, path := range []string{"/health", "/openapi.json"} {
			if rec := liveDo(h, "GET", path, nil); rec.Code != 200 {
				t.Errorf("%s: %d", path, rec.Code)
			}
		}
	}
	if rec := liveDo(api, "GET", "/v1/organizations/"+orgID+"/things", key); rec.Code != 200 {
		t.Errorf("the API host's own route: %d %s", rec.Code, rec.Body)
	}
}

// Handler("live") drift-checks only the live component's routes.
func TestHandlerChecksTheLiveComponent(t *testing.T) {
	m := liveManifest()
	build := func(component string, routes ...string) error {
		s := service.New(service.Options{Manifest: m, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
		for _, r := range routes {
			method, path, _ := strings.Cut(r, " ")
			s.Handle(method, path, func(*service.Call) error { return nil })
		}
		_, err := s.Handler(component)
		return err
	}
	api := []string{"GET /v1/organizations/:orgId/things", "POST /v1/organizations/:orgId/things", "GET /internal/v1/things"}
	if err := build("live", "POST "+liveTail); err != nil {
		t.Errorf("live: %v", err)
	}
	if err := build("", api...); err != nil {
		t.Errorf("api: %v", err)
	}
	if err := build("live"); err == nil || !strings.Contains(err.Error(), "POST "+liveTail+" is in the manifest but has no handler") {
		t.Errorf("a missing live handler: %v", err)
	}
	if err := build("api", append(api, "POST "+liveTail)...); err == nil || !strings.Contains(err.Error(), "POST "+liveTail+" is component live's, not api's") {
		t.Errorf("the live route on api: %v", err)
	}
	if err := build("live", "POST "+liveTail, "GET /v1/organizations/:orgId/things"); err == nil || !strings.Contains(err.Error(), "GET /v1/organizations/:orgId/things is component api's, not live's") {
		t.Errorf("an API route on live: %v", err)
	}
}

// A stream is recorded once, when it opens, before any byte is sent (database: CI).
func TestLiveStreamIsRecordedWhenItOpens(t *testing.T) {
	h := &harness{db: testdb.New(t), org: &fakeOrgAPI{}}
	orgServer := httptest.NewServer(h.org)
	t.Cleanup(orgServer.Close)
	m := liveManifest()
	s := service.New(service.Options{
		Manifest: m, DB: h.db.DB, OrgAPI: &authz.Client{BaseURL: orgServer.URL, PublicBaseURL: "https://widgets-live.ezghcloud.com"},
		Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m),
	})
	s.HandlePrivate("POST", liveTail, func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		c.SetRequestParameters(map[string]any{"thingId": c.R.PathValue("thingId")})
		return c.Stream(200, http.Header{"Content-Type": {"text/event-stream"}}, func(w io.Writer) error {
			for i := 0; i < 3; i++ {
				if _, err := io.WriteString(w, "data: {}\n\n"); err != nil {
					return err
				}
			}
			return nil
		})
	})
	handler, err := s.Handler(manifest.ComponentLive)
	if err != nil {
		t.Fatal(err)
	}
	if rec := liveDo(handler, "POST", liveTailURL, member); rec.Code != 200 || strings.Count(rec.Body.String(), "data:") != 3 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	events := h.events(t)
	if len(events) != 1 || events[0].EventName != "TailThing" {
		t.Fatalf("%+v", events)
	}
}
