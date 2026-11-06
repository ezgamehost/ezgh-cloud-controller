package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/credential"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/delegation"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// Delegated calls, the receiver's half (contracts/service-auth.md, "Delegated calls") and the
// edge's reference (Call.Delegation).

const (
	delegatedIdentity = `{"type":"User","principalId":"u1","userName":"ada@acme.test","isRoot":false,"accessKeyId":null,"sessionContext":{"credential":"session","sessionIssuedAt":null,"ssoProviderId":null,"mfaAuthenticated":false},"invokedBy":"compute.ezghcloud.com"}`
	thingName         = "ezgh:us-west-1:" + orgSlug + ":prj_g7h8i9j0k1l2:thg_4k2m9x0a7qpe"
	session           = "Qm7cXw2KpL9sTn4vBd8yRf3h"
)

func reference(chain string) string {
	return "v=1;org=" + orgID + ";principal=user:u1;credential=session:" + session + ";at=" + strconv.FormatInt(time.Now().Unix(), 10) + ";chain=" + chain
}

// fakeDelegated is org-api's delegated check, batch check and directory lookup.
type fakeDelegated struct {
	mu        sync.Mutex
	delegated []http.Header
	bodies    []map[string]any
	// status and code: org-api's refusal; results: each check's allowed (nil: null), default true.
	status     int
	code       string
	results    []*bool
	credential bool // the batch answer names a credential
	// answers, when set, answer the delegated checks in turn (then the fields above).
	answers []fakeAnswer
	// orgID and invokedBy, when set, replace the delegated answer's.
	orgID, invokedBy string
}

type fakeAnswer struct {
	Status  int     `json:"status"`
	Code    string  `json:"code"`
	Results []*bool `json:"results"`
}

func (f *fakeDelegated) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	org := map[string]any{"id": orgID, "name": "Acme", "slug": orgSlug, "resourceName": "ezgh::" + orgSlug, "isRoot": false}
	switch r.URL.Path {
	case "/internal/v1/directory/lookup":
		orgs, projects := []any{map[string]any{"id": orgID, "slug": orgSlug, "state": "active"}}, []any{}
		if slugs, ok := body["projectSlugs"].([]any); ok {
			// A lookup by prefixed project ID (resolvePathIDs) names no organization.
			orgs = []any{}
			if len(slugs) == 1 && slugs[0] == pathProjectSlug {
				projects = append(projects, map[string]any{"id": pathProjectID, "organizationId": orgID, "slug": pathProjectSlug, "deletedAt": nil})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"organizations": orgs, "projects": projects})
		return
	case "/internal/v1/permissions/batch":
		var results []map[string]any
		for _, c := range body["checks"].([]any) {
			check := c.(map[string]any)
			results = append(results, map[string]any{"action": check["action"], "resource": check["resource"], "allowed": true})
		}
		answer := map[string]any{"organization": org, "principal": map[string]any{"type": "user", "id": "u1"}, "apiKeyId": nil, "userIdentity": json.RawMessage(identity), "results": results}
		if f.credential {
			answer["credential"] = map[string]any{"kind": "session", "id": session}
		}
		_ = json.NewEncoder(w).Encode(answer)
		return
	case "/internal/v1/permissions/delegated":
	default:
		w.WriteHeader(404)
		return
	}
	f.mu.Lock()
	f.delegated = append(f.delegated, r.Header.Clone())
	f.bodies = append(f.bodies, body)
	if len(f.answers) > 0 {
		f.status, f.code, f.results = f.answers[0].Status, f.answers[0].Code, f.answers[0].Results
		f.answers = f.answers[1:]
	}
	f.mu.Unlock()
	if f.status != 0 && f.status != 200 {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(f.status)
		issues := []any{}
		if f.status == 400 {
			issues = append(issues, map[string]any{"path": "reference", "message": "isn't canonical"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": f.code, "message": "x", "issues": issues}})
		return
	}
	var results []map[string]any
	for i, c := range body["checks"].([]any) {
		check := c.(map[string]any)
		result := map[string]any{"action": check["action"], "resource": check["resource"], "allowed": true}
		if i < len(f.results) {
			if f.results[i] == nil {
				result["allowed"] = nil
				result["error"] = map[string]any{"code": "not_delegable", "message": "x"}
			} else {
				result["allowed"] = *f.results[i]
			}
		}
		results = append(results, result)
	}
	if f.orgID != "" {
		org["id"] = f.orgID
	}
	invokedBy := fmt.Sprint(body["caller"], ".ezghcloud.com")
	if f.invokedBy != "" {
		invokedBy = f.invokedBy
	}
	userIdentity := json.RawMessage(strings.Replace(delegatedIdentity, "compute.ezghcloud.com", invokedBy, 1))
	_ = json.NewEncoder(w).Encode(map[string]any{
		"organization": org, "principal": map[string]any{"type": "user", "id": "u1"}, "apiKeyId": nil,
		"userIdentity": userIdentity, "results": results,
		"credential": map[string]any{"kind": "session", "id": session},
	})
}

func (f *fakeDelegated) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.delegated)
}

