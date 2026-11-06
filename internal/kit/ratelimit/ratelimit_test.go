package ratelimit_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ratelimit"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

// limiter is a Limiter on the test Redis with its own key prefix and a clock the test moves.
func limiter(t *testing.T) (*ratelimit.Limiter, *time.Time) {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("REDIS_TEST_URL isn't set")
		}
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	c, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	now := time.UnixMilli(1_790_000_000_000)
	l := ratelimit.New(c, "kit-test:"+ids.NewV7()+":", slog.New(slog.DiscardHandler))
	l.Timeout = 2 * time.Second // the race detector is slow; failing open is Task 12's
	l.Now = func() time.Time { return now }
	return l, &now
}

func allow(t *testing.T, l *ratelimit.Limiter, buckets ...ratelimit.Bucket) ratelimit.Decision {
	t.Helper()
	d, err := l.Allow(context.Background(), buckets)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A token bucket's Retry-After is exact to the millisecond: the time until enough tokens refill.
func TestTokenBucketRetryAfterIsExact(t *testing.T) {
	l, clock := limiter(t)
	b := ratelimit.Bucket{Key: "org:a:requests", Rate: 1, Window: time.Second, Burst: 3}
	for i := range 3 {
		if d := allow(t, l, b); !d.Allowed {
			t.Fatalf("request %d: %+v", i, d)
		}
	}
	if d := allow(t, l, b); d.Allowed || d.RetryAfter != time.Second || d.Never || d.FailedOpen {
		t.Fatalf("4th: %+v", d)
	}
	*clock = clock.Add(999 * time.Millisecond)
	if d := allow(t, l, b); d.Allowed || d.RetryAfter != time.Millisecond {
		t.Fatalf("at 999 ms: %+v", d)
	}
	*clock = clock.Add(time.Millisecond)
	if d := allow(t, l, b); !d.Allowed {
		t.Fatalf("at 1 s: %+v", d)
	}
}

// Cost takes that many tokens; a cost over the burst can never pass (Never), and takes nothing.
func TestTokenBucketCost(t *testing.T) {
	l, _ := limiter(t)
	bytes := func(cost int64) ratelimit.Bucket {
		return ratelimit.Bucket{Key: "org:a:bytes", Rate: 10, Window: time.Second, Burst: 10, Cost: cost}
	}
	if d := allow(t, l, bytes(7)); !d.Allowed {
		t.Fatalf("7: %+v", d)
	}
	if d := allow(t, l, bytes(8)); d.Allowed || d.RetryAfter != 500*time.Millisecond {
		t.Fatalf("8 of 3 left at 10/s: %+v", d)
	}
	if d := allow(t, l, bytes(11)); d.Allowed || !d.Never {
		t.Fatalf("11 over a burst of 10: %+v", d)
	}
	if d := allow(t, l, bytes(3)); !d.Allowed {
		t.Fatalf("the 3 left: %+v", d)
	}
}

// One call takes from every bucket or from none.
func TestAllOrNothing(t *testing.T) {
	l, _ := limiter(t)
	org := ratelimit.Bucket{Key: "org:a:requests", Rate: 1, Window: time.Second, Burst: 5}
	project := ratelimit.Bucket{Key: "project:p:requests", Rate: 1, Window: time.Second, Burst: 1}
	if d := allow(t, l, org, project); !d.Allowed {
		t.Fatalf("first: %+v", d)
	}
	if d := allow(t, l, org, project); d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("second: %+v", d)
	}
	for i := range 4 {
		if d := allow(t, l, org); !d.Allowed {
			t.Fatalf("the organization's token %d was taken by a refused call: %+v", i+2, d)
		}
	}
	if d := allow(t, l, org); d.Allowed {
		t.Fatalf("a 6th from a burst of 5: %+v", d)
	}
}

