package service_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/cursor"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/openapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

const (
	orgID    = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	orgSlug  = "org_k3f9a0x2m7qp"
	identity = `{"type":"User","principalId":"u1","userName":"ada@acme.test","isRoot":false,"accessKeyId":null,"sessionContext":{"credential":"session","sessionIssuedAt":null,"ssoProviderId":null,"mfaAuthenticated":false},"invokedBy":null}`
	apiKey   = "ezgh_0123456789012345678901234567890123456789"
	// A person's OAuth access token (contracts/identity-and-authz.md 1.2); deniedToken's user may do
	// nothing.
	oauthToken    = "ezgh_at_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"
	deniedToken   = "ezgh_at_9999999999abcdefghijABCDEFGHIJklmnopqrstuvw"
	ssoToken      = "ezgh_sat_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"
	oauthIdentity = `{"type":"User","principalId":"u1","userName":"ada@acme.test","isRoot":false,"accessKeyId":null,"sessionContext":{"credential":"oauth","sessionIssuedAt":"2026-09-29T08:00:00.000Z","ssoProviderId":null,"mfaAuthenticated":false,"clientId":"ezgh-cli"},"invokedBy":null}`
	goodOrigin    = "https://console.ezghcloud.com"
)

// fakeOrgAPI answers the permissions endpoint by who the cookie (or key) says the caller is.
type fakeOrgAPI struct {
	mu        sync.Mutex
	calls     int
	forwarded []http.Header
	queries   []url.Values
	// directory, when set, answers the directory lookup.
	directory http.Handler
}

func (f *fakeOrgAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.directory != nil && r.URL.Path == "/internal/v1/directory/lookup" {
		f.directory.ServeHTTP(w, r)
		return
	}
	f.mu.Lock()
	f.calls++
	f.forwarded = append(f.forwarded, r.Header.Clone())
	f.queries = append(f.queries, r.URL.Query())
	f.mu.Unlock()
	who := "member"
	if c, err := r.Cookie("better-auth.session_token"); err == nil {
		who = c.Value
	}
	switch who {
	case "outsider":
		w.WriteHeader(404)
		return
	case "expired":
		w.WriteHeader(401)
		return
	case "down":
		w.WriteHeader(502)
		return
	}
	allowed := strings.Split(r.URL.Query().Get("actions"), ",")
	if who == "denied" || r.Header.Get("Authorization") == "Bearer "+deniedToken {
		allowed = []string{}
	}
	var apiKeyID any
	userIdentity := json.RawMessage(identity)
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ezgh_at_") {
		userIdentity = json.RawMessage(oauthIdentity)
	} else if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ezgh_") {
		apiKeyID = "k1"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"organization":     map[string]any{"id": orgID, "name": "Acme", "slug": orgSlug, "resourceName": "ezgh::" + orgSlug, "isRoot": false, "createdAt": "2026-09-01T08:00:00.000Z", "updatedAt": "2026-09-01T08:00:00.000Z"},
		"principal":        map[string]any{"type": "user", "id": "u1"},
		"apiKeyId":         apiKeyID,
		"userIdentity":     userIdentity,
		"resource":         r.URL.Query().Get("resource"),
		"allowed":          allowed,
		"allowedSomewhere": []string{},
		"somethingNew":     true,
	})
}

func testManifest() *manifest.Manifest {
	return &manifest.Manifest{
		ManifestVersion: 1, Namespace: "widgets", DisplayName: "Widgets", Description: "x",
		API: manifest.API{Hostname: "widgets.ezghcloud.com", Port: 4700},
		Endpoints: []manifest.Endpoint{
			{Method: "GET", Path: "/health", Auth: "none"},
			{Method: "GET", Path: "/ready", Auth: "none"},
			{Method: "GET", Path: "/v1/organizations/:orgId/things", Auth: "caller", Action: "widgets.things.list", Trail: &manifest.Trail{EventName: "ListThings"}},
			{Method: "POST", Path: "/v1/organizations/:orgId/things", Auth: "caller", Action: "widgets.things.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CreateThing", Sensitive: []string{"seed"}}},
			{Method: "GET", Path: "/internal/v1/things", Auth: "service"},
			{Method: "GET", Path: "/openapi.json", Auth: "none"},
		},
	}
}

