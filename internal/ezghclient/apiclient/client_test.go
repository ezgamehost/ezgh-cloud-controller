package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPathEscapesValues(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   string
	}{
		{[]string{"o", "p"}, "/v1/organizations/o/projects/p"},
		{[]string{"a/b", "c?d"}, "/v1/organizations/a%2Fb/projects/c%3Fd"},
		{[]string{"..", "."}, "/v1/organizations/%2E%2E/projects/%2E"},
		{[]string{"", "x"}, "/v1/organizations/%20/projects/x"},
	} {
		if got := Path("/v1/organizations/{}/projects/{}", tc.values...); got != tc.want {
			t.Errorf("%q → %s, want %s", tc.values, got, tc.want)
		}
	}
}

func TestParseError(t *testing.T) {
	h := http.Header{"Retry-After": {"7"}, "X-Request-Id": {"r1"}}
	e := parseError("org-api", 400, h, []byte(`{"error":{"code":"invalid_request","message":"Request validation failed","issues":[{"path":"name","message":"too long"}]}}`))
	if e.Code != "invalid_request" || len(e.Issues) != 1 || e.RetryAfter != 7*time.Second || e.RequestID != "r1" {
		t.Errorf("%+v", e)
	}
	// better-auth's bare shape.
	e = parseError("auth-api", 429, nil, []byte(`{"code":"TOO_MANY","message":"slow down"}`))
	if e.Code != "TOO_MANY" || e.Message != "slow down" {
		t.Errorf("%+v", e)
	}
	// A gateway's HTML page: the status names the error.
	e = parseError("audit-api", 502, nil, []byte(`<html>bad gateway</html>`))
	if e.Code != "unavailable" || !strings.Contains(e.Message, "502") || strings.Contains(e.Message, "<html>") {
		t.Errorf("%+v", e)
	}
	e = parseError("audit-api", 400, nil, []byte(`{"error":{"code":"invalid_query","message":"x","line":1,"column":8}}`))
	if string(e.Details["column"]) != "8" {
		t.Errorf("%+v", e)
	}
	e = parseError("quotas", 409, nil, []byte(`{"error":{"code":"quota_exceeded","message":"m","quota":{"id":"q","kind":"allocation","scope":"project","limit":20,"used":20}}}`))
	if !e.QuotaExceeded() || e.Quota.Limit != 20 || *e.Quota.Used != 20 {
		t.Errorf("%+v", e)
	}
	// The Prometheus HTTP API's body (obs-api's PromQL routes), with the platform's code.
	e = parseError("Observability", 422, nil, []byte(`{"status":"error","errorType":"execution","error":"query processing would load too many samples","code":"query_too_expensive","budget":"samples"}`))
	if e.Code != "query_too_expensive" || e.Message != "query processing would load too many samples" || string(e.Details["budget"]) != `"samples"` || string(e.Details["errorType"]) != `"execution"` {
		t.Errorf("%+v", e)
	}
	// Without a code (plain Prometheus), the status names it.
	e = parseError("Observability", 400, nil, []byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
	if e.Code != "invalid_request" || e.Message != "parse error" {
		t.Errorf("%+v", e)
	}
}

func TestRetryAfter(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 0, "3": 3 * time.Second, "0": time.Second, "-5": time.Second, "soon": time.Second} {
		if got := RetryAfter(v); got != want {
			t.Errorf("%q → %v, want %v", v, got, want)
		}
	}
	date := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if got := RetryAfter(date); got < 80*time.Second || got > 91*time.Second {
		t.Errorf("date → %v", got)
	}
}

// A read is tried again when there's no answer; a write isn't, since it may have happened.
func TestTransportRetriesOnlyReads(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Hang up without answering.
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	// Fresh connections: Go's transport itself replays a request that carries Idempotency-Key on
	// a reused connection that drops, which isn't what this test is about.
	transport := &http.Transport{DisableKeepAlives: true}
	c := &Client{Service: "x", BaseURL: server.URL, HTTP: &http.Client{Transport: transport}, Sleep: func(context.Context, time.Duration) error { return nil }}
	if _, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/"}, &struct{}{}); err != nil || calls.Load() != 2 {
		t.Errorf("GET: %v after %d", err, calls.Load())
	}
	calls.Store(0)
	_, err := c.Do(context.Background(), Request{Method: http.MethodPost, Path: "/", Body: map[string]string{}}, &struct{}{})
	var te *TransportError
	if !errors.As(err, &te) || calls.Load() != 1 {
		t.Errorf("POST: %v after %d", err, calls.Load())
	}
}

