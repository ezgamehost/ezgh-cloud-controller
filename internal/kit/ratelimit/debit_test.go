package ratelimit_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ratelimit"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

// Remaining is each token bucket's whole tokens left after an allowed request, by unprefixed
// key; concurrency buckets and monthly counters aren't in it, and a refusal has none.
func TestDecisionRemaining(t *testing.T) {
	l, clock := limiter(t)
	y, m, _ := time.Now().UTC().Date()
	*clock = time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Minute)
	requests := ratelimit.Bucket{Key: "prj:p:requests", Rate: 600, Window: time.Minute, Burst: 100}
	tokens := ratelimit.Bucket{Key: "org:o:tokens", Rate: 1000, Window: time.Minute, Burst: 1000, Cost: 1}
	slots := ratelimit.Bucket{Key: "org:o:concurrent", Slots: 4, TTL: 90 * time.Second}
	month := ratelimit.Bucket{Key: "org:o:month", Monthly: true, Limit: 100}
	d := allow(t, l, requests, tokens, slots, month)
	if !d.Allowed || len(d.Remaining) != 2 || d.Remaining["prj:p:requests"] != 99 || d.Remaining["org:o:tokens"] != 999 {
		t.Fatalf("%+v", d)
	}
	// Half a request's worth of refill isn't a whole token.
	*clock = clock.Add(50 * time.Millisecond)
	if d = allow(t, l, requests); d.Remaining["prj:p:requests"] != 98 {
		t.Fatalf("%+v", d)
	}
	*clock = clock.Add(100 * time.Millisecond)
	if d = allow(t, l, requests); d.Remaining["prj:p:requests"] != 98 {
		t.Fatalf("after 1.5 tokens of refill: %+v", d)
	}
	if d = allow(t, l, ratelimit.Bucket{Key: "prj:p:requests", Rate: 600, Window: time.Minute, Burst: 100, Cost: 100}); d.Allowed || d.Remaining != nil {
		t.Fatalf("a refusal: %+v", d)
	}
}

func TestDebitDrivesTokenBucketNegative(t *testing.T) {
	l, _ := limiter(t)
	b := ratelimit.Bucket{Key: "org:o:tokens", Rate: 1000, Window: time.Minute, Burst: 1000}
	if d := allow(t, l, b); !d.Allowed || d.Remaining["org:o:tokens"] != 999 {
		t.Fatalf("first allow: %+v", d)
	}
	if err := l.Debit(context.Background(), b, 1500); err != nil {
		t.Fatal(err)
	}
	d := allow(t, l, b)
	if d.Allowed {
		t.Fatal("a bucket debited below zero must refuse until it refills")
	}
	// 502 tokens short (501 below zero, and the request's own) at 1000 a minute: 30.12 s.
	if d.RetryAfter != 30120*time.Millisecond || d.Never {
		t.Fatalf("%+v", d)
	}
}

func TestDebitFloorsAtMinusBurst(t *testing.T) {
	l, clock := limiter(t)
	b := ratelimit.Bucket{Key: "org:o:tokens", Rate: 1000, Window: time.Minute, Burst: 1000}
	if err := l.Debit(context.Background(), b, 1_000_000_000_000); err != nil {
		t.Fatal(err)
	}
	// Floored at -1000: one window back to zero, then a token.
	if d := allow(t, l, b); d.Allowed || d.RetryAfter != 60060*time.Millisecond {
		t.Fatalf("%+v", d)
	}
	*clock = clock.Add(60060 * time.Millisecond)
	if d := allow(t, l, b); !d.Allowed || d.Remaining["org:o:tokens"] != 0 {
		t.Fatalf("refilled: %+v", d)
	}
}