// stubOverlay is an openapi.overlay.json covering every customer endpoint with an empty 200.
func stubOverlay(m *manifest.Manifest) []byte {
	paths := map[string]map[string]any{}
	for _, e := range m.Endpoints {
		if !openapi.Customer(e) {
			continue
		}
		path := openapi.ToOpenAPIPath(e.Path)
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		op := map[string]any{"responses": map[string]any{"200": map[string]any{"description": "OK"}}}
		if e.Trail == nil {
			op["operationId"] = strings.ReplaceAll(strings.Trim(e.Method+e.Path, "/"), "/", "_")
		}
		paths[path][strings.ToLower(e.Method)] = op
	}
	b, _ := json.Marshal(map[string]any{"info": map[string]any{"title": "Widgets API", "version": "1.0.0"}, "paths": paths})
	return b
}

type harness struct {
	db      *testdb.DB
	org     *fakeOrgAPI
	server  *httptest.Server
	creates atomic.Int64
}

func newHarness(t *testing.T) *harness { return newPreparedHarness(t, nil) }

func newPreparedHarness(t *testing.T, prepare func() error) *harness {
	return newHarnessWith(t, prepare, nil, nil)
}

// newHarnessWith is newPreparedHarness with extra endpoints appended to testManifest's, and
// register (when not nil) adding their handlers after the built-in ones.
func newHarnessWith(t *testing.T, prepare func() error, extra []manifest.Endpoint, register func(*service.Service)) *harness {
	return newHarnessSized(t, 0, prepare, extra, register)
}

// newHarnessSized is newHarnessWith whose service uses a pool of at most maxConns connections
// (0: the test database's own pool).
func newHarnessSized(t *testing.T, maxConns int32, prepare func() error, extra []manifest.Endpoint, register func(*service.Service)) *harness {
	h := &harness{db: testdb.New(t), org: &fakeOrgAPI{}}
	pool := h.db.DB
	if maxConns > 0 {
		var err error
		if pool, err = kitdb.Open(context.Background(), h.db.URL, kitdb.PoolOptions{MaxConns: maxConns}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
	}
	orgServer := httptest.NewServer(h.org)
	t.Cleanup(orgServer.Close)
	m := testManifest()
	m.Endpoints = append(m.Endpoints, extra...)
	s := service.New(service.Options{
		Manifest: m, DB: pool, OrgAPI: &authz.Client{BaseURL: orgServer.URL, InternalToken: "t", PublicBaseURL: "https://widgets.ezghcloud.com"},
		Cursors: cursor.NewSigner(strings.Repeat("k", 32)), Origins: httpapi.Origins{List: []string{goodOrigin}},
		Logger: testdb.Logger(), InternalToken: "internal-secret-token", OpenAPIOverlay: stubOverlay(m),
	})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		c.JSON(200, map[string]any{"things": []any{}, "nextCursor": nil})
		return nil
	})
	s.Handle("POST", "/v1/organizations/:orgId/things", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		body, bad := httpapi.DecodeObject(c.Body(), false)
		if bad != nil {
			return bad
		}
		f := httpapi.NewFields(body)
		name, _ := f.String("name", true)
		f.String("seed", false)
		if err := f.Finish(); err != nil {
			return err
		}
		return c.TransactPrepared(prepare, func(tx *kitdb.Tx) (*service.Response, error) {
			h.creates.Add(1)
			id := ids.NewV7()
			return &service.Response{ETag: `"test-tag"`, Status: 201, Body: map[string]any{"id": id, "name": name, "token": "never-recorded"}, Location: "/v1/organizations/" + orgID + "/things/" + id}, nil
		})
	})
	s.Handle("GET", "/internal/v1/things", func(c *service.Call) error {
		c.JSON(200, map[string]string{"caller": c.ServiceCaller})
		return nil
	})
	if register != nil {
		register(s)
	}
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(handler)
	t.Cleanup(h.server.Close)
	return h
}

type result struct {
	status int
	header http.Header
	body   map[string]any
}

func (h *harness) do(t *testing.T, method, path, body string, headers map[string]string) result {
	t.Helper()
	req, _ := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	_ = json.Unmarshal(raw, &parsed)
	return result{status: resp.StatusCode, header: resp.Header, body: parsed}
}

func code(r result) string {
	if e, ok := r.body["error"].(map[string]any); ok {
		return e["code"].(string)
	}
	return ""
}

