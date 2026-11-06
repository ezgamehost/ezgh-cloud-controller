package authz_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

const (
	testOwner     = "jtMJXwL0R8aB3cD4eF5gH6iJ"
	testCA        = "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:pca_7m2k9x0a4qpe"
	issueAction   = "certificates.certificateAuthorities.issue"
	ownerAnswer   = `"organization":{"id":"` + testOrg + `","name":"Acme","slug":"org_k3f9a0x2m7qp","resourceName":"ezgh::org_k3f9a0x2m7qp","isRoot":false},"principal":{"type":"user","id":"` + testOwner + `"},"apiKeyId":null,`
	ownerIdentity = `"userIdentity":{"type":"User","principalId":"` + testOwner + `","userName":"ada@acme.test","isRoot":false,"accessKeyId":null,"sessionContext":null,"invokedBy":null}`
)

var issueCheck = []authz.BatchCheck{{Action: issueAction, Resource: testCA}}

// principalAnswer answers every check, allowed unless its action ends in ".denied", echoing each.
func principalAnswer(checks []authz.BatchCheck) string {
	var results []string
	for _, c := range checks {
		check, _ := json.Marshal(c)
		results = append(results, fmt.Sprintf(`%s,"allowed":%t}`, strings.TrimSuffix(string(check), "}"), !strings.HasSuffix(c.Action, ".denied")))
	}
	return `{` + ownerAnswer + ownerIdentity + `,"results":[` + strings.Join(results, ",") + `]}`
}

// The call: POST /internal/v1/permissions/principal-batch with the product's ok_ alone in
// Authorization and a body of exactly the organization, the principal and the checks: no
// customer credential, no requestUrl, no X-Forwarded-For.
func TestCheckPrincipalBatchSendsPrincipalNotCredential(t *testing.T) {
	var body map[string]any
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/internal/v1/permissions/principal-batch" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		header = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, principalAnswer(issueCheck))
	}))
	defer srv.Close()
	c := &authz.Client{BaseURL: srv.URL, ServiceCredential: testServiceCredential}
	// A principal taken from an answer carries its slug; the request never does (org-api refuses
	// unknown fields).
	ans, herr := c.CheckPrincipalBatch(context.Background(), testOrg, authz.Principal{Type: "user", ID: testOwner, Slug: "usr_abcdefghijkl"}, issueCheck)
	if herr != nil || len(ans.Results) != 1 || !ans.Results[0].Allowed() {
		t.Fatalf("answer %+v %v", ans, herr)
	}
	if got := header.Get("Authorization"); got != "Bearer "+testServiceCredential {
		t.Fatalf("Authorization %q", got)
	}
	if header.Get("X-Forwarded-For") != "" || header.Get("Cookie") != "" {
		t.Fatalf("headers %v", header)
	}
	want := map[string]any{
		"organizationId": testOrg,
		"principal":      map[string]any{"type": "user", "id": testOwner},
		"checks":         []any{map[string]any{"action": issueAction, "resource": testCA}},
	}
	if got, _ := json.Marshal(body); string(got) != string(must(json.Marshal(want))) {
		t.Fatalf("body %s", got)
	}
	if ans.Principal != (authz.Principal{Type: "user", ID: testOwner}) || ans.Organization.ID != testOrg || ans.APIKeyID != nil {
		t.Fatalf("caller %+v", ans)
	}
	var identity map[string]any
	if err := json.Unmarshal(ans.UserIdentity, &identity); err != nil || identity["principalId"] != testOwner || identity["sessionContext"] != nil {
		t.Fatalf("userIdentity %s", ans.UserIdentity)
	}
}