// A debit refills first: tokens that came back since the last request count.
func TestDebitRefillsFirst(t *testing.T) {
	l, clock := limiter(t)
	b := ratelimit.Bucket{Key: "org:o:tokens", Rate: 1000, Window: time.Minute, Burst: 1000, Cost: 1000}
	if d := allow(t, l, b); !d.Allowed || d.Remaining["org:o:tokens"] != 0 {
		t.Fatalf("%+v", d)
	}
	*clock = clock.Add(30 * time.Second) // 500 back
	if err := l.Debit(context.Background(), b, 400); err != nil {
		t.Fatal(err)
	}
	b.Cost = 100
	if d := allow(t, l, b); !d.Allowed || d.Remaining["org:o:tokens"] != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestDebitAddsToMonthlyCounter(t *testing.T) {
	l, clock := limiter(t)
	// The last minute of the real current UTC month, so Redis' own clock hasn't expired its key.
	y, mo, _ := time.Now().UTC().Date()
	next := time.Date(y, mo+1, 1, 0, 0, 0, 0, time.UTC)
	*clock = next.Add(-time.Minute)
	m := ratelimit.Bucket{Key: "org:o:month", Monthly: true, Limit: 100}
	if err := l.Debit(context.Background(), m, 99); err != nil {
		t.Fatal(err)
	}
	if d := allow(t, l, m); !d.Allowed {
		t.Fatal("99 of 100 used: one more is allowed")
	}
	if d := allow(t, l, m); d.Allowed || d.RetryAfter != time.Minute {
		t.Fatalf("100 of 100 used: refused until the month ends: %+v", d)
	}
	at, err := l.Redis.Do(context.Background(), "PEXPIRETIME", l.Prefix+ratelimit.MonthKey(m.Key, *clock))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := at.(int64); got != next.UnixMilli() {
		t.Fatalf("the month's key expires at %d", got)
	}
	// A debit past the limit is kept, not refused: the request was already served.
	if err := l.Debit(context.Background(), m, 50); err != nil {
		t.Fatal(err)
	}
	if n, _ := l.Redis.Do(context.Background(), "GET", l.Prefix+ratelimit.MonthKey(m.Key, *clock)); n != "150" {
		t.Fatalf("count %v", n)
	}
	*clock = next
	if d := allow(t, l, m); !d.Allowed {
		t.Fatalf("a new month: %+v", d)
	}
}

func TestDebitRefusesConcurrencyBucketsAndInvalidOnes(t *testing.T) {
	l, _ := limiter(t)
	if err := l.Debit(context.Background(), ratelimit.Bucket{Key: "s", Slots: 2, TTL: time.Second}, 1); err == nil {
		t.Fatal("Debit on a concurrency bucket is a caller bug")
	}
	if err := l.Debit(context.Background(), ratelimit.Bucket{Key: "t", Rate: 0, Window: time.Second, Burst: 1}, 1); err == nil {
		t.Fatal("an invalid bucket is a caller bug")
	}
	if err := l.Debit(context.Background(), ratelimit.Bucket{Key: "t", Rate: 1, Window: time.Second, Burst: 1}, -5); err == nil {
		t.Fatal("a negative amount is a caller bug")
	}
	// Nothing to debit: nothing is written.
	b := ratelimit.Bucket{Key: "t", Rate: 1, Window: time.Second, Burst: 1}
	if err := l.Debit(context.Background(), b, 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := l.Redis.Do(context.Background(), "EXISTS", l.Prefix+"t"); n != int64(0) {
		t.Fatalf("a zero debit wrote the bucket: %v", n)
	}
}

func TestHeldCountsLiveSlots(t *testing.T) {
	l, clock := limiter(t)
	s := ratelimit.Bucket{Key: "backend:vllm-1", Slots: 8, TTL: time.Minute}
	d1 := allow(t, l, s)
	allow(t, l, s)
	held, err := l.Held(context.Background(), []string{"backend:vllm-1", "backend:vllm-2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 2 || held["backend:vllm-1"] != 2 || held["backend:vllm-2"] != 0 {
		t.Fatalf("%v", held)
	}
	if err := l.Release(context.Background(), d1); err != nil {
		t.Fatal(err)
	}
	if held, _ = l.Held(context.Background(), []string{"backend:vllm-1"}); held["backend:vllm-1"] != 1 {
		t.Fatalf("after a release: %v", held)
	}
	// A slot past its TTL isn't held, though nothing has removed it yet.
	*clock = clock.Add(time.Minute)
	if held, _ = l.Held(context.Background(), []string{"backend:vllm-1"}); held["backend:vllm-1"] != 0 {
		t.Fatalf("after the TTL: %v", held)
	}
	if held, err = l.Held(context.Background(), nil); err != nil || len(held) != 0 {
		t.Fatalf("no keys: %v %v", held, err)
	}
}

func TestDebitAndHeldFailOpenWithoutRedis(t *testing.T) {
	var logs strings.Builder
	l := ratelimit.New(nil, "x:", slog.New(slog.NewTextHandler(&logs, nil)))
	if err := l.Debit(context.Background(), ratelimit.Bucket{Key: "t", Rate: 1, Window: time.Second, Burst: 1}, 5); err != nil {
		t.Fatal(err)
	}
	// A debit that wasn't taken is counted as its own reason, and not as a request: Total and the
	// other reasons keep counting requests allowed unchecked.
	if l.FailOpens.Count(ratelimit.ReasonDebit) != 1 || l.FailOpens.Count(ratelimit.ReasonUnconfigured) != 0 || l.FailOpens.Total() != 0 {
		t.Fatalf("debit %d unconfigured %d total %d", l.FailOpens.Count(ratelimit.ReasonDebit), l.FailOpens.Count(ratelimit.ReasonUnconfigured), l.FailOpens.Total())
	}
	if !strings.Contains(logs.String(), "debit") || strings.Contains(logs.String(), "allowing the request") {
		t.Fatalf("log: %s", logs.String())
	}
	var metric strings.Builder
	if err := l.FailOpens.WritePrometheus(&metric); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metric.String(), `ratelimit_fail_open_total{reason="debit"} 1`) {
		t.Fatalf("metric:\n%s", metric.String())
	}
	held, err := l.Held(context.Background(), []string{"k"})
	if err == nil || held != nil {
		t.Fatal("Held without Redis is an error the caller handles (routing falls back to plain HRW)")
	}

	c, err := redis.New("redis://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	down := ratelimit.New(c, "x:", slog.New(slog.DiscardHandler))
	if err := down.Debit(context.Background(), ratelimit.Bucket{Key: "t", Rate: 1, Window: time.Second, Burst: 1}, 5); err != nil {
		t.Fatal(err)
	}
	if down.FailOpens.Count(ratelimit.ReasonDebit) != 1 || down.FailOpens.Total() != 0 {
		t.Fatal("a debit Redis didn't take counts as a debit fail-open")
	}
	if held, err := down.Held(context.Background(), []string{"k"}); err == nil || held != nil {
		t.Fatalf("Held with Redis down: %v %v", held, err)
	}
}

// A monthly key that only Debit ever wrote still expires when its UTC month ends.
func TestDebitAloneSetsTheMonthsExpiry(t *testing.T) {
	l, clock := limiter(t)
	y, mo, _ := time.Now().UTC().Date()
	next := time.Date(y, mo+1, 1, 0, 0, 0, 0, time.UTC)
	*clock = next.Add(-time.Hour)
	m := ratelimit.Bucket{Key: "org:o:month", Monthly: true, Limit: 100}
	if err := l.Debit(context.Background(), m, 7); err != nil {
		t.Fatal(err)
	}
	key := l.Prefix + ratelimit.MonthKey(m.Key, *clock)
	at, err := l.Redis.Do(context.Background(), "PEXPIRETIME", key)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := at.(int64); got != next.UnixMilli() {
		t.Fatalf("the month's key expires at %d, want %d", got, next.UnixMilli())
	}
	if n, _ := l.Redis.Do(context.Background(), "GET", key); n != "7" {
		t.Fatalf("count %v", n)
	}
}

// A debited token bucket lives until it would have refilled (two windows from -Burst), then a
// second more, so its debt isn't forgotten early.
func TestDebitedBucketLivesUntilRefilled(t *testing.T) {
	l, _ := limiter(t)
	b := ratelimit.Bucket{Key: "org:o:tokens", Rate: 1000, Window: time.Minute, Burst: 1000}
	if err := l.Debit(context.Background(), b, 5000); err != nil {
		t.Fatal(err)
	}
	ttl, err := l.Redis.Do(context.Background(), "PTTL", l.Prefix+b.Key)
	if err != nil {
		t.Fatal(err)
	}
	// From -1000 to +1000 at 1000 a minute: 120 s, plus the script's second.
	if ms, _ := ttl.(int64); ms <= 120_000 || ms > 121_000 {
		t.Fatalf("PTTL %d ms", ms)
	}
	if err := l.Debit(context.Background(), b, 1); err != nil {
		t.Fatal(err)
	}
	if ttl, _ := l.Redis.Do(context.Background(), "PTTL", l.Prefix+b.Key); ttl.(int64) <= 120_000 {
		t.Fatalf("a floored bucket's PTTL %v", ttl)
	}
}