type recorded struct {
	EventName    string          `json:"eventName"`
	ErrorCode    *string         `json:"errorCode"`
	UserIdentity json.RawMessage `json:"userIdentity"`
	Request      map[string]any  `json:"requestParameters"`
	Response     map[string]any  `json:"responseElements"`
	RequestID    string          `json:"requestId"`
	SourceIP     *string         `json:"sourceIpAddress"`
	ReadOnly     bool            `json:"readOnly"`
	Resources    []map[string]string
}

func (h *harness) events(t *testing.T) []recorded {
	t.Helper()
	rows, err := h.db.Pool.Query(context.Background(), `SELECT event::text FROM audit_outbox ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	texts, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	var out []recorded
	for _, text := range texts {
		var e recorded
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

var (
	member = map[string]string{"Cookie": "better-auth.session_token=member; other=secret", "Origin": goodOrigin}
	things = "/v1/organizations/" + orgID + "/things"
)

func TestCatalogMustMatchRoutes(t *testing.T) {
	db := testdb.New(t)
	s := service.New(service.Options{Manifest: testManifest(), DB: db.DB, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(testManifest())})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("GET", "/v1/organizations/:orgId/unlisted", func(*service.Call) error { return nil })
	_, err := s.Handler()
	if err == nil || !strings.Contains(err.Error(), "POST /v1/organizations/:orgId/things is in the manifest but has no handler") || !strings.Contains(err.Error(), "unlisted has a handler but no manifest entry") {
		t.Fatalf("got %v", err)
	}
}

func TestRefusalsBeforeAuthorizationAreNotRecorded(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name, method, path string
		headers            map[string]string
		status             int
		code               string
	}{
		{"no credential", "GET", things, nil, 401, "unauthenticated"},
		{"only other cookies", "GET", things, map[string]string{"Cookie": "theme=dark"}, 401, "unauthenticated"},
		{"a service credential on a public route", "GET", things, map[string]string{"Authorization": "Bearer pk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}, 401, "unauthenticated"},
		{"cookie write without Origin", "POST", things, map[string]string{"Cookie": "better-auth.session_token=member"}, 403, "forbidden_origin"},
		{"cookie write from another origin", "POST", things, map[string]string{"Cookie": "better-auth.session_token=member", "Origin": "https://evil.ezghcloud.com"}, 403, "forbidden_origin"},
		{"malformed organization id", "GET", "/v1/organizations/not-a-uuid/things", member, 404, "not_found"},
		{"not a member", "GET", things, map[string]string{"Cookie": "better-auth.session_token=outsider"}, 404, "not_found"},
		{"expired session", "GET", things, map[string]string{"Cookie": "better-auth.session_token=expired"}, 401, "unauthenticated"},
	}
	for _, c := range cases {
		r := h.do(t, c.method, c.path, `{"name":"x"}`, c.headers)
		if r.status != c.status || code(r) != c.code {
			t.Errorf("%s: %d %s", c.name, r.status, code(r))
		}
		if r.header.Get("X-Request-Id") == "" {
			t.Errorf("%s: no X-Request-Id", c.name)
		}
	}
	if e := h.events(t); len(e) != 0 {
		t.Errorf("recorded %d events", len(e))
	}
	// The caller's X-Request-Id comes back on the router's own answers too: a CORS preflight, a
	// method the route doesn't have, and an unknown path.
	for _, c := range []struct{ method, path string }{{"OPTIONS", things}, {"PUT", things}, {"GET", "/nope"}} {
		r := h.do(t, c.method, c.path, "", map[string]string{"X-Request-Id": "req-router", "Origin": goodOrigin, "Access-Control-Request-Method": "POST"})
		if r.header.Get("X-Request-Id") != "req-router" {
			t.Errorf("%s %s: %d, X-Request-Id %q", c.method, c.path, r.status, r.header.Get("X-Request-Id"))
		}
	}
	// Malformed IDs never reach org-api.
	calls := h.org.calls
	h.do(t, "GET", "/v1/organizations/NOT-A-UUID/things", "", member)
	if h.org.calls != calls {
		t.Error("asked org-api about a malformed ID")
	}
}

func TestOrgAPIDownIs503(t *testing.T) {
	h := newHarness(t)
	r := h.do(t, "GET", things, "", map[string]string{"Cookie": "better-auth.session_token=down"})
	if r.status != 503 || code(r) != "unavailable" || r.header.Get("Retry-After") == "" {
		t.Errorf("%d %s %q", r.status, code(r), r.header.Get("Retry-After"))
	}
}

func TestDeniedIsRecorded(t *testing.T) {
	h := newHarness(t)
	r := h.do(t, "POST", things, `{"name":"x"}`, map[string]string{"Cookie": "better-auth.session_token=denied", "Origin": goodOrigin})
	if r.status != 403 || code(r) != "access_denied" || r.body["error"].(map[string]any)["message"] != "You're not allowed to widgets.things.create" {
		t.Fatalf("%d %v", r.status, r.body)
	}
	ev := h.events(t)
	if len(ev) != 1 || ev[0].EventName != "CreateThing" || *ev[0].ErrorCode != "access_denied" || ev[0].Response != nil {
		t.Fatalf("events %+v", ev)
	}
	// jsonb normalizes key order and spacing; the value must be org-api's, unchanged.
	got, _ := canonical.Decode(ev[0].UserIdentity)
	want, _ := canonical.Decode([]byte(identity))
	if string(canonical.MustMarshal(got)) != string(canonical.MustMarshal(want)) {
		t.Errorf("userIdentity changed: %s", ev[0].UserIdentity)
	}
}

// Without SetRequestParameters an event keeps only the body's field names, and nothing from the
// body of a refused call (an unknown field included): no value the handler didn't choose.
func TestBodyValuesAreNotRecordedByDefault(t *testing.T) {
	h := newHarness(t)
	body := `{"name":"build-runner","privateKey":"LEAK-private-key","private_key":"LEAK-private_key","keyPem":"LEAK-keyPem","key_pem":"LEAK-key_pem",` +
		`"passphrase":"LEAK-passphrase","nested":{"privateKey":"LEAK-nested","inner":{"value":"LEAK-inner"}},"list":[{"passphrase":"LEAK-list"},"LEAK-item"]}`
	r := h.do(t, "POST", things, body, member)
	if r.status != 400 || code(r) != "invalid_request" {
		t.Fatalf("%d %v", r.status, r.body)
	}
	if r := h.do(t, "POST", things, `{"name":"build-runner","seed":"LEAK-seed"}`, member); r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	ev := h.events(t)
	if len(ev) != 2 || ev[0].ErrorCode == nil || ev[1].ErrorCode != nil {
		t.Fatalf("events %+v", ev)
	}
	if len(ev[0].Request) != 1 || ev[0].Request["orgId"] != orgID {
		t.Errorf("refused call's parameters %v", ev[0].Request)
	}
	if len(ev[1].Request) != 3 || ev[1].Request["orgId"] != orgID || ev[1].Request["name"] != trails.Hidden || ev[1].Request["seed"] != trails.Hidden {
		t.Errorf("parameters %v", ev[1].Request)
	}
	var all string
	if err := h.db.Pool.QueryRow(context.Background(), "SELECT string_agg(event::text, '') FROM audit_outbox").Scan(&all); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(all, "LEAK-") {
		t.Errorf("a body value was recorded: %s", all)
	}
}

func TestSuccessfulWriteIsRecordedInItsTransaction(t *testing.T) {
	h := newHarness(t)
	r := h.do(t, "POST", things, `{"name":"build-runner","seed":"s3cret"}`, map[string]string{"Cookie": "better-auth.session_token=member; theme=dark", "Origin": goodOrigin, "X-Request-Id": "req-123", "X-Forwarded-For": "198.51.100.7, 203.0.113.9"})
	if r.status != 201 || r.header.Get("Location") == "" || r.header.Get("X-Request-Id") != "req-123" {
		t.Fatalf("%d %v", r.status, r.body)
	}
	ev := h.events(t)
	if len(ev) != 1 {
		t.Fatalf("%d events", len(ev))
	}
	e := ev[0]
	if e.ErrorCode != nil || e.RequestID != "req-123" || e.Request["seed"] != trails.Hidden || e.Request["orgId"] != orgID || e.Request["name"] != trails.Hidden {
		t.Errorf("request parameters %+v", e)
	}
	if _, leaked := e.Response["token"]; leaked || e.Response["name"] != "build-runner" || e.Response["id"] == nil {
		t.Errorf("response elements %v", e.Response)
	}
	if len(e.Resources) != 1 || e.Resources[0]["type"] != "Organization" {
		t.Errorf("resources %v", e.Resources)
	}
	// The client is the rightmost X-Forwarded-For entry, the Gateway's.
	if e.SourceIP == nil || *e.SourceIP != "203.0.113.9" {
		t.Errorf("sourceIpAddress %v", e.SourceIP)
	}
	// Only better-auth's cookie reached org-api, with the client's X-Forwarded-For, unchanged.
	if got := h.org.forwarded[len(h.org.forwarded)-1].Get("Cookie"); got != "better-auth.session_token=member" {
		t.Errorf("forwarded Cookie %q", got)
	}
	if got := h.org.forwarded[len(h.org.forwarded)-1].Get("X-Forwarded-For"); got != "198.51.100.7, 203.0.113.9" {
		t.Errorf("forwarded X-Forwarded-For %q", got)
	}
}

func TestAPIKeyWinsAndSkipsTheOriginCheck(t *testing.T) {
	h := newHarness(t)
	r := h.do(t, "POST", things, `{"name":"x"}`, map[string]string{"Authorization": "Bearer " + apiKey, "Cookie": "better-auth.session_token=outsider"})
	if r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	fwd := h.org.forwarded[len(h.org.forwarded)-1]
	if fwd.Get("Cookie") != "" || fwd.Get("Authorization") != "Bearer "+apiKey || fwd.Get("X-Internal-Token") != "" {
		t.Errorf("forwarded %v", fwd)
	}
}

// An OAuth access token is forwarded exactly like an API key (alone, winning over the cookie,
// skipping the Origin check), its refusals speak to the person, and Trails records org-api's
// oauth identity as it is.
func TestOAuthAccessTokenActsAsItsUser(t *testing.T) {
	h := newHarness(t)
	r := h.do(t, "POST", things, `{"name":"x"}`, map[string]string{"Authorization": "Bearer " + oauthToken, "Cookie": "better-auth.session_token=outsider"})
	if r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	fwd := h.org.forwarded[len(h.org.forwarded)-1]
	if fwd.Get("Cookie") != "" || fwd.Get("Authorization") != "Bearer "+oauthToken || fwd.Get("X-Internal-Token") != "" {
		t.Errorf("forwarded %v", fwd)
	}
	ev := h.events(t)
	var got, want any
	_ = json.Unmarshal(ev[len(ev)-1].UserIdentity, &got)
	_ = json.Unmarshal([]byte(oauthIdentity), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("userIdentity %s", ev[len(ev)-1].UserIdentity)
	}

	r = h.do(t, "GET", things, "", map[string]string{"Authorization": "Bearer " + deniedToken})
	if r.status != 403 || r.body["error"].(map[string]any)["message"] != "You're not allowed to widgets.things.list" {
		t.Errorf("%d %v", r.status, r.body)
	}
}

// An EZGH SSO resource token is forwarded alone, with requestUrl: the configured base URL plus the
// raw path as received (not decoded, no query), whatever Host, X-Forwarded-* or an absolute-form
// request-target say. No other credential sends requestUrl.
func TestSSOResourceTokenSendsRequestURL(t *testing.T) {
	h := newHarness(t)
	addr := strings.TrimPrefix(h.server.URL, "http://")
	for target, want := range map[string]string{
		things + "?limit=5":                             "https://widgets.ezghcloud.com" + things,
		"https://evil.example:8443" + things:            "https://widgets.ezghcloud.com" + things,
		"/v1/organizations/%30" + orgID[1:] + "/things": "https://widgets.ezghcloud.com/v1/organizations/%30" + orgID[1:] + "/things",
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "GET "+target+" HTTP/1.1\r\nHost: evil.example\r\nX-Forwarded-Host: evil.example\r\nX-Forwarded-Proto: http\r\n"+
			"Authorization: Bearer "+ssoToken+"\r\nCookie: better-auth.session_token=outsider\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		_ = conn.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", target, resp.StatusCode)
		}
		q, fwd := h.org.queries[len(h.org.queries)-1], h.org.forwarded[len(h.org.forwarded)-1]
		if q.Get("requestUrl") != want || fwd.Get("Authorization") != "Bearer "+ssoToken || fwd.Get("Cookie") != "" {
			t.Errorf("%s: sent requestUrl %q with %v, want %q", target, q.Get("requestUrl"), fwd, want)
		}
	}
	for _, auth := range []string{"Bearer " + apiKey, "Bearer " + oauthToken} {
		if r := h.do(t, "GET", things, "", map[string]string{"Authorization": auth}); r.status != 200 || h.org.queries[len(h.org.queries)-1].Has("requestUrl") {
			t.Errorf("%s: %d, sent %v", auth, r.status, h.org.queries[len(h.org.queries)-1])
		}
	}
	if r := h.do(t, "GET", things, "", map[string]string{"Cookie": "better-auth.session_token=member"}); r.status != 200 || h.org.queries[len(h.org.queries)-1].Has("requestUrl") {
		t.Errorf("cookie: %d, sent %v", r.status, h.org.queries[len(h.org.queries)-1])
	}
}

// A refresh token (or anything else in Authorization) is 401 without asking org-api, and never the
// cookie instead.
func TestOtherAuthorizationHeadersAre401WithoutAsking(t *testing.T) {
	h := newHarness(t)
	for _, auth := range []string{"Bearer ezgh_rt_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw", "Bearer ezgh_at_short", "Basic abc"} {
		r := h.do(t, "GET", things, "", map[string]string{"Authorization": auth, "Cookie": "better-auth.session_token=member"})
		if r.status != 401 || code(r) != "unauthenticated" {
			t.Errorf("%s: %d %v", auth, r.status, r.body)
		}
	}
	if h.org.calls != 0 {
		t.Errorf("org-api was asked %d times", h.org.calls)
	}
}

func TestValidationFailureFromAMemberIsRecorded(t *testing.T) {
	h := newHarness(t)
	r := h.do(t, "POST", things, `{"nme":"x"}`, member)
	if r.status != 400 || code(r) != "invalid_request" {
		t.Fatalf("%d %v", r.status, r.body)
	}
	issues := r.body["error"].(map[string]any)["issues"].([]any)
	if len(issues) != 2 {
		t.Errorf("issues %v", issues)
	}
	if ev := h.events(t); len(ev) != 1 || *ev[0].ErrorCode != "invalid_request" {
		t.Errorf("events %+v", ev)
	}
}

func TestReadsAreRecordedReadOnly(t *testing.T) {
	h := newHarness(t)
	if r := h.do(t, "GET", things, "", member); r.status != 200 {
		t.Fatal(r.status)
	}
	ev := h.events(t)
	if len(ev) != 1 || ev[0].EventName != "ListThings" || !ev[0].ReadOnly || ev[0].Response != nil {
		t.Errorf("events %+v", ev)
	}
}

func TestIdempotencyKeys(t *testing.T) {
	h := newHarness(t)
	headers := map[string]string{"Cookie": "better-auth.session_token=member", "Origin": goodOrigin, "Idempotency-Key": "k-1"}
	first := h.do(t, "POST", things, `{"name":"a"}`, headers)
	again := h.do(t, "POST", things, `{"name":"a"}`, headers)
	if first.status != 201 || again.status != 201 || again.header.Get("Idempotent-Replayed") != "true" || first.body["id"] != again.body["id"] || again.header.Get("Location") != first.header.Get("Location") {
		t.Fatalf("first %v, again %v %v", first.body, again.body, again.header)
	}
	if h.creates.Load() != 1 {
		t.Errorf("created %d times", h.creates.Load())
	}
	conflict := h.do(t, "POST", things, `{"name":"b"}`, headers)
	if conflict.status != 409 || code(conflict) != "idempotency_conflict" {
		t.Errorf("%d %v", conflict.status, conflict.body)
	}
	bad := h.do(t, "POST", things, `{"name":"b"}`, map[string]string{"Cookie": "better-auth.session_token=member", "Origin": goodOrigin, "Idempotency-Key": "has space"})
	if bad.status != 400 {
		t.Errorf("bad key: %d", bad.status)
	}
	// Each call, replayed or not, is its own event.
	if ev := h.events(t); len(ev) != 4 {
		t.Errorf("%d events", len(ev))
	}
}

func TestInternalRoutes(t *testing.T) {
	h := newHarness(t)
	if r := h.do(t, "GET", "/internal/v1/things", "", map[string]string{"Cookie": "better-auth.session_token=member"}); r.status != 401 {
		t.Errorf("a customer credential on /internal: %d", r.status)
	}
	if r := h.do(t, "GET", "/internal/v1/things", "", map[string]string{"X-Internal-Token": "wrong"}); r.status != 401 {
		t.Errorf("wrong token: %d", r.status)
	}
	if r := h.do(t, "GET", "/internal/v1/things", "", map[string]string{"X-Internal-Token": "internal-secret-token"}); r.status != 200 {
		t.Errorf("right token: %d", r.status)
	}
	if ev := h.events(t); len(ev) != 0 {
		t.Error("an internal call was recorded")
	}
}

// GET /openapi.json: the customer endpoints' document, unauthenticated, never recorded; and a
// service whose overlay doesn't describe exactly its customer endpoints refuses to start.
func TestOpenAPIDocument(t *testing.T) {
	h := newHarness(t)
	r := h.do(t, "GET", "/openapi.json", "", nil)
	if r.status != 200 || r.body["openapi"] != "3.1.0" || !strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %v", r.status, r.header)
	}
	raw, _ := json.Marshal(r.body)
	ops, err := openapi.Operations(raw)
	if want := []string{"GET /v1/organizations/:orgId/things", "POST /v1/organizations/:orgId/things"}; err != nil || !slices.Equal(ops, want) {
		t.Fatalf("operations %v, want %v (%v)", ops, want, err)
	}
	if ev := h.events(t); len(ev) != 0 {
		t.Error("the document was recorded")
	}

	db := testdb.New(t)
	m := testManifest()
	missing := *m
	missing.Endpoints = append([]manifest.Endpoint{}, m.Endpoints...)
	missing.Endpoints = append(missing.Endpoints, manifest.Endpoint{Method: "DELETE", Path: "/v1/organizations/:orgId/things/:thingId", Auth: "caller", Action: "widgets.things.create", Trail: &manifest.Trail{EventName: "DeleteThing"}})
	s := service.New(service.Options{Manifest: &missing, DB: db.DB, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("DELETE", "/v1/organizations/:orgId/things/:thingId", func(*service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
	if _, err := s.Handler(); err == nil || !strings.Contains(err.Error(), "DELETE /v1/organizations/:orgId/things/:thingId: a customer endpoint with no operation in the overlay") {
		t.Fatalf("got %v", err)
	}
	s = service.New(service.Options{Manifest: m, DB: db.DB, Logger: testdb.Logger()})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
	if _, err := s.Handler(); err == nil || !strings.Contains(err.Error(), "the overlay isn't a JSON object") {
		t.Fatalf("no overlay: %v", err)
	}
}

func TestHealthReadyAndCORS(t *testing.T) {
	h := newHarness(t)
	if r := h.do(t, "GET", "/health", "", nil); r.status != 200 || r.body["status"] != "ok" {
		t.Errorf("health %d", r.status)
	}
	if r := h.do(t, "GET", "/ready", "", nil); r.status != 200 {
		t.Errorf("ready %d", r.status)
	}
	r := h.do(t, "OPTIONS", things, "", map[string]string{"Origin": goodOrigin, "Access-Control-Request-Method": "POST"})
	if r.status != 204 || r.header.Get("Access-Control-Allow-Origin") != goodOrigin || r.header.Get("Access-Control-Allow-Credentials") != "true" || !strings.Contains(r.header.Get("Access-Control-Allow-Headers"), "Idempotency-Key") {
		t.Errorf("preflight %d %v", r.status, r.header)
	}
	r = h.do(t, "OPTIONS", things, "", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	if r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("allowed another origin")
	}
	if r := h.do(t, "GET", "/nope", "", nil); r.status != 404 || code(r) != "not_found" {
		t.Errorf("unknown route %d", r.status)
	}
}

func TestPreparedReplaySkipsExternalCheck(t *testing.T) {
	var calls atomic.Int64
	h := newPreparedHarness(t, func() error { calls.Add(1); return nil })
	headers := map[string]string{"Cookie": "better-auth.session_token=member", "Origin": goodOrigin, "Idempotency-Key": "prepared"}
	path := "/v1/organizations/" + orgID + "/things"
	first := h.do(t, "POST", path, `{"name":"one"}`, headers)
	second := h.do(t, "POST", path, `{"name":"one"}`, headers)
	if first.status != 201 || second.status != 201 || second.header.Get("Idempotent-Replayed") != "true" || calls.Load() != 1 || second.header.Get("ETag") != `"test-tag"` {
		t.Fatalf("first=%+v second=%+v calls=%d", first, second, calls.Load())
	}
}