type countingAuth struct{ refreshes int }

func (a *countingAuth) Authorization(context.Context) (string, error) {
	return "Bearer t" + string(rune('0'+a.refreshes)), nil
}

func (a *countingAuth) Refresh(_ context.Context, refused string) (bool, error) {
	a.refreshes++
	return true, nil
}

// A 401 refreshes once and retries once; a second 401 is the answer.
func TestUnauthorizedRefreshesOnce(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"Sign in required"}}`))
	}))
	defer server.Close()
	auth := &countingAuth{}
	c := &Client{Service: "x", BaseURL: server.URL, HTTP: server.Client(), Auth: auth}
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/"}, nil)
	if e, ok := AsError(err); !ok || e.Status != 401 || auth.refreshes != 1 || len(seen) != 2 || seen[0] == seen[1] {
		t.Errorf("%v, refreshes %d, sent %v", err, auth.refreshes, seen)
	}
}

type refusedAuth struct{ calls int }

var errLoginEnded = errors.New("login has ended")

func (a *refusedAuth) Authorization(context.Context) (string, error) {
	a.calls++
	return "", errLoginEnded
}

func (a *refusedAuth) Refresh(context.Context, string) (bool, error) { return false, nil }

// A credential that can't be used fails the call at once, as itself: it isn't a request that got
// no answer, so it's neither retried nor reported as an unreachable service.
func TestUnusableCredentialIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	auth := &refusedAuth{}
	var slept time.Duration
	c := &Client{Service: "x", BaseURL: server.URL, HTTP: server.Client(), Auth: auth,
		Sleep: func(_ context.Context, d time.Duration) error { slept += d; return nil }}
	_, err := c.Do(context.Background(), Request{Method: http.MethodGet, Path: "/"}, &struct{}{})
	var te *TransportError
	if !errors.Is(err, errLoginEnded) || errors.As(err, &te) || auth.calls != 1 || slept != 0 || calls.Load() != 0 {
		t.Errorf("%v: %d authorizations, slept %s, %d requests", err, auth.calls, slept, calls.Load())
	}
}

// Over a reused keep-alive connection that drops before the answer, Go's transport resends a
// request that carries Idempotency-Key. So only creates carry one: a create is resent with the
// same key (the service replays it), and any other POST is never sent twice.
func TestOnlyCreatesAreReplayed(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = append(seen[r.URL.Path], r.Header.Get("Idempotency-Key"))
		first := len(seen[r.URL.Path]) == 1
		mu.Unlock()
		if first && r.URL.Path != "/warm" {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	c := &Client{Service: "x", BaseURL: server.URL, HTTP: server.Client(), Sleep: func(context.Context, time.Duration) error { return nil }}
	ctx := context.Background()
	warm := func() {
		if _, err := c.Do(ctx, Request{Method: http.MethodGet, Path: "/warm"}, &struct{}{}); err != nil {
			t.Fatal(err)
		}
	}
	warm()
	if _, err := c.Do(ctx, Request{Method: http.MethodPost, Path: "/accept"}, &struct{}{}); err == nil {
		t.Error("a POST that lost its answer reported success")
	}
	warm()
	if _, err := c.Do(ctx, Request{Method: http.MethodPost, Path: "/create", Create: true, Body: map[string]string{}}, &struct{}{}); err != nil {
		t.Errorf("create: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen["/accept"]) != 1 || seen["/accept"][0] != "" {
		t.Errorf("a plain POST was sent %d times, with key %q", len(seen["/accept"]), seen["/accept"])
	}
	if keys := seen["/create"]; len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("a create was sent with keys %q", keys)
	}
}
