package ratelimit_test

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ratelimit"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

var someBucket = ratelimit.Bucket{Key: "org:a:requests", Rate: 1, Window: time.Second, Burst: 1}

// Redis down: the request goes ahead, counted as ratelimit_fail_open_total{reason="unavailable"}.
func TestRedisDownFailsOpen(t *testing.T) {
	c, err := redis.New("redis://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	l := ratelimit.New(c, "x:", slog.New(slog.DiscardHandler))
	d, err := l.Allow(context.Background(), []ratelimit.Bucket{someBucket})
	if err != nil || !d.Allowed || !d.FailedOpen {
		t.Fatalf("%+v %v", d, err)
	}
	var out strings.Builder
	if err := l.FailOpens.WritePrometheus(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `ratelimit_fail_open_total{reason="unavailable"} 1`) || l.FailOpens.Total() != 1 {
		t.Fatalf("metric:\n%s", out.String())
	}
}

// No Redis configured (a local run): every request fails open as "unconfigured".
func TestNoRedisFailsOpen(t *testing.T) {
	l := ratelimit.New(nil, "x:", slog.New(slog.DiscardHandler))
	d, err := l.Allow(context.Background(), []ratelimit.Bucket{someBucket})
	if err != nil || !d.Allowed || !d.FailedOpen || l.FailOpens.Count(ratelimit.ReasonUnconfigured) != 1 {
		t.Fatalf("%+v %v", d, err)
	}
}

// A Redis that takes the connection and never answers costs the request the timeout, not more.
func TestHungRedisFailsOpenWithinTheTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	c, err := redis.New("redis://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	l := ratelimit.New(c, "x:", slog.New(slog.DiscardHandler))
	l.Timeout = 50 * time.Millisecond
	started := time.Now()
	d, err := l.Allow(context.Background(), []ratelimit.Bucket{someBucket})
	if err != nil || !d.Allowed || !d.FailedOpen {
		t.Fatalf("%+v %v", d, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("took %v", elapsed)
	}
	if l.FailOpens.Count(ratelimit.ReasonUnavailable) != 1 {
		t.Fatal("not counted")
	}
}

// An invalid bucket is still the caller's error, Redis or not.
func TestInvalidBucketsAreNotFailedOpen(t *testing.T) {
	l := ratelimit.New(nil, "x:", slog.New(slog.DiscardHandler))
	if _, err := l.Allow(context.Background(), []ratelimit.Bucket{{Key: "k"}}); err == nil || l.FailOpens.Total() != 0 {
		t.Fatalf("%v, %d fail-opens", err, l.FailOpens.Total())
	}
}
