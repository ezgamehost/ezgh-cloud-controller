package service_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/logging"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

// accessService is the harness's service without a database: its routes record nothing in
// Trails, so the access line is all there is to see. Every request goes through
// httpapi.AccessLog, as ServeWith serves it.
func accessService(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	org := &fakeOrgAPI{}
	orgServer := httptest.NewServer(org)
	t.Cleanup(orgServer.Close)
	m := &manifest.Manifest{
		ManifestVersion: 1, Namespace: "widgets", DisplayName: "Widgets", Description: "x",
		API: manifest.API{Hostname: "widgets.ezghcloud.com", Port: 4700},
		Endpoints: []manifest.Endpoint{
			{Method: "GET", Path: "/health", Auth: "none"},
			{Method: "GET", Path: "/ready", Auth: "none"},
			{Method: "GET", Path: "/v1/organizations/:orgId/things", Auth: "caller", Action: "widgets.things.list"},
			{Method: "GET", Path: "/internal/v1/things", Auth: "service"},
			{Method: "GET", Path: "/openapi.json", Auth: "none"},
		},
	}
	var buf bytes.Buffer
	logger := logging.New(&buf, "widgets-api", false)
	s := service.New(service.Options{
		Manifest: m, OrgAPI: &authz.Client{BaseURL: orgServer.URL, InternalToken: "t", PublicBaseURL: "https://widgets.ezghcloud.com"},
		Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: logger,
		InternalToken: "internal-secret-token", OpenAPIOverlay: stubOverlay(m),
	})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		c.JSON(200, map[string]any{"things": []any{}, "nextCursor": nil})
		return nil
	})
	s.Handle("GET", "/internal/v1/things", func(c *service.Call) error {
		c.JSON(200, map[string]string{"caller": c.ServiceCaller})
		return nil
	})
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return httpapi.AccessLog(logger, h), &buf
}

// accessLines serves one request and returns its "request" lines.
func accessLines(t *testing.T, path string, headers map[string]string) (int, []map[string]any) {
	t.Helper()
	h, buf := accessService(t)
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		if m["message"] == "request" {
			out = append(out, m)
		}
	}
	return rec.Code, out
}

func TestAccessLineForAnAuthorizedRoute(t *testing.T) {
	status, lines := accessLines(t, things, map[string]string{"Cookie": "better-auth.session_token=member"})
	if status != 200 || len(lines) != 1 {
		t.Fatalf("status %d, request lines %v", status, lines)
	}
	l := lines[0]
	for k, want := range map[string]any{
		"kind": "access", "route": "/v1/organizations/{orgId}/things", "status": float64(200),
		"principalType": "user", "principalId": "u1", "organizationId": orgID,
	} {
		if l[k] != want {
			t.Errorf("%s = %v, want %v", k, l[k], want)
		}
	}
	if _, ok := l["errorCode"]; ok {
		t.Errorf("errorCode on a 200: %v", l)
	}
}

func TestAccessLineForAnUnknownPathIsUnmatched(t *testing.T) {
	status, lines := accessLines(t, "/v1/nope/secret-path", nil)
	if status != 404 || len(lines) != 1 || lines[0]["route"] != "unmatched" || lines[0]["errorCode"] != "not_found" {
		t.Fatalf("status %d, lines %v", status, lines)
	}
}

func TestAccessLineForADenialHasItsCode(t *testing.T) {
	status, lines := accessLines(t, things, map[string]string{"Cookie": "better-auth.session_token=denied"})
	if status != 403 || len(lines) != 1 || lines[0]["errorCode"] != "access_denied" || lines[0]["level"] != "info" {
		t.Fatalf("status %d, lines %v", status, lines)
	}
}

func TestAccessLineForAnInternalCall(t *testing.T) {
	status, lines := accessLines(t, "/internal/v1/things", map[string]string{"X-Internal-Token": "internal-secret-token"})
	if status != 200 || len(lines) != 1 || lines[0]["principalType"] != "internal" || lines[0]["route"] != "/internal/v1/things" {
		t.Fatalf("status %d, lines %v", status, lines)
	}
	if _, ok := lines[0]["principalId"]; ok {
		t.Errorf("an internal call has a principalId: %v", lines[0])
	}
}