type delegatedHarness struct {
	db                        *testdb.DB
	org                       *fakeDelegated
	server                    *httptest.Server
	compute, network, unbound string
	outbound                  []string
	blindCaller               string
	blindCommits              int
}

func newDelegatedHarness(t *testing.T) *delegatedHarness {
	h := &delegatedHarness{db: testdb.New(t), org: &fakeDelegated{}}
	orgServer := httptest.NewServer(h.org)
	t.Cleanup(orgServer.Close)
	store := &credential.Store{DB: h.db.DB, Prefix: "wk"}
	issue := func(caller, namespace string) string {
		issued, err := store.IssueFor(context.Background(), credential.IssueRequest{Caller: caller, Namespace: namespace, CreatedBy: "test", Days: 1})
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + issued.Token
	}
	h.compute, h.network, h.unbound = issue("compute-api", "compute"), issue("network-api", "network"), issue("billing-worker", "")
	m := testManifest()
	m.Delegation = &manifest.Delegation{Callers: []manifest.DelegationCaller{
		{Namespace: "compute", Actions: []string{"widgets.things.*"}},
		{Namespace: "network", Actions: []string{"widgets.things.create"}, Origins: []string{"compute"}},
	}}
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "POST", Path: "/internal/v1/projects/:projectId/things", Auth: manifest.AuthDelegated, Action: "widgets.things.create", Trail: &manifest.Trail{EventName: "CreateThing"}},
		manifest.Endpoint{Method: "GET", Path: "/internal/v1/things/count", Auth: manifest.AuthDelegated, Action: "widgets.things.list", Trail: &manifest.Trail{EventName: "ListThings"}},
		manifest.Endpoint{Method: "POST", Path: "/internal/v1/blind", Auth: manifest.AuthDelegated, Action: "widgets.things.create", Trail: &manifest.Trail{EventName: "CreateBlindThing"}},
		manifest.Endpoint{Method: "GET", Path: "/internal/v1/blind", Auth: manifest.AuthDelegated, Action: "widgets.things.list", Trail: &manifest.Trail{EventName: "ListBlindThings"}},
		manifest.Endpoint{Method: "POST", Path: "/internal/v1/twice", Auth: manifest.AuthDelegated, Action: "widgets.things.create", Trail: &manifest.Trail{EventName: "CreateTwice"}},
		manifest.Endpoint{Method: "POST", Path: "/v1/organizations/:orgId/launches", Auth: "caller", Action: "widgets.launches.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CreateLaunch"}},
	)
	s := service.New(service.Options{
		Manifest: m, DB: h.db.DB, OrgAPI: &authz.Client{BaseURL: orgServer.URL, InternalToken: "t", PublicBaseURL: "https://widgets.ezghcloud.com", ServiceCredential: okCredential},
		Logger: testdb.Logger(), InternalToken: "internal-secret-token", OpenAPIOverlay: stubOverlay(m), Credentials: store,
	})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
	s.Handle("POST", "/internal/v1/projects/:projectId/things", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, thingName); err != nil {
			return err
		}
		if out, err := c.Delegate(); err == nil {
			h.outbound = append(h.outbound, out.Reference.String())
		} else {
			h.outbound = append(h.outbound, "refused: "+httpapi.ErrorOf(err).Code)
		}
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			return &service.Response{Status: 201, Body: map[string]any{"id": "0199a3d0-2b3c-7d4e-9f50-6b7c8d9e0f1a", "resourceName": thingName}}, nil
		})
	})
	// Blind trust, the handler's bug: it answers without asking org-api.
	s.Handle("POST", "/internal/v1/blind", func(c *service.Call) error {
		h.blindCaller = c.ServiceCaller
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			h.blindCommits++
			return &service.Response{Status: 201, Body: map[string]any{"id": "x"}}, nil
		})
	})
	s.Handle("GET", "/internal/v1/blind", func(c *service.Call) error {
		c.JSON(200, map[string]any{"things": []any{}})
		return nil
	})
	s.Handle("POST", "/internal/v1/twice", func(c *service.Call) error {
		for range 2 {
			if err := c.Authorize(c.Endpoint.Action, thingName); err != nil {
				return err
			}
		}
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			return &service.Response{Status: 201, Body: map[string]any{"id": "x"}}, nil
		})
	})
	s.Handle("GET", "/internal/v1/things/count", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		c.JSON(200, map[string]any{"count": 0, "principal": c.Principal().ID, "caller": c.Delegated().Caller()})
		return nil
	})
	s.Handle("POST", "/v1/organizations/:orgId/launches", func(c *service.Call) error {
		if _, err := c.Delegation(); err == nil {
			return httpapi.Internal(nil) // before CheckBatch it must refuse
		}
		if _, err := c.CheckBatch([]authz.BatchCheck{{Action: "widgets.launches.create", Resource: "ezgh::" + orgSlug}}); err != nil {
			return err
		}
		out, err := c.Delegation()
		if err != nil {
			return err
		}
		if _, err := c.Delegate(); err == nil {
			return httpapi.Internal(nil) // an edge never relays
		}
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			return &service.Response{Status: 201, Body: map[string]any{"reference": out.Reference.String(), "forwardedFor": out.ForwardedFor, "requestId": out.RequestID, "received": c.Received.Unix()}}, nil
		})
	})
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(handler)
	t.Cleanup(h.server.Close)
	return h
}

