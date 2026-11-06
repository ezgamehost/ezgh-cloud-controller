package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
)

type key struct{}

func (key) APIKey(context.Context) (string, error) { return "ezgh_k", nil }

// Review Focus 1: a reconciler whose every call is refused keeps running, reports each refusal,
// and backs off to five minutes.
func TestDeniedReconcilerKeepsRunning(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"access_denied","message":"This API key isn't allowed to lb.loadBalancers.create"}}`))
	}))
	defer api.Close()
	c := ezghclient.NewAt(key{}, "test", func(string) string { return api.URL })
	rec := events.NewFakeRecorder(100)
	svc := &corev1.Service{}
	limiter := RateLimiter()
	item := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "web"}}
	var last time.Duration
	for i := range 50 {
		_, err := c.LB.CreateLoadBalancer(context.Background(), "o", "p", lbapi.LoadBalancerCreate{Name: "a", Subnet: "sbn_a"})
		res, err := Fail(rec, svc, err)
		if err == nil || res.RequeueAfter != 0 {
			t.Fatalf("run %d: %v %v", i, res, err)
		}
		if d := limiter.When(item); d < last {
			t.Fatalf("run %d: delay fell from %s to %s", i, last, d)
		} else {
			last = d
		}
		if ev := <-rec.Events; !strings.Contains(ev, "MissingPermission") {
			t.Fatal(ev)
		}
	}
	if last != 5*time.Minute {
		t.Fatalf("delay reached %s, want 5m", last)
	}
}

func TestFailHonoursLongerRetryAfter(t *testing.T) {
	svc := &corev1.Service{}
	svc.Namespace, svc.Name = "ns", "retry"
	item := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "retry"}}
	limiter := RateLimiter()
	res, err := Fail(events.NewFakeRecorder(1), svc, &ezghclient.CredentialsError{Err: http.ErrServerClosed, RetryAfter: 90 * time.Second})
	if err == nil || res.RequeueAfter != 0 {
		t.Fatal("Fail always hands the error back, so backoff keeps growing")
	}
	if d := limiter.When(item); d != 90*time.Second {
		t.Fatalf("first wait %s, want the Retry-After", d)
	}
	// A short Retry-After never undercuts the growing backoff.
	_, _ = Fail(events.NewFakeRecorder(1), svc, &ezghclient.CredentialsError{Err: http.ErrServerClosed, RetryAfter: time.Second})
	if d := limiter.When(item); d != 10*time.Second {
		t.Fatalf("second wait %s, want the exponential 10s", d)
	}
}
