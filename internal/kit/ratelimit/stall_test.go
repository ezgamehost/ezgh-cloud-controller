package ratelimit_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ratelimit"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

// delayProxy forwards TCP to the test Redis, holding each reply back by the current delay: Redis
// runs the command at once, and the client hears about it late.
type delayProxy struct {
	addr  string
	delay atomic.Int64 // nanoseconds
}

func newDelayProxy(t *testing.T) *delayProxy {
	t.Helper()
	raw := os.Getenv("REDIS_TEST_URL")
	if raw == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("REDIS_TEST_URL isn't set")
		}
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &delayProxy{addr: listener.Addr().String()}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			upstream, err := net.Dial("tcp", u.Host)
			if err != nil {
				_ = client.Close()
				continue
			}
			mu.Lock()
			conns = append(conns, client, upstream)
			mu.Unlock()
			go func() { _, _ = io.Copy(upstream, client) }()
			go func() {
				buf := make([]byte, 32<<10)
				for {
					n, err := upstream.Read(buf)
					if n > 0 {
						time.Sleep(time.Duration(p.delay.Load()))
						if _, werr := client.Write(buf[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return p
}

// A Redis reply slower than the Limiter's timeout fails Allow open, though the script already
// took the slot. The Decision keeps the slot, so Release gives it back once Redis answers again,
// instead of the slot being held for its whole TTL (fail-open turning into fail-closed).
func TestReleaseGivesBackASlotTakenByAFailOpenAllow(t *testing.T) {
	p := newDelayProxy(t)
	c, err := redis.New("redis://" + p.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	l := ratelimit.New(c, "kit-test:"+ids.NewV7()+":", slog.New(slog.DiscardHandler))
	l.Timeout = 200 * time.Millisecond
	slot := ratelimit.Bucket{Key: "org:o:concurrent", Slots: 4, TTL: 90 * time.Second}
	rate := ratelimit.Bucket{Key: "org:o:requests", Rate: 100, Window: time.Second, Burst: 100}

	p.delay.Store(int64(time.Second))
	d, err := l.Allow(context.Background(), []ratelimit.Bucket{slot, rate})
	if err != nil || !d.Allowed || !d.FailedOpen || d.Remaining != nil {
		t.Fatalf("a stalled Redis: %+v %v", d, err)
	}
	p.delay.Store(0)

	// The script ran: the slot is taken.
	deadline := time.Now().Add(5 * time.Second)
	for {
		held, err := l.Held(context.Background(), []string{slot.Key})
		if err == nil && held[slot.Key] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stalled script never took its slot: %v %v", held, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := l.Renew(context.Background(), d); err != nil {
		t.Fatalf("renewing a failed-open Decision's slot: %v", err)
	}
	if err := l.Release(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if held, err := l.Held(context.Background(), []string{slot.Key}); err != nil || held[slot.Key] != 0 {
		t.Fatalf("after Release: %v %v", held, err)
	}
	// A failed-open Decision whose slot was never taken renews and releases without complaint.
	if err := l.Renew(context.Background(), d); err != nil {
		t.Fatalf("renewing a slot that isn't there, after a fail-open: %v", err)
	}
	if err := l.Release(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

// Without a Redis client nothing reached Redis: the fail-open Decision holds nothing.
func TestFailOpenWithoutRedisHoldsNothing(t *testing.T) {
	l := ratelimit.New(nil, "x:", slog.New(slog.DiscardHandler))
	d, err := l.Allow(context.Background(), []ratelimit.Bucket{{Key: "s", Slots: 1, TTL: time.Second}})
	if err != nil || !d.Allowed || !d.FailedOpen {
		t.Fatalf("%+v %v", d, err)
	}
	if err := l.Renew(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}
