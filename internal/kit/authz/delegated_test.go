package authz_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

const testReference = "v=1;org=0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70;principal=user:jtMJXwL0R8aB3cD4eF5gH6iJ;credential=session:Qm7cXw2KpL9sTn4vBd8yRf3h;at=1792051200;chain=compute"

// The C1 example batch answer names ada's session; an answer without credential names none.
func TestBatchAnswerCredentialExample(t *testing.T) {
	var answer authz.BatchAnswer
	if err := json.Unmarshal(vectors.File(t, "examples/ada-session.permissions-batch-response.json"), &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Credential == nil || *answer.Credential != (authz.CredentialRef{Kind: "session", ID: "Qm7cXw2KpL9sTn4vBd8yRf3h"}) {
		t.Fatalf("Credential = %+v", answer.Credential)
	}
	var plain authz.BatchAnswer
	if err := json.Unmarshal(vectors.File(t, "examples/deployer.permissions-batch-response.json"), &plain); err != nil {
		t.Fatal(err)
	}
	if plain.Credential != nil {
		t.Fatalf("Credential = %+v, want nil", plain.Credential)
	}
}

// delegatedAnswer is the C1 delegated answer with the given allowed values, one check each.
func delegatedAnswer(t *testing.T, results []*bool) ([]byte, []authz.BatchCheck) {
	t.Helper()
	var answer map[string]any
	if err := json.Unmarshal(vectors.File(t, "examples/ada-via-compute.delegated-check-response.json"), &answer); err != nil {
		t.Fatal(err)
	}
	var checks []authz.BatchCheck
	var out []map[string]any
	for i, allowed := range results {
		check := authz.BatchCheck{Action: "blockstore.volumes.create", Resource: fmt.Sprintf("ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:vol_%012d", i)}
		checks = append(checks, check)
		r := map[string]any{"action": check.Action, "resource": check.Resource, "allowed": allowed}
		if allowed == nil {
			r["error"] = map[string]any{"code": "not_delegable", "message": "x"}
		}
		out = append(out, r)
	}
	answer["results"] = out
	raw, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return raw, checks
}

// contracts/vectors/delegated-checks.json receiverMapping: a fake org-api answers as the row says,
// and CheckDelegated (with the caller's mapping of false and null results) gives the receiver's
// answer.
func TestCheckDelegatedMapping(t *testing.T) {
	var file struct {
		ReceiverMapping []struct {
			Name   string `json:"name"`
			OrgAPI struct {
				Status     *int     `json:"status"`
				Code       string   `json:"code"`
				Issues     []string `json:"issues"`
				Results    []*bool  `json:"results"`
				RetryAfter *int     `json:"retryAfter"`
				Body       string   `json:"body"`
			} `json:"orgApi"`
			Expected struct {
				Status     int    `json:"status"`
				Code       string `json:"code"`
				RetryAfter int    `json:"retryAfter"`
			} `json:"expected"`
		} `json:"receiverMapping"`
	}
	vectors.Load(t, "delegated-checks.json", &file)
	for _, c := range file.ReceiverMapping {
		t.Run(c.Name, func(t *testing.T) {
			results := c.OrgAPI.Results
			if results == nil {
				results = []*bool{new(bool)}
			}
			body, checks := delegatedAnswer(t, results)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.OrgAPI.Status == nil {
					hj, _ := w.(http.Hijacker)
					conn, _, _ := hj.Hijack()
					_ = conn.Close()
					return
				}
				if c.OrgAPI.RetryAfter != nil {
					w.Header().Set("Retry-After", strconv.Itoa(*c.OrgAPI.RetryAfter))
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(*c.OrgAPI.Status)
				switch {
				case *c.OrgAPI.Status == http.StatusOK && c.OrgAPI.Body == "invalid":
					_, _ = w.Write([]byte(`{"organization":{}}`))
				case *c.OrgAPI.Status == http.StatusOK:
					_, _ = w.Write(body)
				default:
					issues := []map[string]string{}
					for _, path := range c.OrgAPI.Issues {
						issues = append(issues, map[string]string{"path": path, "message": "x"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": c.OrgAPI.Code, "message": "x", "issues": issues}})
				}
			}))
			defer server.Close()
			client := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
			answer, herr := client.CheckDelegated(context.Background(), authz.DelegatedRequest{Caller: "compute", Reference: testReference, Checks: checks})
			status, code, retry := http.StatusOK, "", 0
			switch {
			case herr != nil:
				status, code, retry = herr.Status, herr.Code, herr.RetryAfter
			default:
				// The receiver's mapping of a 200 (service.Call): a null result first, then a false one.
				for _, r := range answer.Results {
					if r.Decision == nil {
						status, code = http.StatusForbidden, "delegation_forbidden"
					}
				}
				if status == http.StatusOK {
					for _, r := range answer.Results {
						if !r.Allowed() {
							status, code = http.StatusForbidden, "access_denied"
						}
					}
				}
			}
			if status != c.Expected.Status || code != c.Expected.Code || retry != c.Expected.RetryAfter {
				t.Errorf("got %d %q Retry-After %d, want %d %q %d", status, code, retry, c.Expected.Status, c.Expected.Code, c.Expected.RetryAfter)
			}
		})
	}
}

// The request carries the receiver's ok_, the body as the contract's, the edge's X-Forwarded-For
// and the request's ID.
func TestCheckDelegatedRequest(t *testing.T) {
	body, checks := delegatedAnswer(t, []*bool{ptr(true)})
	var got *http.Request
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sent)
		_, _ = w.Write(body)
	}))
	defer server.Close()
	client := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
	ctx := httpapi.ContextWithRequestID(context.Background(), "edge-request-1")
	answer, herr := client.CheckDelegated(ctx, authz.DelegatedRequest{Caller: "compute", Reference: testReference, ForwardedFor: "203.0.113.9, 198.51.100.23", Checks: checks})
	if herr != nil {
		t.Fatal(herr)
	}
	if !answer.Results[0].Allowed() || answer.Credential == nil {
		t.Fatalf("answer %+v", answer)
	}
	if got.URL.Path != "/internal/v1/permissions/delegated" || got.Method != http.MethodPost {
		t.Errorf("%s %s", got.Method, got.URL.Path)
	}
	if got.Header.Get("Authorization") != "Bearer "+testServiceCredential || got.Header.Get("X-Forwarded-For") != "203.0.113.9, 198.51.100.23" || got.Header.Get("X-Request-Id") != "edge-request-1" {
		t.Errorf("headers %v", got.Header)
	}
	if sent["caller"] != "compute" || sent["reference"] != testReference || len(sent["checks"].([]any)) != 1 || len(sent) != 3 {
		t.Errorf("body %v", sent)
	}
}

// More than 50 checks, or none, is a bug answered without a call (planner's call 15: never split).
func TestCheckDelegatedTooMany(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
	for _, n := range []int{0, 51} {
		_, herr := client.CheckDelegated(context.Background(), authz.DelegatedRequest{Caller: "compute", Reference: testReference, Checks: checks(n)})
		if herr == nil || herr.Status != http.StatusInternalServerError {
			t.Errorf("%d checks: %v", n, herr)
		}
	}
	if calls.Load() != 0 {
		t.Errorf("%d calls", calls.Load())
	}
}

func ptr[T any](v T) *T { return &v }
