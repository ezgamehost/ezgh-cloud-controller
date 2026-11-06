package authz_test

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

const (
	testServiceCredential = "ok_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	testAPIKey            = "Bearer ezgh_0123456789abcdefghijABCDEFGHIJklmnopqrst"
	testSSOToken          = "Bearer ezgh_sat_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"
	testOrg               = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	testIdentity          = `"organization":{"id":"` + testOrg + `","name":"Acme","slug":"org_k3f9a0x2m7qp","resourceName":"ezgh::org_k3f9a0x2m7qp","isRoot":false},` +
		`"principal":{"type":"bot","id":"b"},"apiKeyId":"k",` +
		`"userIdentity":{"type":"Bot","principalId":"b","userName":"deployer","isRoot":false,"accessKeyId":"k","sessionContext":{"credential":"apiKey","sessionIssuedAt":null,"ssoProviderId":null,"mfaAuthenticated":false},"invokedBy":null}`
)

// batchRequest is what the fake org-api reads.
type batchRequest struct {
	OrganizationID string             `json:"organizationId"`
	Credential     map[string]string  `json:"credential"`
	RequestURL     *string            `json:"requestUrl"`
	Checks         []authz.BatchCheck `json:"checks"`
}

// answerAll answers every check of a well-formed request, allowed unless its action ends in
// ".denied", echoing each.
func answerAll(w http.ResponseWriter, req batchRequest) {
	var results []string
	for _, c := range req.Checks {
		check, _ := json.Marshal(c)
		allowed := strconv.FormatBool(!strings.HasSuffix(c.Action, ".denied"))
		results = append(results, strings.TrimSuffix(string(check), "}")+`,"allowed":`+allowed+`}`)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{`+testIdentity+`,"results":[`+strings.Join(results, ",")+`]}`)
}

func readBatch(t *testing.T, r *http.Request) batchRequest {
	t.Helper()
	var req batchRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		t.Errorf("body: %v", err)
	}
	return req
}

func checks(n int) []authz.BatchCheck {
	out := make([]authz.BatchCheck, n)
	for i := range out {
		out[i] = authz.BatchCheck{Action: "widgets.widgets.get", Resource: fmt.Sprintf("ezgh::org_k3f9a0x2m7qp/check/%d", i)}
	}
	return out
}

// index is the check's place in the whole batch, from its resource.
func index(c authz.BatchCheck) int {
	n, _ := strconv.Atoi(c.Resource[strings.LastIndex(c.Resource, "/")+1:])
	return n
}

// The call: POST /internal/v1/permissions/batch with the product's ok_ alone in Authorization, the
// customer's API key in the body (never a cookie, never requestUrl), their X-Forwarded-For.
func TestCheckBatchSendsTheCustomersCredentialInTheBody(t *testing.T) {
	var got http.Header
	var body batchRequest
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, method, path = r.Header.Clone(), r.Method, r.URL.Path
		body = readBatch(t, r)
		answerAll(w, body)
	}))
	defer server.Close()
	r := httptest.NewRequest("POST", "/v1/organizations/o/widgets", nil)
	r.Header.Set("Authorization", testAPIKey)
	r.Header.Set("Cookie", "better-auth.session_token=abc")
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	cred, err := authz.CredentialFrom(r)
	if err != nil {
		t.Fatal(err)
	}
	c := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential, PublicBaseURL: "https://widgets.ezghcloud.com"}
	answer, err := c.CheckBatch(context.Background(), cred, testOrg, []authz.BatchCheck{{Action: "widgets.widgets.start", Resource: "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:wgt_4k2m9x0a7qpe"}, {Action: "widgets.widgets.denied", Resource: "ezgh::org_k3f9a0x2m7qp"}})
	if err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/internal/v1/permissions/batch" {
		t.Errorf("%s %s", method, path)
	}
	if got.Get("Authorization") != "Bearer "+testServiceCredential || got.Get("Cookie") != "" || got.Get("Content-Type") != "application/json" || got.Get("X-Forwarded-For") != "203.0.113.7, 10.0.0.1" || got.Get("X-Internal-Token") != "" {
		t.Errorf("headers %v", got)
	}
	if body.OrganizationID != testOrg || len(body.Credential) != 1 || body.Credential["authorization"] != testAPIKey || body.RequestURL != nil || len(body.Checks) != 2 {
		t.Errorf("body %+v", body)
	}
	if len(answer.Results) != 2 || !answer.Results[0].Allowed() || answer.Results[1].Allowed() || answer.Results[1].Decision == nil || answer.Organization.ID != testOrg || *answer.APIKeyID != "k" || !strings.Contains(string(answer.UserIdentity), `"userName":"deployer"`) {
		t.Errorf("answer %+v", answer)
	}
}

// A session's better-auth cookies go as credential.cookie (and nothing else of the browser's), no
// X-Forwarded-For without one.
func TestCheckBatchSendsCookies(t *testing.T) {
	var got http.Header
	var body batchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r.Header.Clone(), readBatch(t, r)
		answerAll(w, body)
	}))
	defer server.Close()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", "theme=dark; __Secure-better-auth.session_token=abc.def")
	cred, err := authz.CredentialFrom(r)
	if err != nil {
		t.Fatal(err)
	}
	c := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
	if _, err := c.CheckBatch(context.Background(), cred, testOrg, checks(1)); err != nil {
		t.Fatal(err)
	}
	if len(body.Credential) != 1 || body.Credential["cookie"] != "__Secure-better-auth.session_token=abc.def" || body.RequestURL != nil {
		t.Errorf("body %+v", body)
	}
	if got.Get("Authorization") != "Bearer "+testServiceCredential || got.Get("Cookie") != "" || got.Values("X-Forwarded-For") != nil {
		t.Errorf("headers %v", got)
	}
}

// An EZGH SSO resource token, and only it, sends requestUrl, built as Check builds it; a path with
// no canonical form is 401 without calling.
func TestCheckBatchSendsRequestURLForAnSSOResourceToken(t *testing.T) {
	var body batchRequest
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body = readBatch(t, r)
		answerAll(w, body)
	}))
	defer server.Close()
	c := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential, PublicBaseURL: "https://mcp.ezghcloud.com"}
	r := httptest.NewRequest("POST", "/v1/organizations/o/gateways/g/mcp", nil)
	r.Header.Set("Authorization", testSSOToken)
	cred, err := authz.CredentialFrom(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.CheckBatch(context.Background(), cred, testOrg, checks(1)); err != nil {
		t.Fatal(err)
	}
	if body.Credential["authorization"] != testSSOToken || body.RequestURL == nil || *body.RequestURL != "https://mcp.ezghcloud.com/v1/organizations/o/gateways/g/mcp" {
		t.Errorf("body %+v", body)
	}
	cred.RequestTarget = "/v1/%2e%2e/x"
	if _, err := c.CheckBatch(context.Background(), cred, testOrg, checks(1)); err == nil || err.Status != 401 || calls != 1 {
		t.Errorf("no canonical form: %v after %d calls", err, calls)
	}
	noBase := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
	cred.RequestTarget = "/v1/organizations/o/gateways/g/mcp"
	if _, err := noBase.CheckBatch(context.Background(), cred, testOrg, checks(1)); err == nil || err.Status != 500 || calls != 1 {
		t.Errorf("no PublicBaseURL: %v after %d calls", err, calls)
	}

	// A product on a per-directory host sets the request's origin; it replaces PublicBaseURL.
	cred.RequestTarget = "/central/mgw_4k2m9x0a7qpe"
	if _, err := noBase.CheckBatch(context.Background(), cred.WithPublicBase("https://acme.mcp.ezgh.app"), testOrg, checks(1)); err != nil || calls != 2 {
		t.Fatalf("per-request base: %v after %d calls", err, calls)
	}
	if body.RequestURL == nil || *body.RequestURL != "https://acme.mcp.ezgh.app/central/mgw_4k2m9x0a7qpe" {
		t.Errorf("per-request base sent %v", body.RequestURL)
	}
	if _, err := c.CheckBatch(context.Background(), cred.WithPublicBase("http://acme.mcp.ezgh.app"), testOrg, checks(1)); err == nil || err.Status != 500 || calls != 2 {
		t.Errorf("an http per-request base: %v after %d calls", err, calls)
	}
}

// Zero checks is a bug, a missing ok_ credential a misconfiguration; neither calls org-api.
func TestCheckBatchRefusesWithoutCalling(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	cred := authz.Credential{Header: "Authorization", Value: testAPIKey}
	if _, err := (&authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}).CheckBatch(context.Background(), cred, testOrg, nil); err == nil || err.Code != "internal_error" {
		t.Errorf("no checks: %v", err)
	}
	if _, err := (&authz.Client{BaseURL: server.URL}).CheckBatch(context.Background(), cred, testOrg, checks(1)); err == nil || err.Code != "unavailable" {
		t.Errorf("no service credential: %v", err)
	}
	if _, err := (&authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}).CheckBatch(context.Background(), authz.Credential{}, testOrg, checks(1)); err == nil || err.Code != "internal_error" {
		t.Errorf("no credential: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("%d calls", calls.Load())
	}
}

// One call's statuses map to the caller's errors; nothing fails open.
func TestCheckBatchMapsOrgAPIAnswers(t *testing.T) {
	for status, want := range map[int]string{401: "unauthenticated", 404: "not_found", 400: "internal_error", 403: "unavailable", 429: "too_many_requests", 500: "unavailable", 502: "unavailable", 503: "unavailable"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(status)
		}))
		c := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
		_, err := c.CheckBatch(context.Background(), authz.Credential{Header: "Cookie", Value: "better-auth.session_token=x"}, testOrg, checks(3))
		if err == nil || err.Code != want || (status == 429 && err.RetryAfter != 9) || (status == 403 && !strings.Contains(fmt.Sprint(err.Cause), "permissions.batch")) {
			t.Errorf("%d → %v, want %s", status, err, want)
		}
		server.Close()
	}
	down := &authz.Client{BaseURL: "http://127.0.0.1:1", ServiceCredential: testServiceCredential}
	if _, err := down.CheckBatch(context.Background(), authz.Credential{Header: "Cookie", Value: "x"}, testOrg, checks(1)); err == nil || err.Code != "unavailable" {
		t.Errorf("unreachable → %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer slow.Close()
	timeout := &authz.Client{BaseURL: slow.URL, ServiceCredential: testServiceCredential, HTTP: &http.Client{Timeout: 20 * time.Millisecond}}
	if _, err := timeout.CheckBatch(context.Background(), authz.Credential{Header: "Cookie", Value: "x"}, testOrg, checks(1)); err == nil || err.Code != "unavailable" {
		t.Errorf("timeout → %v", err)
	}
}

// A 200 is believed only when it answers exactly the checks sent, one for one, in order.
func TestCheckBatchValidatesTheAnswer(t *testing.T) {
	sent := []authz.BatchCheck{{Action: "a.b.c", Resource: "ezgh::org_k3f9a0x2m7qp"}, {Action: "a.b.d", Resource: "ezgh::org_k3f9a0x2m7qp"}}
	ok0 := `{"action":"a.b.c","resource":"ezgh::org_k3f9a0x2m7qp","allowed":true}`
	ok1 := `{"action":"a.b.d","resource":"ezgh::org_k3f9a0x2m7qp","allowed":false}`
	for name, body := range map[string]string{
		"not JSON":                     `nope`,
		"no identity":                  `{"results":[` + ok0 + `,` + ok1 + `]}`,
		"a result missing":             `{` + testIdentity + `,"results":[` + ok0 + `]}`,
		"a result too many":            `{` + testIdentity + `,"results":[` + ok0 + `,` + ok1 + `,` + ok1 + `]}`,
		"out of order":                 `{` + testIdentity + `,"results":[` + ok1 + `,` + ok0 + `]}`,
		"another resource":             `{` + testIdentity + `,"results":[` + ok0 + `,{"action":"a.b.d","resource":"ezgh::org_a1b2c3d4e5f6","allowed":true}]}`,
		"no action":                    `{` + testIdentity + `,"results":[` + ok0 + `,{"resource":"ezgh::org_k3f9a0x2m7qp","allowed":true}]}`,
		"no allowed":                   `{` + testIdentity + `,"results":[` + ok0 + `,{"action":"a.b.d","resource":"ezgh::org_k3f9a0x2m7qp"}]}`,
		"allowed isn't a boolean":      `{` + testIdentity + `,"results":[` + ok0 + `,{"action":"a.b.d","resource":"ezgh::org_k3f9a0x2m7qp","allowed":"true"}]}`,
		"null without an error":        `{` + testIdentity + `,"results":[` + ok0 + `,{"action":"a.b.d","resource":"ezgh::org_k3f9a0x2m7qp","allowed":null}]}`,
		"an error without a code":      `{` + testIdentity + `,"results":[` + ok0 + `,{"action":"a.b.d","resource":"ezgh::org_k3f9a0x2m7qp","allowed":null,"error":{"message":"x"}}]}`,
		"an error with an answer":      `{` + testIdentity + `,"results":[` + ok0 + `,{"action":"a.b.d","resource":"ezgh::org_k3f9a0x2m7qp","allowed":true,"error":{"code":"x","message":"x"}}]}`,
		"an error code isn't a string": `{` + testIdentity + `,"results":[` + ok0 + `,{"action":"a.b.d","resource":"ezgh::org_k3f9a0x2m7qp","allowed":null,"error":{"code":7}}]}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
		c := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
		if answer, err := c.CheckBatch(context.Background(), authz.Credential{Header: "Cookie", Value: "x"}, testOrg, sent); err == nil || err.Code != "unavailable" {
			t.Errorf("%s: %+v %v", name, answer, err)
		}
		server.Close()
	}

	// org-api's per-check errors (and codes it may add) are answers, read tolerantly.
	body := `{` + testIdentity + `,"extra":1,"results":[{"action":"a.b.c","resource":"ezgh::org_k3f9a0x2m7qp","allowed":null,"error":{"code":"unknown_action","message":"a.b.c isn't a registered action"},"note":"x"},` +
		`{"action":"a.b.d","resource":"ezgh::org_k3f9a0x2m7qp","allowed":null,"error":{"code":"something_new","message":"x","hint":"y"}}]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
	defer server.Close()
	answer, err := (&authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}).CheckBatch(context.Background(), authz.Credential{Header: "Cookie", Value: "x"}, testOrg, sent)
	if err != nil || answer.Results[0].Allowed() || answer.Results[0].Decision != nil || answer.Results[0].Error.Code != authz.BatchUnknownAction || answer.Results[1].Error.Code != "something_new" {
		t.Fatalf("%+v %v", answer, err)
	}
}

// 120 checks are three calls, 50, 50 and 20, in flight at once, merged back in request order.
func TestCheckBatchSplitsConcurrently(t *testing.T) {
	var mu sync.Mutex
	sizes := map[int]int{}
	arrived := make(chan struct{}, 3)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readBatch(t, r)
		mu.Lock()
		sizes[index(req.Checks[0])] = len(req.Checks)
		mu.Unlock()
		arrived <- struct{}{}
		<-release
		answerAll(w, req)
	}))
	defer server.Close()
	go func() {
		// Every call must be in flight before any is answered: they're concurrent.
		for range 3 {
			select {
			case <-arrived:
			case <-time.After(5 * time.Second):
				t.Error("the calls aren't concurrent")
			}
		}
		close(release)
	}()
	all := checks(120)
	answer, err := (&authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}).CheckBatch(context.Background(), authz.Credential{Header: "Authorization", Value: testAPIKey}, testOrg, all)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(sizes) != "map[0:50 50:50 100:20]" {
		t.Errorf("calls %v", sizes)
	}
	for i, r := range answer.Results {
		if r.Action != all[i].Action || r.Resource != all[i].Resource || !r.Allowed() {
			t.Fatalf("result %d: %+v", i, r)
		}
	}
}

// contracts/vectors/permissions-batch.json splits and combine: a fake org-api answers each call as
// the case says (the call is known by its first check's place), and CheckBatch makes the calls
// and the whole answer the vectors give.
func TestBatchVectors(t *testing.T) {
	var file struct {
		Splits []struct {
			Count int   `json:"count"`
			Max   int   `json:"max"`
			Sizes []int `json:"sizes"`
		} `json:"splits"`
		Combine []struct {
			Name    string `json:"name"`
			Answers []struct {
				Status     *int   `json:"status"`
				Body       string `json:"body"`
				RetryAfter *int   `json:"retryAfter"`
			} `json:"answers"`
			Expected struct {
				Error             string              `json:"error"`
				RetryAfterSeconds int                 `json:"retryAfterSeconds"`
				Answered          []*authz.BatchError `json:"answered"`
			} `json:"expected"`
		} `json:"combine"`
	}
	vectors.Load(t, "permissions-batch.json", &file)
	cred := authz.Credential{Header: "Authorization", Value: testAPIKey}

	for _, c := range file.Splits {
		if c.Max != authz.MaxBatchChecks {
			t.Fatalf("splits %d: max %d", c.Count, c.Max)
		}
		var mu sync.Mutex
		var got []int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			req := readBatch(t, r)
			mu.Lock()
			for len(got) <= index(req.Checks[0])/authz.MaxBatchChecks {
				got = append(got, 0)
			}
			got[index(req.Checks[0])/authz.MaxBatchChecks] = len(req.Checks)
			mu.Unlock()
			answerAll(w, req)
		}))
		answer, err := (&authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}).CheckBatch(context.Background(), cred, testOrg, checks(c.Count))
		server.Close()
		if c.Sizes == nil {
			if err == nil || err.Code != "internal_error" || got != nil {
				t.Errorf("splits %d: %v, calls %v", c.Count, err, got)
			}
			continue
		}
		if err != nil || fmt.Sprint(got) != fmt.Sprint(c.Sizes) || len(answer.Results) != c.Count {
			t.Errorf("splits %d: calls %v, want %v (%v)", c.Count, got, c.Sizes, err)
		}
	}

	codes := map[string]string{"unauthenticated": "unauthenticated", "not_found": "not_found", "bad_request": "internal_error", "rate_limited": "too_many_requests", "unavailable": "unavailable"}
	for _, c := range file.Combine {
		t.Run(c.Name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := readBatch(t, r)
				a := c.Answers[index(req.Checks[0])/authz.MaxBatchChecks]
				switch {
				case a.Status == nil:
					// Unreachable: the connection drops without an answer.
					conn, _, _ := w.(http.Hijacker).Hijack()
					_ = conn.Close()
				case *a.Status == 200 && a.Body == "invalid":
					_, _ = io.WriteString(w, `{"results":[]}`)
				case *a.Status == 200:
					answerAll(w, req)
				default:
					if a.RetryAfter != nil {
						w.Header().Set("Retry-After", strconv.Itoa(*a.RetryAfter))
					}
					w.WriteHeader(*a.Status)
				}
			}))
			defer server.Close()
			all := checks(authz.MaxBatchChecks*(len(c.Answers)-1) + 7)
			answer, err := (&authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}).CheckBatch(context.Background(), cred, testOrg, all)
			if c.Expected.Error != "" {
				if err == nil || err.Code != codes[c.Expected.Error] || (c.Expected.Error == "rate_limited" && err.RetryAfter != c.Expected.RetryAfterSeconds) {
					t.Fatalf("%+v %v, want %s", answer, err, c.Expected.Error)
				}
				return
			}
			if err != nil || len(answer.Results) != len(all) || answer.Organization.ID != testOrg {
				t.Fatalf("%+v %v", answer, err)
			}
			for i, r := range answer.Results {
				want := c.Expected.Answered[i/authz.MaxBatchChecks]
				if r.Action != all[i].Action || r.Resource != all[i].Resource {
					t.Fatalf("result %d answers %+v", i, r)
				}
				if want == nil && !r.Allowed() {
					t.Fatalf("result %d: %+v, want org-api's answer", i, r)
				}
				if want != nil && (r.Allowed() || r.Decision != nil || r.Error == nil || r.Error.Code != want.Code || r.Error.RetryAfterSeconds != want.RetryAfterSeconds) {
					t.Fatalf("result %d: %+v, want %+v", i, r, want)
				}
			}
		})
	}
}

func TestBatchResultAllowed(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		r    authz.BatchResult
		want bool
	}{
		{authz.BatchResult{Decision: &yes}, true},
		{authz.BatchResult{Decision: &no}, false},
		{authz.BatchResult{Error: &authz.BatchError{Code: authz.BatchUnavailable}}, false},
		{authz.BatchResult{}, false},
	} {
		if c.r.Allowed() != c.want {
			t.Errorf("%+v: %v", c.r, !c.want)
		}
	}
}
