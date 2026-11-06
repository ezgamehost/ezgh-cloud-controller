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

// Invocation-token endpoints (contracts/registration.md, "Invocation-token endpoints"): the live
// component reads the bearer, hands it to the handler unchecked, never asks org-api, never reads
// a cookie, never answers CORS and never records anything.

func invocationService(t *testing.T) (http.Handler, *fakeOrgAPI, *[]string) {
	t.Helper()
	org := &fakeOrgAPI{}
	orgServer := httptest.NewServer(org)
	t.Cleanup(orgServer.Close)
	m := testManifest()
	m.Live = &manifest.LiveHost{Hostname: "widgets-live.ezghcloud.com", Port: 4750}
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "POST", Path: "/v1/invocations/checkpoint", Host: manifest.HostLive, Component: manifest.ComponentLive, Auth: manifest.AuthInvocationToken},
		manifest.Endpoint{Method: "GET", Path: "/v1/invocations/payloads/:payloadId", Host: manifest.HostLive, Component: manifest.ComponentLive, Auth: manifest.AuthInvocationToken},
	)
	s := service.New(service.Options{
		Manifest: m, OrgAPI: &authz.Client{BaseURL: orgServer.URL},
		Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m),
	})
	var seen []string
	s.Handle("POST", "/v1/invocations/checkpoint", func(c *service.Call) error {
		seen = append(seen, c.InvocationToken()+"|"+string(c.Body()))
		c.JSON(200, map[string]any{"ok": true})
		return nil
	})
	s.Handle("GET", "/v1/invocations/payloads/:payloadId", func(c *service.Call) error {
		seen = append(seen, c.InvocationToken()+"|"+c.Param("payloadId"))
		c.JSON(200, map[string]any{})
		return nil
	})
	h, err := s.Handler(manifest.ComponentLive)
	if err != nil {
		t.Fatal(err)
	}
	return h, org, &seen
}

func invocationDo(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(`{"seq":1}`))
	req.Host = "widgets-live.ezghcloud.com"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestInvocationTokenReachesTheHandlerUnchecked(t *testing.T) {
	h, org, seen := invocationService(t)
	rec := invocationDo(h, "POST", "/v1/invocations/checkpoint", map[string]string{"Authorization": "Bearer 01a.tok.inv_x.secret"})
	if rec.Code != 200 || len(*seen) != 1 || (*seen)[0] != `01a.tok.inv_x.secret|{"seq":1}` {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, *seen)
	}
	rec = invocationDo(h, "GET", "/v1/invocations/payloads/pay_abc", map[string]string{"Authorization": "Bearer t2"})
	if rec.Code != 200 || (*seen)[1] != "t2|pay_abc" {
		t.Fatalf("%d %v", rec.Code, *seen)
	}
	// A cookie beside the bearer is ignored, and a write needs no Origin.
	rec = invocationDo(h, "POST", "/v1/invocations/checkpoint", map[string]string{"Authorization": "Bearer t3", "Cookie": "better-auth.session_token=member", "Origin": "https://evil.example"})
	if rec.Code != 200 || (*seen)[2] != `t3|{"seq":1}` {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, *seen)
	}
	if len(org.forwarded) != 0 {
		t.Errorf("asked org-api: %v", org.forwarded)
	}
}

func TestInvocationTokenRefusesNoBearer(t *testing.T) {
	h, org, seen := invocationService(t)
	for name, headers := range map[string]map[string]string{
		"nothing":       {},
		"a cookie only": {"Cookie": "better-auth.session_token=member", "Origin": goodOrigin},
		"Basic":         {"Authorization": "Basic dTpw"},
		"empty bearer":  {"Authorization": "Bearer "},
		"too long":      {"Authorization": "Bearer " + strings.Repeat("a", 513)},
		"bad character": {"Authorization": "Bearer abc def"},
	} {
		rec := invocationDo(h, "POST", "/v1/invocations/checkpoint", headers)
		if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"unauthenticated"`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := invocationDo(h, "POST", "/v1/invocations/checkpoint", map[string]string{"Authorization": "Bearer " + strings.Repeat("a", 512)}); rec.Code != 200 {
		t.Errorf("512 characters: %d %s", rec.Code, rec.Body)
	}
	if len(*seen) != 1 || len(org.forwarded) != 0 {
		t.Errorf("handler ran %v, org-api asked %v", *seen, org.forwarded)
	}
}

func TestInvocationTokenEndpointsAnswerNoCORS(t *testing.T) {
	h, _, _ := invocationService(t)
	rec := invocationDo(h, "POST", "/v1/invocations/checkpoint", map[string]string{"Authorization": "Bearer t", "Origin": goodOrigin})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS on an invocation endpoint: %v", rec.Header())
	}
	pre := invocationDo(h, "OPTIONS", "/v1/invocations/checkpoint", map[string]string{"Origin": goodOrigin, "Access-Control-Request-Method": "POST"})
	if pre.Header().Get("Access-Control-Allow-Methods") != "" {
		t.Errorf("preflight answered: %d %v", pre.Code, pre.Header())
	}
}