// Concurrency slots: held until Release or the TTL; a full bucket says retry within a second; a
// refused call takes no slot.
func TestConcurrencySlots(t *testing.T) {
	l, clock := limiter(t)
	searches := ratelimit.Bucket{Key: "org:a:searches", Slots: 2, TTL: time.Minute}
	first, second := allow(t, l, searches), allow(t, l, searches)
	if !first.Allowed || !second.Allowed {
		t.Fatalf("%+v %+v", first, second)
	}
	if d := allow(t, l, searches); d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("a 3rd of 2 slots: %+v", d)
	}
	if err := l.Release(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if d := allow(t, l, searches); !d.Allowed {
		t.Fatalf("after a release: %+v", d)
	}
	*clock = clock.Add(61 * time.Second)
	if d := allow(t, l, searches); !d.Allowed {
		t.Fatalf("after the TTL: %+v", d)
	}

	slot := ratelimit.Bucket{Key: "org:b:searches", Slots: 1, TTL: time.Minute}
	tokens := ratelimit.Bucket{Key: "org:b:requests", Rate: 1, Window: time.Hour, Burst: 1}
	held := allow(t, l, slot, tokens)
	if !held.Allowed {
		t.Fatalf("%+v", held)
	}
	if err := l.Release(context.Background(), held); err != nil {
		t.Fatal(err)
	}
	if d := allow(t, l, slot, tokens); d.Allowed || d.RetryAfter != time.Hour {
		t.Fatalf("no token left: %+v", d)
	}
	if d := allow(t, l, slot); !d.Allowed {
		t.Fatalf("the refused call took the slot: %+v", d)
	}
}

// Fifty concurrent requests take exactly the burst: the script is atomic.
func TestConcurrentRequestsTakeExactlyTheBurst(t *testing.T) {
	l, _ := limiter(t)
	b := ratelimit.Bucket{Key: "org:a:requests", Rate: 1, Window: time.Hour, Burst: 20}
	var allowed, unanswered atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := l.Allow(context.Background(), []ratelimit.Bucket{b})
			switch {
			case err != nil || d.FailedOpen:
				unanswered.Add(1)
			case d.Allowed:
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	// A call Redis didn't answer in time (an error here, failing open after Task 12) says nothing
	// about atomicity: fail on Redis latency by name, not on the count.
	if unanswered.Load() != 0 {
		t.Fatalf("Redis didn't answer %d of 50 calls within %v: test Redis latency, not the script", unanswered.Load(), l.Timeout)
	}
	if allowed.Load() != 20 {
		t.Fatalf("%d allowed, want 20", allowed.Load())
	}
}

// A lease with a short TTL never shortens a longer one's: the slot set expires with its last
// lease, so a still-running request keeps its slot (Codex C03).
func TestAShortLeaseNeverExpiresALongerOne(t *testing.T) {
	l, clock := limiter(t)
	long := ratelimit.Bucket{Key: "org:a:searches", Slots: 2, TTL: time.Minute}
	short := ratelimit.Bucket{Key: "org:a:searches", Slots: 2, TTL: time.Second}
	if d := allow(t, l, long); !d.Allowed {
		t.Fatalf("the long lease: %+v", d)
	}
	if d := allow(t, l, short); !d.Allowed {
		t.Fatalf("the short lease: %+v", d)
	}
	ttl, err := l.Redis.Do(context.Background(), "PTTL", l.Prefix+long.Key)
	if err != nil {
		t.Fatal(err)
	}
	if ms, _ := ttl.(int64); ms < 59_000 {
		t.Fatalf("the slot set expires in %d ms, before the 60 s lease it holds", ms)
	}
	// Past the short lease, in the script's clock and in Redis' own (the key's expiry).
	*clock = clock.Add(2 * time.Second)
	time.Sleep(2100 * time.Millisecond)
	if d := allow(t, l, long); !d.Allowed {
		t.Fatalf("the short lease's slot, expired: %+v", d)
	}
	if d := allow(t, l, long); d.Allowed {
		t.Fatalf("a 3rd slot while the long lease still runs: %+v", d)
	}
}

// A replica whose clock is behind never refills a bucket backwards.
func TestAClockBehindNeverRefills(t *testing.T) {
	l, clock := limiter(t)
	b := ratelimit.Bucket{Key: "org:a:requests", Rate: 1, Window: time.Second, Burst: 2}
	if d := allow(t, l, b); !d.Allowed {
		t.Fatalf("%+v", d)
	}
	*clock = clock.Add(-10 * time.Second)
	if d := allow(t, l, b); !d.Allowed {
		t.Fatalf("the 2nd token: %+v", d)
	}
	if d := allow(t, l, b); d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("an empty bucket behind the clock: %+v", d)
	}
}