func (h *delegatedHarness) do(t *testing.T, method, path string, headers http.Header) result {
	t.Helper()
	req, _ := http.NewRequest(method, h.server.URL+path, strings.NewReader(`{"name":"x"}`))
	req.Header = headers
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

func (h *delegatedHarness) events(t *testing.T) []map[string]any {
	t.Helper()
	rows, err := h.db.Pool.Query(context.Background(), `SELECT event::text FROM audit_outbox ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	texts, _ := pgx.CollectRows(rows, pgx.RowTo[string])
	var out []map[string]any
	for _, text := range texts {
		var e map[string]any
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	return h
}

const createPath = "/internal/v1/projects/0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81/things"

// The receiver's checks before org-api, in order: its own credential (401), then the reference
// (400), then the credential's namespace and the allowlist (403); none asks org-api or records.
func TestDelegatedPrepare(t *testing.T) {
	h := newDelegatedHarness(t)
	cases := []struct {
		name   string
		header http.Header
		status int
		code   string
	}{
		{"no credential, and no reference", headers(), 401, "unauthenticated"},
		{"a bad credential before a bad reference", headers("Authorization", "Bearer wk_0000000000000000.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", "X-Ezgh-Caller", "v=2"), 401, "unauthenticated"},
		{"X-Internal-Token never authorizes a delegated route", headers("X-Internal-Token", "internal-secret-token", "X-Ezgh-Caller", reference("compute")), 401, "unauthenticated"},
		{"a customer credential isn't a service credential", headers("Authorization", "Bearer "+apiKey, "X-Ezgh-Caller", reference("compute")), 401, "unauthenticated"},
		{"no reference (planner's call 3)", headers("Authorization", h.compute), 400, "invalid_caller_reference"},
		{"a reference that isn't canonical, before the namespace", headers("Authorization", h.unbound, "X-Ezgh-Caller", reference("compute")+";"), 400, "invalid_caller_reference"},
		{"two references", headers("Authorization", h.compute, "X-Ezgh-Caller", reference("compute"), "X-Ezgh-Caller", reference("compute")), 400, "invalid_caller_reference"},
		{"a credential without a namespace (Review focus 5)", headers("Authorization", h.unbound, "X-Ezgh-Caller", reference("compute")), 403, "delegation_forbidden"},
		{"the credential's namespace isn't the reference's caller", headers("Authorization", h.compute, "X-Ezgh-Caller", reference("network")), 403, "delegation_forbidden"},
		{"a relayed reference whose origin the entry doesn't name", headers("Authorization", h.network, "X-Ezgh-Caller", reference("firewall,network")), 403, "delegation_forbidden"},
		{"a relayed reference through compute, which allows no origins", headers("Authorization", h.compute, "X-Ezgh-Caller", reference("network,compute")), 403, "delegation_forbidden"},
	}
	for _, c := range cases {
		r := h.do(t, "POST", createPath, c.header)
		if r.status != c.status || code(r) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, r.status, code(r), c.status, c.code)
		}
	}
	if r := h.do(t, "POST", createPath, headers("Authorization", h.compute)); r.body["error"].(map[string]any)["issues"].([]any)[0].(map[string]any)["path"] != "X-Ezgh-Caller" {
		t.Errorf("the issue's path: %v", r.body)
	}
	if n := h.org.calls(); n != 0 {
		t.Errorf("%d org-api calls", n)
	}
	if ev := h.events(t); len(ev) != 0 {
		t.Errorf("%d events", len(ev))
	}
	// The unbound credential still works on auth "service" routes.
	if r := h.do(t, "GET", "/internal/v1/things", headers("Authorization", h.unbound)); r.status != 200 {
		t.Errorf("the unbound credential on a service route: %d", r.status)
	}
	// A relayed reference the entry's origins name.
	if r := h.do(t, "POST", createPath, headers("Authorization", h.network, "X-Ezgh-Caller", reference("compute,network"))); r.status != 201 {
		t.Errorf("relayed: %d %v", r.status, r.body)
	}
	if got := h.outbound[len(h.outbound)-1]; got != "refused: internal_error" {
		t.Errorf("a two-entry chain relays no further: %s", got)
	}
}

// What org-api answers decides the receiver's answer and whether it records the call
// (contracts/vectors/trails-recording.json delegatedCalls).
func TestDelegatedAuthorize(t *testing.T) {
	var file struct {
		DelegatedCalls []struct {
			Name   string `json:"name"`
			OrgAPI struct {
				Status  int     `json:"status"`
				Code    string  `json:"code"`
				Results []*bool `json:"results"`
			} `json:"orgApi"`
			Caller         string          `json:"caller"`
			Earlier        []any           `json:"earlier"`
			ReceiverStatus int             `json:"receiverStatus"`
			Recorded       bool            `json:"recorded"`
			UserIdentity   json.RawMessage `json:"userIdentity"`
			UserAgent      string          `json:"userAgent"`
		} `json:"delegatedCalls"`
	}
	vectors.Load(t, "trails-recording.json", &file)
	if len(file.DelegatedCalls) < 8 {
		t.Fatalf("only %d cases", len(file.DelegatedCalls))
	}
	for _, c := range file.DelegatedCalls {
		if c.Caller != "compute" || len(c.Earlier) > 0 {
			continue // a relayed call is TestDelegatedPrepare's, earlier checks TestDelegatedRefusalAfterAllowed's
		}
		t.Run(c.Name, func(t *testing.T) {
			h := newDelegatedHarness(t)
			h.org.status, h.org.code, h.org.results = c.OrgAPI.Status, c.OrgAPI.Code, c.OrgAPI.Results
			r := h.do(t, "POST", createPath, headers("Authorization", h.compute, "X-Ezgh-Caller", reference("compute"), "User-Agent", "compute-api/1.0"))
			want := c.ReceiverStatus
			if want == 200 {
				want = 201
			}
			if r.status != want {
				t.Fatalf("%d %v, want %d", r.status, r.body, want)
			}
			ev := h.events(t)
			if (len(ev) == 1) != c.Recorded || len(ev) > 1 {
				t.Fatalf("%d events", len(ev))
			}
			if c.Recorded {
				if ev[0]["userAgent"] != c.UserAgent || ev[0]["eventName"] != "CreateThing" || ev[0]["eventType"] != "ApiCall" || ev[0]["organizationId"] != orgID {
					t.Errorf("event %v", ev[0])
				}
				identity, _ := json.Marshal(ev[0]["userIdentity"])
				var got, wantIdentity map[string]any
				_ = json.Unmarshal(identity, &got)
				_ = json.Unmarshal([]byte(delegatedIdentity), &wantIdentity)
				if got["invokedBy"] != "compute.ezghcloud.com" || got["principalId"] != wantIdentity["principalId"] {
					t.Errorf("userIdentity %v", got)
				}
			}
		})
	}
}

// The recorded event carries the edge's request ID, the customer's address and the call's
// project; Check on the organization ("") asks about its resource name; the reference reaches
// org-api exactly as received, with the calling namespace.
func TestDelegatedTrailEvent(t *testing.T) {
	h := newDelegatedHarness(t)
	ref := reference("compute")
	r := h.do(t, "POST", createPath, headers("Authorization", h.compute, "X-Ezgh-Caller", ref, "X-Request-Id", "edge-req-7", "X-Forwarded-For", "198.51.100.23"))
	if r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	ev := h.events(t)[0]
	if ev["requestId"] != "edge-req-7" || ev["sourceIpAddress"] != "198.51.100.23" || ev["projectId"] != "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81" || ev["readOnly"] != false {
		t.Errorf("event %v", ev)
	}
	if h.outbound[0] != strings.Replace(ref, "chain=compute", "chain=compute,widgets", 1) {
		t.Errorf("Delegate relayed %s", h.outbound[0])
	}
	body := h.org.bodies[0]
	if body["caller"] != "compute" || body["reference"] != ref || body["checks"].([]any)[0].(map[string]any)["resource"] != thingName {
		t.Errorf("org-api got %v", body)
	}
	r = h.do(t, "GET", "/internal/v1/things/count", headers("Authorization", h.compute, "X-Ezgh-Caller", ref))
	if r.status != 200 || r.body["caller"] != "compute" || r.body["principal"] != "u1" {
		t.Fatalf("%d %v", r.status, r.body)
	}
	if check := h.org.bodies[1]["checks"].([]any)[0].(map[string]any); check["resource"] != "ezgh::"+orgSlug || check["action"] != "widgets.things.list" {
		t.Errorf("the organization's check: %v", check)
	}
	// The example: blockstore's CreateVolume through compute carries the delegated answer's
	// identity and the calling service as userAgent.
	var example map[string]any
	_ = json.Unmarshal(vectors.File(t, "examples/create-volume-via-compute.trail-event.json"), &example)
	identity := example["userIdentity"].(map[string]any)
	if example["userAgent"] != identity["invokedBy"] || example["userAgent"] != "compute.ezghcloud.com" {
		t.Errorf("example %v", example)
	}
}

// Review focus 4: a missing or hostile request ID is replaced by the existing rule, and several
// X-Forwarded-For headers go to org-api joined, unchanged.
func TestDelegatedRequestHeaders(t *testing.T) {
	h := newDelegatedHarness(t)
	ref := reference("compute")
	long := strings.Repeat("r", 129)
	r := h.do(t, "POST", createPath, headers("Authorization", h.compute, "X-Ezgh-Caller", ref, "X-Request-Id", long, "X-Forwarded-For", "10.0.0.1, 203.0.113.9", "X-Forwarded-For", "198.51.100.23"))
	if r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	sent := h.org.delegated[0]
	id := r.header.Get("X-Request-Id")
	if id == long || id == "" || sent.Get("X-Request-Id") != id {
		t.Errorf("request ID %q, org-api got %q", id, sent.Get("X-Request-Id"))
	}
	if sent.Get("X-Forwarded-For") != "10.0.0.1, 203.0.113.9, 198.51.100.23" {
		t.Errorf("X-Forwarded-For %q", sent.Get("X-Forwarded-For"))
	}
	if sent.Get("Authorization") != "Bearer "+okCredential || sent.Get("X-Ezgh-Caller") != "" {
		t.Errorf("org-api headers %v", sent)
	}
	if ev := h.events(t)[0]; ev["sourceIpAddress"] != "198.51.100.23" || ev["requestId"] != id {
		t.Errorf("event %v", ev)
	}
	// Without a request ID, the receiver makes one and sends it on.
	r = h.do(t, "POST", createPath, headers("Authorization", h.compute, "X-Ezgh-Caller", ref))
	if id := r.header.Get("X-Request-Id"); id == "" || h.org.delegated[1].Get("X-Request-Id") != id {
		t.Errorf("made request ID %q, org-api got %q", id, h.org.delegated[1].Get("X-Request-Id"))
	}
}

// An edge's reference comes from its own batch answer's credential; an answer without one is 503
// and no reference.
func TestCallDelegationNeedsCredential(t *testing.T) {
	h := newDelegatedHarness(t)
	launches := "/v1/organizations/" + orgID + "/launches"
	call := func() result {
		return h.do(t, "POST", launches, headers("Authorization", "Bearer "+apiKey, "X-Forwarded-For", "198.51.100.23", "X-Request-Id", "req-9"))
	}
	if r := call(); r.status != 503 || code(r) != "unavailable" {
		t.Errorf("no credential: %d %v", r.status, r.body)
	}
	h.org.credential = true
	r := call()
	if r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	received := strconv.FormatInt(int64(r.body["received"].(float64)), 10)
	want := "v=1;org=" + orgID + ";principal=user:u1;credential=session:" + session + ";at=" + received + ";chain=widgets"
	if r.body["reference"] != want || r.body["forwardedFor"] != "198.51.100.23" || r.body["requestId"] != "req-9" {
		t.Errorf("%v, want %s", r.body, want)
	}
	if _, err := delegation.Parse(want); err != nil {
		t.Error(err)
	}
}

// M1: a delegated route never answers a success without org-api's delegated check (blind trust
// of the calling service), and the calling service's name isn't offered as authority.
func TestDelegatedNeverWithoutOrgAPI(t *testing.T) {
	h := newDelegatedHarness(t)
	auth := headers("Authorization", h.compute, "X-Ezgh-Caller", reference("compute"))
	if r := h.do(t, "POST", "/internal/v1/blind", auth); r.status != 500 {
		t.Errorf("a write without a check: %d %v", r.status, r.body)
	}
	if h.blindCaller != "" {
		t.Errorf("ServiceCaller %q on a delegated call", h.blindCaller)
	}
	if r := h.do(t, "GET", "/internal/v1/blind", auth); r.status != 500 {
		t.Errorf("a read without a check: %d %v", r.status, r.body)
	}
	if n := h.org.calls(); n != 0 || len(h.events(t)) != 0 {
		t.Errorf("%d org-api calls, %d events", n, len(h.events(t)))
	}
	var rows int
	if err := h.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_outbox`).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("committed %d rows (%v)", rows, err)
	}
}

