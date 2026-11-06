package networkapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

const (
	waitOrg, waitProject, waitOp = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81", "019a12f0-1234-7000-8000-000000000001"
)

// answer is one reply of the fake: an operation's status and blockedReason, and its Retry-After.
type answer struct {
	status, blocked, retryAfter string
	finished                    bool
	// code, when set, is an error answer's status (429).
	code int
	// phase, when set, replaces the phase the status implies.
	phase string
}

func (a answer) body() string {
	blocked, finished, failure, phase := "null", "null", "null", "withdrawing"
	if a.blocked != "" {
		blocked = `"` + a.blocked + `"`
	}
	if a.finished {
		finished, phase = `"2026-10-24T22:00:09Z"`, "complete"
	}
	if a.phase != "" {
		phase = a.phase
	}
	if a.status == "failed" {
		failure = `{"code":"address_in_use","message":"The address is in use"}`
	}
	return `{"id":"` + waitOp + `","type":"reassign","status":"` + a.status + `","phase":"` + phase + `","family":"ipv4","address":"10.0.1.20",` +
		`"sourceInterface":"eni_s0urce000001","destinationInterface":"eni_d3st00000001","expectedGeneration":"7","generation":"8",` +
		`"blockedReason":` + blocked + `,"createdAt":"2026-10-24T22:00:00Z","updatedAt":"2026-10-24T22:00:01Z","finishedAt":` + finished + `,"error":` + failure + `}`
}

// waitFake answers each GET of the operation with the next answer, and records each request and
// each sleep.
type waitFake struct {
	mu       sync.Mutex
	answers  []answer
	requests []string
	slept    []time.Duration
}