// The contract's example request and answer round-trip: the kit sends the example's body and reads
// the example's answer.
func TestCheckPrincipalBatchContractExamples(t *testing.T) {
	var request struct {
		OrganizationID string             `json:"organizationId"`
		Principal      authz.Principal    `json:"principal"`
		Checks         []authz.BatchCheck `json:"checks"`
	}
	requestJSON := vectors.File(t, "examples/acme.principal-batch-request.json")
	if err := json.Unmarshal(requestJSON, &request); err != nil {
		t.Fatal(err)
	}
	answer := vectors.File(t, "examples/acme-owner.principal-batch-response.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if compact(t, got) != compact(t, requestJSON) {
			t.Errorf("body %s, want the example's %s", got, compact(t, requestJSON))
		}
		_, _ = w.Write(answer)
	}))
	defer srv.Close()
	c := &authz.Client{BaseURL: srv.URL, ServiceCredential: testServiceCredential}
	ans, herr := c.CheckPrincipalBatch(context.Background(), request.OrganizationID, request.Principal, request.Checks)
	if herr != nil || !ans.Results[0].Allowed() || ans.Principal != request.Principal {
		t.Fatalf("answer %+v %v", ans, herr)
	}
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return string(must(json.Marshal(v)))
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// 404 principal_not_found (a removed member, a deleted bot, an organization that's gone): every
// check denied, with no identity to record.
func TestCheckPrincipalBatchNotFoundDeniesAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"error":{"code":"principal_not_found","message":"Principal not found"}}`)
	}))
	defer srv.Close()
	c := &authz.Client{BaseURL: srv.URL, ServiceCredential: testServiceCredential}
	checks := []authz.BatchCheck{{Action: issueAction, Resource: testCA}, {Action: "certificates.certificateAuthorities.get", Resource: testCA}}
	ans, herr := c.CheckPrincipalBatch(context.Background(), testOrg, authz.Principal{Type: "bot", ID: "0199a3c4-0d1e-7f20-8a3b-4c5d6e7f8091"}, checks)
	if herr != nil || len(ans.Results) != 2 {
		t.Fatalf("want every result denied, got %+v %v", ans, herr)
	}
	for i, r := range ans.Results {
		if r.Allowed() || r.Decision == nil || *r.Decision || r.Error != nil || r.Action != checks[i].Action || r.Resource != checks[i].Resource {
			t.Errorf("result %d: %+v", i, r)
		}
	}
	if len(ans.UserIdentity) != 0 || ans.Organization.ID != "" {
		t.Fatalf("a missing principal has no identity: %+v", ans)
	}
}

// Everything else that isn't an answer is never allowed: org-api refusing the product's own
// credential or declaration, or unavailable, is 503 (a 404 that isn't principal_not_found too:
// an org-api without the route); a malformed body is 500; a rate limit keeps org-api's
// Retry-After.
func TestCheckPrincipalBatchFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   int
		cause  string
	}{
		{"our credential is refused", 401, `{"error":{"code":"unauthenticated","message":"x"}}`, 503, "401"},
		{"the namespace didn't declare it", 403, `{"error":{"code":"forbidden","message":"x"}}`, 503, "permissions.principalBatch"},
		{"a malformed body", 400, `{"error":{"code":"invalid_request","message":"x"}}`, 500, "malformed"},
		{"rate limited", 429, `{"error":{"code":"too_many_requests","message":"x"}}`, 429, ""},
		{"unavailable", 503, `{}`, 503, "503"},
		{"a server error", 500, `{}`, 503, "500"},
		{"a 404 without principal_not_found", 404, `{"error":{"code":"not_found","message":"x"}}`, 503, "404"},
		{"a 404 that isn't JSON", 404, `not found`, 503, "404"},
		{"an answer for another organization", 200, strings.Replace(principalAnswer(issueCheck), `"id":"`+testOrg+`"`, `"id":"0199a3c9-0a1b-7c2d-8e3f-4a5b6c7d8e9f"`, 1), 503, "organization"},
		{"an answer for another principal", 200, strings.Replace(principalAnswer(issueCheck), `"id":"`+testOwner+`"}`, `"id":"someone-else"}`, 1), 503, "principal"},
		{"an answer that doesn't echo its check", 200, principalAnswer([]authz.BatchCheck{{Action: issueAction, Resource: "ezgh::org_k3f9a0x2m7qp"}}), 503, "doesn't answer"},
		{"an answer with no identity", 200, `{` + ownerAnswer + `"results":[{"action":"` + issueAction + `","resource":"` + testCA + `","allowed":true}]}`, 503, "userIdentity"},
		{"not JSON", 200, `<html>`, 503, "unexpected body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status == 429 {
					w.Header().Set("Retry-After", "9")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := &authz.Client{BaseURL: srv.URL, ServiceCredential: testServiceCredential}
			// A principal taken from an answer carries its slug; the request never does (org-api refuses
			// unknown fields).
			ans, herr := c.CheckPrincipalBatch(context.Background(), testOrg, authz.Principal{Type: "user", ID: testOwner, Slug: "usr_abcdefghijkl"}, issueCheck)
			if ans != nil || herr == nil || herr.Status != tc.want {
				t.Fatalf("got %+v %+v, want %d", ans, herr, tc.want)
			}
			if tc.want == 429 && herr.RetryAfter != 9 {
				t.Errorf("Retry-After %d", herr.RetryAfter)
			}
			if tc.want == 503 && herr.RetryAfter == 0 {
				t.Errorf("503 without Retry-After")
			}
			if !strings.Contains(fmt.Sprint(herr.Cause), tc.cause) {
				t.Errorf("cause %v doesn't say %q", herr.Cause, tc.cause)
			}
		})
	}
	// Unreachable.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c := &authz.Client{BaseURL: srv.URL, ServiceCredential: testServiceCredential}
	if ans, herr := c.CheckPrincipalBatch(context.Background(), testOrg, authz.Principal{Type: "user", ID: testOwner}, issueCheck); ans != nil || herr == nil || herr.Status != 503 {
		t.Fatalf("connection refused: %+v %+v", ans, herr)
	}
}

// The product's bugs and misconfiguration are answered without a call: no checks or more than
// MaxBatchChecks (500), a principal that isn't a user or a bot (500), no ok_ credential (503).
func TestCheckPrincipalBatchRefusesWithoutACall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, principalAnswer(issueCheck))
	}))
	defer srv.Close()
	c := &authz.Client{BaseURL: srv.URL, ServiceCredential: testServiceCredential}
	user := authz.Principal{Type: "user", ID: testOwner}
	for _, tc := range []struct {
		name      string
		client    *authz.Client
		principal authz.Principal
		checks    []authz.BatchCheck
		want      int
	}{
		{"no checks", c, user, nil, 500},
		{"more than 50 checks", c, user, checks(authz.MaxBatchChecks + 1), 500},
		{"an API key isn't a principal", c, authz.Principal{Type: "apiKey", ID: "k"}, issueCheck, 500},
		{"a directory user isn't a principal here", c, authz.Principal{Type: "ssoUser", ID: "d"}, issueCheck, 500},
		{"a principal has an ID", c, authz.Principal{Type: "user"}, issueCheck, 500},
		{"no ok_ credential", &authz.Client{BaseURL: srv.URL}, user, issueCheck, 503},
	} {
		if ans, herr := tc.client.CheckPrincipalBatch(context.Background(), testOrg, tc.principal, tc.checks); ans != nil || herr == nil || herr.Status != tc.want {
			t.Errorf("%s: got %+v %+v, want %d", tc.name, ans, herr, tc.want)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("%d calls", calls.Load())
	}
	// 50 checks is one call.
	srv50 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Checks []authz.BatchCheck `json:"checks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = io.WriteString(w, principalAnswer(req.Checks))
	}))
	defer srv50.Close()
	c50 := &authz.Client{BaseURL: srv50.URL, ServiceCredential: testServiceCredential}
	if ans, herr := c50.CheckPrincipalBatch(context.Background(), testOrg, user, checks(authz.MaxBatchChecks)); herr != nil || len(ans.Results) != authz.MaxBatchChecks || calls.Load() != 1 {
		t.Fatalf("50 checks: %v, %d calls", herr, calls.Load())
	}
}

// Nothing is cached: two calls ask twice, and the second answer (the policy was revoked in
// between) stands on its own.
func TestCheckPrincipalBatchAsksEveryTime(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := principalAnswer(issueCheck)
		if calls.Add(1) > 1 {
			answer = strings.Replace(answer, `"allowed":true`, `"allowed":false`, 1)
		}
		_, _ = io.WriteString(w, answer)
	}))
	defer srv.Close()
	c := &authz.Client{BaseURL: srv.URL, ServiceCredential: testServiceCredential}
	user := authz.Principal{Type: "user", ID: testOwner}
	first, herr := c.CheckPrincipalBatch(context.Background(), testOrg, user, issueCheck)
	if herr != nil || !first.Results[0].Allowed() {
		t.Fatalf("first: %+v %v", first, herr)
	}
	second, herr := c.CheckPrincipalBatch(context.Background(), testOrg, user, issueCheck)
	if herr != nil || second.Results[0].Allowed() || calls.Load() != 2 {
		t.Fatalf("second: %+v %v, %d calls", second, herr, calls.Load())
	}
}