// Repeated Authorization headers are 401 on delegated and service routes.
func TestRepeatedAuthorizationRefused(t *testing.T) {
	h := newDelegatedHarness(t)
	if r := h.do(t, "POST", createPath, headers("Authorization", h.compute, "Authorization", h.unbound, "X-Ezgh-Caller", reference("compute"))); r.status != 401 {
		t.Errorf("delegated: %d", r.status)
	}
	if r := h.do(t, "GET", "/internal/v1/things", headers("Authorization", h.unbound, "Authorization", h.compute)); r.status != 401 {
		t.Errorf("service: %d", r.status)
	}
}

// L4, L5: an answer for another organization, or not invoked by the calling service, is 503 and
// neither acted on nor recorded.
func TestDelegatedAnswerMustBeThisCalls(t *testing.T) {
	for name, set := range map[string]func(*fakeDelegated){
		"another organization":   func(f *fakeDelegated) { f.orgID = "0199a3c2-0000-7000-8000-000000000000" },
		"another invoking party": func(f *fakeDelegated) { f.invokedBy = "network.ezghcloud.com" },
	} {
		h := newDelegatedHarness(t)
		set(h.org)
		r := h.do(t, "POST", createPath, headers("Authorization", h.compute, "X-Ezgh-Caller", reference("compute")))
		if r.status != 503 || len(h.events(t)) != 0 || len(h.outbound) != 0 {
			t.Errorf("%s: %d, %d events", name, r.status, len(h.events(t)))
		}
	}
}

