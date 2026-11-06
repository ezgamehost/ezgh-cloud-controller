package service_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/logging"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

const callbackToken = "01a.0123456789abcdefghijklmnopqrstuvwxyzABCDEFG"

// A public callback (contracts/registration.md, "Public endpoints") is served without a
// credential, its token reaches the handler as a path value, the request log names the route
// pattern only, and no CORS is answered, preflights included.
func TestPublicCallbackNeverLogsItsToken(t *testing.T) {
	var logs bytes.Buffer
	logger := logging.New(&logs, "widgets-api", false)
	m := testManifest()
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "POST", Path: "/public/v1/callbacks/:token", Auth: "none", Public: manifest.PublicCallback})
	s := service.New(service.Options{Manifest: m, Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: logger, OpenAPIOverlay: stubOverlay(m)})
	var got, body string
	s.Handle("POST", "/public/v1/callbacks/:token", func(c *service.Call) error {
		got, body = c.Param("token"), string(c.Body())
		c.JSON(200, map[string]any{"status": "completed"})
		return nil
	})
	stub := func(c *service.Call) error { c.JSON(200, map[string]any{}); return nil }
	s.Handle("GET", "/v1/organizations/:orgId/things", stub)
	s.Handle("POST", "/v1/organizations/:orgId/things", stub)
	s.Handle("GET", "/internal/v1/things", stub)
	kh, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	h := httpapi.AccessLog(logger, kh)
	req := httptest.NewRequest("POST", "/public/v1/callbacks/"+callbackToken, strings.NewReader(`{"approved":true}`))
	req.Header.Set("Origin", goodOrigin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got != callbackToken || body != `{"approved":true}` {
		t.Fatalf("%d %s %q %q", rec.Code, rec.Body, got, body)
	}
	if strings.Contains(logs.String(), callbackToken) || !strings.Contains(logs.String(), `"route":"/public/v1/callbacks/{token}"`) {
		t.Fatalf("log: %s", logs.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS on a callback: %v", rec.Header())
	}
	pre := httptest.NewRequest("OPTIONS", "/public/v1/callbacks/"+callbackToken, nil)
	pre.Header.Set("Origin", goodOrigin)
	pre.Header.Set("Access-Control-Request-Method", "POST")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, pre)
	if rec.Header().Get("Access-Control-Allow-Methods") != "" || strings.Contains(logs.String(), callbackToken) {
		t.Errorf("preflight answered: %d %v", rec.Code, rec.Header())
	}
	// The kit reads the body, bounded as on every route: over 1 MiB is 413, and the handler never runs.
	got = ""
	big := httptest.NewRequest("POST", "/public/v1/callbacks/"+callbackToken, strings.NewReader(strings.Repeat("a", httpapi.MaxBodyBytes+1)))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, big)
	if rec.Code != http.StatusRequestEntityTooLarge || got != "" {
		t.Errorf("over-size callback: %d %s, handler saw %q", rec.Code, rec.Body, got)
	}
}
