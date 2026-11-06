package ezghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
)

// agent is a fake k8sagent 9444 endpoint handing out numbered keys.
type agent struct {
	fetches atomic.Int32
	expires time.Time
	status  int
	delay   time.Duration
}

func (a *agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := a.fetches.Add(1)
	time.Sleep(a.delay)
	if a.status != 0 {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"mgmt is unreachable"}}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"apiKey": "ezgh_key" + string(rune('0'+n)), "expiresAt": a.expires})
}

func TestLocalSourceRefreshesFiveMinutesEarly(t *testing.T) {
	T := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	a := &agent{expires: T}
	srv := httptest.NewServer(a)
	defer srv.Close()
	now := T.Add(-time.Hour)
	s := &LocalSource{URL: srv.URL, Now: func() time.Time { return now }}
	ctx := context.Background()
	if _, err := s.APIKey(ctx); err != nil {
		t.Fatal(err)
	}
	now = T.Add(-5*time.Minute - time.Second)
	if _, _ = s.APIKey(ctx); a.fetches.Load() != 1 {
		t.Fatalf("refetched at T-5m1s: %d fetches", a.fetches.Load())
	}
	now = T.Add(-5*time.Minute + time.Second)
	if _, _ = s.APIKey(ctx); a.fetches.Load() != 2 {
		t.Fatalf("didn't refetch at T-4m59s: %d fetches", a.fetches.Load())
	}
}

func TestLocalSourceSingleFlight(t *testing.T) {
	a := &agent{expires: time.Now().Add(time.Hour), delay: 50 * time.Millisecond}
	srv := httptest.NewServer(a)
	defer srv.Close()
	s := &LocalSource{URL: srv.URL}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := s.APIKey(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := a.fetches.Load(); n != 1 {
		t.Fatalf("%d fetches, want 1", n)
	}
}

func TestLocalSource503(t *testing.T) {
	srv := httptest.NewServer(&agent{status: http.StatusServiceUnavailable})
	defer srv.Close()
	_, err := (&LocalSource{URL: srv.URL}).APIKey(context.Background())
	ce, ok := AsCredentials(err)
	if !ok || ce.RetryAfter != 7*time.Second || !strings.Contains(err.Error(), "mgmt is unreachable") {
		t.Fatalf("got %v (%T)", err, err)
	}
}

// lbFake answers every request with status and body, counting the Authorization headers it saw.
func lbFake(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*Clients, *LocalSource, *agent) {
	t.Helper()
	a := &agent{expires: time.Now().Add(time.Hour)}
	agentSrv := httptest.NewServer(a)
	t.Cleanup(agentSrv.Close)
	api := httptest.NewServer(http.HandlerFunc(handle))
	t.Cleanup(api.Close)
	src := &LocalSource{URL: agentSrv.URL}
	return NewAt(src, "ezgh-cloud/test", func(string) string { return api.URL }), src, a
}

func TestUnauthorizedInvalidatesAndRetriesOnce(t *testing.T) {
	var seen []string
	c, _, a := lbFake(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if len(seen) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	if _, err := c.LB.GetLoadBalancer(context.Background(), "o", "p", "lbr_a"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] == seen[1] || a.fetches.Load() != 2 {
		t.Fatalf("auth headers %q, fetches %d", seen, a.fetches.Load())
	}
}

func TestAccessDeniedIsPermissionError(t *testing.T) {
	c, _, _ := lbFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"access_denied","message":"This API key isn't allowed to lb.loadBalancers.create"}}`))
	})
	_, err := c.LB.CreateLoadBalancer(context.Background(), "o", "p", lbapi.LoadBalancerCreate{Name: "a", Subnet: "sbn_a"})
	p, ok := AsPermission(err)
	if !ok || p.Action != "lb.loadBalancers.create" || p.Code != "access_denied" {
		t.Fatalf("got %+v from %v", p, err)
	}
}

func TestQuotaExceeded(t *testing.T) {
	c, _, _ := lbFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"quota_exceeded","message":"This project has 2 load balancers, its limit"}}`))
	})
	_, err := c.LB.CreateLoadBalancer(context.Background(), "o", "p", lbapi.LoadBalancerCreate{Name: "a", Subnet: "sbn_a"})
	code, msg, ok := AsQuota(err)
	if !ok || code != "quota_exceeded" || msg != "This project has 2 load balancers, its limit" {
		t.Fatalf("got %q %q %v", code, msg, ok)
	}
	if _, ok := AsPermission(err); ok {
		t.Fatal("a quota refusal isn't a permission error")
	}
}

func TestKeyNeverLogged(t *testing.T) {
	var log bytes.Buffer
	c, _, _ := lbFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	c.LB.API.Debug = &log
	_, err := c.LB.GetLoadBalancer(context.Background(), "o", "p", "lbr_a")
	if err == nil {
		t.Fatal("want an error")
	}
	if text := log.String() + err.Error(); strings.Contains(text, "ezgh_") || log.Len() == 0 {
		t.Fatalf("log %q, error %q", log.String(), err)
	}
}

func TestIdempotencyKeyFromContext(t *testing.T) {
	var keys []string
	c, _, _ := lbFake(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"op"}`))
	})
	ctx := WithIdempotencyKey(context.Background(), "svc-uid")
	for range 2 {
		if _, err := c.LB.CreateLoadBalancer(ctx, "o", "p", lbapi.LoadBalancerCreate{Name: "a", Subnet: "sbn_a"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.LB.GetLoadBalancer(ctx, "o", "p", "lbr_a"); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 || !strings.HasPrefix(keys[0], "svc-uid-") || keys[1] != keys[0] || keys[2] != "" {
		t.Fatalf("keys %q", keys)
	}
}

func TestIdempotencyKeyOnlyOnPOSTAndPerPath(t *testing.T) {
	var keys []string
	c, _, _ := lbFake(t, func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Method+" "+r.Header.Get("Idempotency-Key"))
		_, _ = w.Write([]byte(`{"id":"x"}`))
	})
	ctx := WithIdempotencyKey(context.Background(), "svc-uid")
	_, _ = c.LB.CreateTargetGroup(ctx, "o", "p", lbapi.TargetGroupCreate{Name: "a"})
	_, _ = c.LB.CreateLoadBalancer(ctx, "o", "p", lbapi.LoadBalancerCreate{Name: "a", Subnet: "sbn_a"})
	_, _ = c.LB.UpdateLoadBalancer(ctx, "o", "p", "lbr_a", lbapi.LoadBalancerUpdate{})
	if len(keys) != 3 || keys[0] == keys[1] || strings.Contains(keys[2], "svc-uid") {
		t.Fatalf("keys %q", keys)
	}
}
