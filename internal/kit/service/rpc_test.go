package service_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// Trails per JSON-RPC method (contracts/trails.md, "JSON-RPC endpoints"): a gateway endpoint
// records tools/call as a Data event CallTool and nothing for initialize or tools/list.

func rpcManifest() *manifest.Manifest {
	m := edgeManifest()
	entry := &manifest.Trail{EventName: "CallTool", EventCategory: manifest.CategoryData, Sensitive: []string{"arguments"}}
	for i, e := range m.Endpoints {
		if e.Method == "POST" && e.Path == "/central/:gatewaySlug" {
			m.Endpoints[i].Trail = &manifest.Trail{ByRPCMethod: map[string]*manifest.Trail{"tools/call": entry, "tools/list": nil, "initialize": nil}}
		}
	}
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{
		Method: "POST", Path: "/central", Host: manifest.HostEdge, Auth: "caller", Action: "widgets.gateways.connect", IdempotencyException: "computation",
		Trail: &manifest.Trail{ByRPCMethod: map[string]*manifest.Trail{"tools/call": entry}, Otherwise: nil},
	})
	return m
}

// rpcService serves the api component: /central/:gatewaySlug reads its body through the kit (the
// method comes from it), /central is private (the handler names the method). Both answer a tool
// error with HTTP 200 when the tool is "broken".
func rpcService(t *testing.T, db *kitdb.DB) http.Handler {
	t.Helper()
	org := &fakeOrgAPI{}
	orgServer := httptest.NewServer(org)
	t.Cleanup(orgServer.Close)
	m := rpcManifest()
	s := service.New(service.Options{
		Manifest: m, DB: db, OrgAPI: &authz.Client{BaseURL: orgServer.URL, PublicBaseURL: "https://widgets.ezghcloud.com"},
		Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m), OrgResolver: (&resolver{}).resolve,
	})
	for _, r := range []string{"GET /v1/organizations/:orgId/things", "POST /v1/organizations/:orgId/things", "GET /internal/v1/things", "GET /central"} {
		method, path, _ := strings.Cut(r, " ")
		s.Handle(method, path, func(*service.Call) error { return nil })
	}
	answer := func(c *service.Call, request map[string]any) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		result := map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{}}
		if params, _ := request["params"].(map[string]any); params != nil && params["name"] == "broken" {
			c.SetTrailError("upstream_unavailable", "The upstream server didn't answer")
			result["result"] = map[string]any{"isError": true}
		}
		if request["method"] == "tools/call" {
			return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: 200, Body: result}, nil
			})
		}
		c.JSON(200, result)
		return nil
	}
	s.Handle("POST", "/central/:gatewaySlug", func(c *service.Call) error {
		var request map[string]any
		_ = json.Unmarshal(c.Body(), &request)
		return answer(c, request)
	})
	s.HandlePrivate("POST", "/central", func(c *service.Call) error {
		raw, _ := io.ReadAll(io.LimitReader(c.R.Body, 1<<20))
		var request map[string]any
		_ = json.Unmarshal(raw, &request)
		method, _ := request["method"].(string)
		c.SetRPCMethod(method)
		return answer(c, request)
	})
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func rpcCall(h http.Handler, path, method, tool string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": map[string]any{"name": tool, "arguments": map[string]any{"title": "never recorded"}}})
	req := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
	req.Host = "acme.mcp.ezgh.app"
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Methods whose entry is null (or that fall to a null otherwise) record nothing: no database is
// touched at all.
func TestRPCMethodsWithoutAnEntryRecordNothing(t *testing.T) {
	h := rpcService(t, nil)
	for _, c := range []struct{ path, method string }{
		{"/central/support-bots", "tools/list"},
		{"/central/support-bots", "initialize"},
		{"/central", "tools/list"},
		{"/central", "ping"},
	} {
		if rec := rpcCall(h, c.path, c.method, ""); rec.Code != 200 {
			t.Errorf("%s %s: %d %s", c.path, c.method, rec.Code, rec.Body)
		}
	}
}

// tools/call records CallTool, a Data event, with its params hidden; a tool error answered with
// HTTP 200 is the event's errorCode; the private endpoint records by the method its handler named
// (database: CI).
func TestRPCToolCallsAreDataEvents(t *testing.T) {
	h := &harness{db: testdb.New(t)}
	handler := rpcService(t, h.db.DB)
	for _, c := range []struct{ path, tool string }{
		{"/central/support-bots", "create_issue"},
		{"/central/support-bots", "broken"},
		{"/central", "create_issue"},
	} {
		if rec := rpcCall(handler, c.path, "tools/call", c.tool); rec.Code != 200 {
			t.Fatalf("%s %s: %d %s", c.path, c.tool, rec.Code, rec.Body)
		}
	}
	rpcCall(handler, "/central/support-bots", "tools/list", "")
	rows, err := h.db.Pool.Query(t.Context(), `SELECT event::text FROM audit_outbox ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []map[string]any
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		var e map[string]any
		_ = json.Unmarshal([]byte(text), &e)
		events = append(events, e)
	}
	if len(events) != 3 {
		t.Fatalf("%d events: %v", len(events), events)
	}
	for i, e := range events {
		if e["eventName"] != "CallTool" || e["eventCategory"] != "Data" || e["readOnly"] != false || e["organizationId"] != edgeOrgID {
			t.Errorf("event %d: %v", i, e)
		}
	}
	// Without SetRequestParameters the body's fields are names only: params, arguments and all.
	if events[0]["requestParameters"].(map[string]any)["params"] != "HIDDEN_DUE_TO_SECURITY_REASONS" || events[0]["errorCode"] != nil {
		t.Errorf("first: %v", events[0])
	}
	if events[1]["errorCode"] != "upstream_unavailable" || events[1]["errorMessage"] != "The upstream server didn't answer" || events[1]["responseElements"] != nil {
		t.Errorf("tool error: %v", events[1])
	}
	if p, _ := events[2]["requestParameters"].(map[string]any); len(p) != 0 {
		t.Errorf("a private endpoint's automatic parameters: %v", p)
	}
}