// A bucket that makes no sense is the caller's bug: an error, before Redis is asked.
func TestInvalidBuckets(t *testing.T) {
	l := ratelimit.New(nil, "x:", slog.New(slog.DiscardHandler))
	ok := ratelimit.Bucket{Key: "k", Rate: 1, Window: time.Second, Burst: 1}
	for name, buckets := range map[string][]ratelimit.Bucket{
		"no key":                              {{Rate: 1, Window: time.Second, Burst: 1}},
		"no rate":                             {{Key: "k", Window: time.Second, Burst: 1}},
		"no burst":                            {{Key: "k", Rate: 1, Window: time.Second}},
		"no window":                           {{Key: "k", Rate: 1, Burst: 1}},
		"a sub-millisecond window":            {{Key: "k", Rate: 1, Window: 1500 * time.Microsecond, Burst: 1}},
		"a negative cost":                     {{Key: "k", Rate: 1, Window: time.Second, Burst: 1, Cost: -1}},
		"slots with a rate":                   {{Key: "k", Slots: 1, TTL: time.Second, Rate: 1}},
		"slots without a TTL":                 {{Key: "k", Slots: 1}},
		"a TTL without slots":                 {{Key: "k", Rate: 1, Window: time.Second, Burst: 1, TTL: time.Second}},
		"a burst too big to be exact":         {{Key: "k", Rate: 1, Window: time.Hour, Burst: 1 << 40}},
		"the same key twice":                  {ok, ok},
		"a monthly counter without a limit":   {{Key: "k", Monthly: true}},
		"a monthly counter with a rate":       {{Key: "k", Monthly: true, Limit: 5, Rate: 1}},
		"a monthly counter with slots":        {{Key: "k", Monthly: true, Limit: 5, Slots: 1, TTL: time.Second}},
		"a limit on a token bucket":           {{Key: "k", Rate: 1, Window: time.Second, Burst: 1, Limit: 5}},
		"a monthly counter's negative cost":   {{Key: "k", Monthly: true, Limit: 5, Cost: -1}},
		"a monthly limit too big to be exact": {{Key: "k", Monthly: true, Limit: 1 << 53}},
	} {
		if _, err := l.Allow(context.Background(), buckets); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if d, err := l.Allow(context.Background(), nil); err != nil || !d.Allowed {
		t.Errorf("no buckets: %+v %v", d, err)
	}
}

// Retry-After headers are whole seconds, rounded up, from 1 to 3600.
func TestRetryAfterSeconds(t *testing.T) {
	for d, want := range map[time.Duration]int{0: 1, time.Millisecond: 1, time.Second: 1, 1001 * time.Millisecond: 2, 90 * time.Second: 90, 2 * time.Hour: 3600} {
		if got := ratelimit.RetryAfterSeconds(d); got != want {
			t.Errorf("%v: %d, want %d", d, got, want)
		}
	}
}

// A monthly counter counts this UTC month's requests (its cost each) up to its limit, refuses past
// it until the month ends (RetryAfter is exact), names itself in Denied, and starts again at 0 at
// the start of the next UTC month, its key expiring then.
func TestMonthlyCounter(t *testing.T) {
	l, clock := limiter(t)
	// The last minute of the real current UTC month, so Redis' own clock hasn't expired its key.
	y, m, _ := time.Now().UTC().Date()
	next := time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
	*clock = next.Add(-time.Minute)
	month := ratelimit.Bucket{Key: "org:a:calls", Monthly: true, Limit: 3}
	rate := ratelimit.Bucket{Key: "org:a:requests", Rate: 100, Window: time.Second, Burst: 100}
	for i := range 3 {
		if d := allow(t, l, month, rate); !d.Allowed {
			t.Fatalf("call %d: %+v", i+1, d)
		}
	}
	d := allow(t, l, month, rate)
	if d.Allowed || d.Never || d.RetryAfter != time.Minute || len(d.Denied) != 1 || d.Denied[0] != "org:a:calls" {
		t.Fatalf("the 4th: %+v", d)
	}
	ttl, err := l.Redis.Do(context.Background(), "PEXPIRETIME", l.Prefix+ratelimit.MonthKey(month.Key, *clock))
	if err != nil {
		t.Fatal(err)
	}
	if at, _ := ttl.(int64); at != next.UnixMilli() {
		t.Fatalf("the month's key expires at %d", at)
	}
	if d := allow(t, l, ratelimit.Bucket{Key: "org:a:calls", Monthly: true, Limit: 3, Cost: 4}); d.Allowed || !d.Never {
		t.Fatalf("a cost over the limit: %+v", d)
	}
	*clock = next
	if d := allow(t, l, month); !d.Allowed {
		t.Fatalf("a new month: %+v", d)
	}
	if d := allow(t, l, ratelimit.Bucket{Key: "org:a:calls", Monthly: true, Limit: 3, Cost: 2}); !d.Allowed {
		t.Fatalf("a cost of 2 with 2 left: %+v", d)
	}
	if d := allow(t, l, month); d.Allowed {
		t.Fatalf("past the new month's limit: %+v", d)
	}
}

// A refused call takes nothing from a monthly counter either, and Denied names every bucket that
// refused.
func TestMonthlyCounterAllOrNothing(t *testing.T) {
	l, clock := limiter(t)
	// A month whose key Redis' own clock hasn't expired.
	*clock = time.Now()
	month := ratelimit.Bucket{Key: "org:b:calls", Monthly: true, Limit: 2}
	slot := ratelimit.Bucket{Key: "org:b:slots", Slots: 1, TTL: time.Minute}
	held := allow(t, l, month, slot)
	if !held.Allowed {
		t.Fatalf("%+v", held)
	}
	d := allow(t, l, month, slot)
	if d.Allowed || len(d.Denied) != 1 || d.Denied[0] != "org:b:slots" {
		t.Fatalf("the slot is taken: %+v", d)
	}
	if d := allow(t, l, month); !d.Allowed {
		t.Fatalf("the refused call counted: %+v", d)
	}
	if d := allow(t, l, month, slot); d.Allowed || len(d.Denied) != 2 {
		t.Fatalf("both refuse: %+v", d)
	}
}

// Renew extends a held slot past its TTL; a slot already expired isn't taken again (ErrSlotLost).
func TestRenewSlots(t *testing.T) {
	l, clock := limiter(t)
	slot := ratelimit.Bucket{Key: "org:c:calls", Slots: 1, TTL: time.Minute}
	held := allow(t, l, slot)
	if !held.Allowed {
		t.Fatalf("%+v", held)
	}
	*clock = clock.Add(50 * time.Second)
	if err := l.Renew(context.Background(), held); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(50 * time.Second) // 100 s after it was taken, 50 s after the renewal
	if d := allow(t, l, slot); d.Allowed {
		t.Fatalf("the renewed slot expired: %+v", d)
	}
	*clock = clock.Add(11 * time.Second)
	if err := l.Renew(context.Background(), held); !errors.Is(err, ratelimit.ErrSlotLost) {
		t.Fatalf("renewing an expired slot: %v", err)
	}
	if d := allow(t, l, slot); !d.Allowed {
		t.Fatalf("the expired slot: %+v", d)
	}
}

func TestMonthKey(t *testing.T) {
	at := time.Date(2026, 12, 31, 23, 59, 59, 0, time.FixedZone("x", -8*3600)) // already January in UTC
	if got := ratelimit.MonthKey("org:a:calls", at); got != "org:a:calls:2027-01" {
		t.Errorf("%s", got)
	}
	l := ratelimit.New(nil, "x:", slog.New(slog.DiscardHandler))
	if err := l.Renew(context.Background(), ratelimit.Decision{Allowed: true}); err != nil {
		t.Errorf("renewing with no Redis: %v", err)
	}
	d, err := l.Allow(context.Background(), []ratelimit.Bucket{{Key: "k", Monthly: true, Limit: 1}})
	if err != nil || !d.Allowed || !d.FailedOpen {
		t.Errorf("no Redis: %+v %v", d, err)
	}
}