func newWaitClient(t *testing.T, f *waitFake, sleep func(context.Context, time.Duration) error) *Client {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		a := f.answers[0]
		if len(f.answers) > 1 {
			f.answers = f.answers[1:]
		}
		f.mu.Unlock()
		if a.retryAfter != "" {
			w.Header().Set("Retry-After", a.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		if a.code != 0 {
			w.WriteHeader(a.code)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"Slow down"}}`))
			return
		}
		_, _ = w.Write([]byte(a.body()))
	}))
	t.Cleanup(server.Close)
	if sleep == nil {
		sleep = func(_ context.Context, d time.Duration) error {
			f.mu.Lock()
			f.slept = append(f.slept, d)
			f.mu.Unlock()
			return nil
		}
	}
	return &Client{API: &apiclient.Client{Service: "Network", BaseURL: server.URL, HTTP: server.Client(), Sleep: sleep}}
}

// It waits as each answer's Retry-After says, keeps going while blocked, and ends only on succeeded.
func TestWaitFollowsRetryAfterUntilSucceeded(t *testing.T) {
	f := &waitFake{answers: []answer{
		{status: "running", blocked: "waiting_for_fence", retryAfter: "2"},
		{status: "running", blocked: "waiting_for_withdrawal", retryAfter: "5"},
		{status: "running"},
		{status: "succeeded", finished: true},
	}}
	c := newWaitClient(t, f, nil)
	op, err := c.WaitPrivateAddressOperation(context.Background(), waitOrg, waitProject, waitOp)
	if err != nil || op.Status != "succeeded" || op.Generation != "8" {
		t.Fatalf("%+v, %v", op, err)
	}
	want := []time.Duration{2 * time.Second, 5 * time.Second, defaultPoll}
	if len(f.slept) != len(want) || f.slept[0] != want[0] || f.slept[1] != want[1] || f.slept[2] != want[2] {
		t.Errorf("slept %v, want %v", f.slept, want)
	}
	if len(f.requests) != 4 || f.requests[0] != "GET /v1/organizations/"+waitOrg+"/projects/"+waitProject+"/private-address-operations/"+waitOp {
		t.Errorf("requests %v", f.requests)
	}
}

// A 429 while waiting is waited out as its Retry-After says, and the wait carries on.
func TestWaitWaitsOutRateLimits(t *testing.T) {
	f := &waitFake{answers: []answer{{code: 429, retryAfter: "7"}, {status: "succeeded", finished: true}}}
	op, err := newWaitClient(t, f, nil).WaitPrivateAddressOperation(context.Background(), waitOrg, waitProject, waitOp)
	if err != nil || op.Status != "succeeded" || len(f.slept) != 1 || f.slept[0] != 7*time.Second {
		t.Errorf("%+v, %v, slept %v", op, err, f.slept)
	}
}

// A failed operation, a status it doesn't know, and a succeeded one that isn't finished are never
// success.
func TestWaitNeverTakesFailureOrUnknownForSuccess(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer answer
		check  func(error) bool
	}{
		{"failed", answer{status: "failed", finished: true}, func(err error) bool {
			var e *OperationFailedError
			return errors.As(err, &e) && strings.Contains(err.Error(), "address_in_use")
		}},
		{"unknown status", answer{status: "paused"}, func(err error) bool { var e *UnknownStatusError; return errors.As(err, &e) }},
		{"succeeded but blocked", answer{status: "succeeded", blocked: "waiting_for_fence", finished: true}, func(err error) bool { var e *UnknownStatusError; return errors.As(err, &e) }},
		{"succeeded but not finished", answer{status: "succeeded"}, func(err error) bool { var e *UnknownStatusError; return errors.As(err, &e) }},
		{"succeeded but not complete", answer{status: "succeeded", finished: true, phase: "activating"}, func(err error) bool { var e *UnknownStatusError; return errors.As(err, &e) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &waitFake{answers: []answer{tc.answer}}
			op, err := newWaitClient(t, f, nil).WaitPrivateAddressOperation(context.Background(), waitOrg, waitProject, waitOp)
			if !tc.check(err) || op == nil || len(f.requests) != 1 {
				t.Errorf("%+v, %v after %v", op, err, f.requests)
			}
		})
	}
}

// Ending the wait stops the reads and sends nothing else: the operation carries on.
func TestWaitCancelledStopsPolling(t *testing.T) {
	f := &waitFake{answers: []answer{{status: "running", blocked: "dependency_unavailable", retryAfter: "2"}}}
	ctx, cancel := context.WithCancel(context.Background())
	c := newWaitClient(t, f, func(ctx context.Context, d time.Duration) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})
	op, err := c.WaitPrivateAddressOperation(ctx, waitOrg, waitProject, waitOp)
	if !errors.Is(err, context.Canceled) || op == nil || op.Status != "running" {
		t.Fatalf("%+v, %v", op, err)
	}
	if len(f.requests) != 1 || !strings.HasPrefix(f.requests[0], "GET ") {
		t.Errorf("requests after cancelling: %v", f.requests)
	}
}

// A 202 is the operation as accepted, with its Retry-After; a running one isn't done.
func TestAcceptedOperationKeepsRetryAfter(t *testing.T) {
	f := &waitFake{answers: []answer{{status: "queued", retryAfter: "2"}}}
	op, err := newWaitClient(t, f, nil).ReassignPrivateAddress(context.Background(), waitOrg, waitProject, "eni_s0urce000001",
		PrivateAddressReassign{Family: "ipv4", Address: "10.0.1.20", ExpectedGeneration: "7", DestinationInterface: "eni_d3st00000001"})
	if err != nil || op.RetryAfter != 2*time.Second || op.Status != "queued" || op.ExpectedGeneration == nil || *op.ExpectedGeneration != "7" {
		t.Errorf("%+v, %v", op, err)
	}
}

// An address write that loses its answer is sent again with the same Idempotency-Key, so the
// service answers the operation it already started instead of starting a second.
func TestAddressWriteRetriesWithTheSameKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		first := len(keys) == 1
		mu.Unlock()
		if first {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(answer{status: "queued"}.body()))
	}))
	defer server.Close()
	// Fresh connections, so it's the client's retry being tested, not Go's transport's.
	c := &Client{API: &apiclient.Client{Service: "Network", BaseURL: server.URL, HTTP: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}},
		Sleep: func(context.Context, time.Duration) error { return nil }}}
	op, err := c.AssignPrivateAddress(context.Background(), waitOrg, waitProject, "eni_s0urce000001", PrivateAddressAssign{Family: "ipv4"})
	if err != nil || op.ID != waitOp {
		t.Fatalf("%+v, %v", op, err)
	}
	if len(keys) != 2 || keys[0] == "" || keys[0] != keys[1] {
		t.Errorf("sent with keys %q", keys)
	}
}