// L6, contracts/vectors/trails-recording.json delegatedCalls with earlier checks: a refusal of the
// delegation after an allowed check isn't recorded; an IAM refusal is.
func TestDelegatedRefusalAfterAllowed(t *testing.T) {
	var file struct {
		DelegatedCalls []struct {
			Name           string       `json:"name"`
			Earlier        []fakeAnswer `json:"earlier"`
			OrgAPI         fakeAnswer   `json:"orgApi"`
			ReceiverStatus int          `json:"receiverStatus"`
			Recorded       bool         `json:"recorded"`
		} `json:"delegatedCalls"`
	}
	vectors.Load(t, "trails-recording.json", &file)
	ran := 0
	for _, c := range file.DelegatedCalls {
		if len(c.Earlier) != 1 {
			continue
		}
		ran++
		t.Run(c.Name, func(t *testing.T) {
			h := newDelegatedHarness(t)
			h.org.answers = append(append([]fakeAnswer{}, c.Earlier...), c.OrgAPI)
			r := h.do(t, "POST", "/internal/v1/twice", headers("Authorization", h.compute, "X-Ezgh-Caller", reference("compute")))
			if r.status != c.ReceiverStatus || (len(h.events(t)) == 1) != c.Recorded {
				t.Errorf("%d, %d events; want %d, recorded %t", r.status, len(h.events(t)), c.ReceiverStatus, c.Recorded)
			}
		})
	}
	if ran < 4 {
		t.Fatalf("only %d cases", ran)
	}
}

// A delegated route's prefixed project resolves like a caller route's, in the reference's
// organization; an unknown one is 404 before org-api is asked to check anything.
func TestDelegatedPrefixedProject(t *testing.T) {
	h := newDelegatedHarness(t)
	ok := h.do(t, "POST", "/internal/v1/projects/"+pathProjectSlug+"/things", headers("Authorization", h.compute, "X-Ezgh-Caller", reference("compute")))
	if ok.status != 201 {
		t.Fatalf("prefixed project: %d %v", ok.status, ok.body)
	}
	if r := h.do(t, "POST", "/internal/v1/projects/prj_aaaaaaaaaaaa/things", headers("Authorization", h.compute, "X-Ezgh-Caller", reference("compute"))); r.status != 404 || h.org.calls() != 1 {
		t.Errorf("unknown project: %d, %d checks", r.status, h.org.calls())
	}
}
