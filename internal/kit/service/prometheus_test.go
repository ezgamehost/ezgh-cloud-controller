package service_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
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

// errorModel prometheus (contracts/api-conventions.md, "Prometheus query surface"): every failure
// the kit or the handler raises is the Prometheus HTTP API's body, the GET and the computation
// POST share one event, and both are recorded readOnly.
func TestPrometheusErrorModelAndReadOnlyComputations(t *testing.T) {
	const path = "/v1/organizations/:orgId/prometheus/api/v1/query"
	trail := &manifest.Trail{EventName: "QueryMetrics", EventCategory: manifest.CategoryData, Sensitive: []string{"query"}}
	m := testManifest()
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "GET", Path: path, Auth: "caller", Action: "widgets.metrics.query", ErrorModel: manifest.ErrorModelPrometheus, Trail: trail},
		manifest.Endpoint{Method: "POST", Path: path, Auth: "caller", Action: "widgets.metrics.query", ErrorModel: manifest.ErrorModelPrometheus, IdempotencyException: "computation", Trail: trail},
	)
	h := &harness{db: testdb.New(t), org: &fakeOrgAPI{}}
	upstream := httptest.NewServer(h.org)
	t.Cleanup(upstream.Close)
	var logs bytes.Buffer
	s := service.New(service.Options{Manifest: m, DB: h.db.DB, OrgAPI: &authz.Client{BaseURL: upstream.URL}, Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: slog.New(slog.NewJSONHandler(&logs, nil)), OpenAPIOverlay: stubOverlay(m)})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
	query := func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		if c.R.Method == "POST" {
			raw, _ := io.ReadAll(c.R.Body)
			c.SetRequestParameters(map[string]any{"query": string(raw)})
		}
		if strings.Contains(c.R.URL.RawQuery, "fail") {
			return &httpapi.Error{Status: 422, Code: "query_too_expensive", Message: "The query ran out of time", Extra: map[string]any{"budget": "time"}}
		}
		return c.Stream(200, http.Header{"Content-Type": {"application/json"}}, func(w io.Writer) error {
			_, err := io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			return err
		})
	}
	s.HandlePrivate("GET", path, query)
	s.HandlePrivate("POST", path, query)
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	url := "/v1/organizations/" + orgID + "/prometheus/api/v1/query"
	call := func(method, target, cookie, origin string) (int, map[string]any, http.Header) {
		var body io.Reader
		if method == "POST" {
			body = strings.NewReader("query=up")
		}
		req := httptest.NewRequest(method, target, body)
		if cookie != "" {
			req.Header.Set("Cookie", "better-auth.session_token="+cookie)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Header()
	}
	for _, c := range []struct {
		name, method, target, cookie, origin string
		status                               int
		errorType, code                      string
	}{
		{"no credential", "GET", url, "", "", 401, "unauthorized", "unauthenticated"},
		{"denied", "GET", url, "denied", "", 403, "forbidden", "access_denied"},
		{"cookie POST from another origin", "POST", url, "member", "https://evil.example", 403, "forbidden", "forbidden_origin"},
		{"malformed organization", "GET", "/v1/organizations/nope/prometheus/api/v1/query", "member", "", 404, "not_found", "not_found"},
		{"the handler's own error", "GET", url + "?fail=1", "member", "", 422, "timeout", "query_too_expensive"},
	} {
		status, body, _ := call(c.method, c.target, c.cookie, c.origin)
		if status != c.status || body["status"] != "error" || body["errorType"] != c.errorType || body["code"] != c.code || body["error"] == nil {
			t.Errorf("%s: %d %v", c.name, status, body)
		}
	}
	// The rest of the API keeps the platform's body.
	if status, body, _ := call("GET", things, "", ""); status != 401 || body["error"].(map[string]any)["code"] != "unauthenticated" {
		t.Errorf("things: %d %v", status, body)
	}

	before := len(h.events(t))
	for _, method := range []string{"GET", "POST"} {
		if status, body, _ := call(method, url, "member", goodOrigin); status != 200 || body["status"] != "success" {
			t.Fatalf("%s: %d %v", method, status, body)
		}
	}
	ev := h.events(t)
	if len(ev) != before+2 {
		t.Fatalf("events %v", ev)
	}
	for _, e := range ev[before:] {
		if e.EventName != "QueryMetrics" || !e.ReadOnly || e.Response != nil || e.ErrorCode != nil {
			t.Errorf("event %+v", e)
		}
	}
	if ev[len(ev)-1].Request["query"] != "HIDDEN_DUE_TO_SECURITY_REASONS" {
		t.Errorf("the POST's query %v", ev[len(ev)-1].Request)
	}
	// The denial and the handler's 422 were recorded too, readOnly.
	var denied, failed bool
	for _, e := range ev {
		if e.EventName == "QueryMetrics" && e.ErrorCode != nil {
			denied = denied || *e.ErrorCode == "access_denied"
			failed = failed || *e.ErrorCode == "query_too_expensive"
			if !e.ReadOnly {
				t.Errorf("a failed read isn't readOnly: %+v", e)
			}
		}
	}
	if !denied || !failed {
		t.Errorf("denied %t failed %t: %v", denied, failed, ev)
	}
	if strings.Contains(logs.String(), "a write responded without Transact") {
		t.Error("a computation was logged as a write")
	}
}
