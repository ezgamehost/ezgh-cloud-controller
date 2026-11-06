// Package ratelimit is a product's own rate limits (contracts/api-conventions.md, "Rate limits"):
// token buckets, concurrency slots and monthly counters in Redis, checked and taken together by
// one Lua script, so a request takes from every bucket or from none. The counters are shared by every replica and
// approximate by design; anything that must be exact is a quota. A data plane uses it for the
// throughput customers plan around (ingest bytes and requests, concurrent searches) without
// calling quotas-api.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

// Bucket is one limit a request draws from: a token bucket, a concurrency bucket (Slots > 0), or a
// monthly counter (Monthly).
type Bucket struct {
	// Key names the bucket under the Limiter's Prefix: "org:<id>:bytes".
	Key string
	// A token bucket: Rate tokens are added every Window, up to Burst, and the request takes Cost
	// (0 counts as 1).
	Rate   int64
	Window time.Duration
	Burst  int64
	Cost   int64
	// A concurrency bucket: the request holds one of Slots until Release, or for TTL at most (Renew
	// extends it, for long requests).
	Slots int64
	TTL   time.Duration
	// A monthly counter: the request adds Cost (0 counts as 1) to this UTC month's count, and is
	// refused when that would pass Limit. The count starts again at 0 at the start of each UTC
	// month (a fixed window: INCRBY on a key for the month, PEXPIREAT its end). A spending guard,
	// not a billing period.
	Monthly bool
	Limit   int64
}

func (b Bucket) concurrency() bool { return b.Slots > 0 }

func (b Bucket) cost() int64 {
	if b.Cost == 0 {
		return 1
	}
	return b.Cost
}

// maxExact is the largest integer Redis' Lua numbers (doubles) hold exactly. The script counts in
// token-milliseconds (tokens × the window in ms), so refill and Retry-After stay exact.
const maxExact = 1 << 53

func (b Bucket) validate() error {
	if b.Key == "" {
		return errors.New("ratelimit: a bucket needs a key")
	}
	if b.Monthly {
		if b.Rate != 0 || b.Window != 0 || b.Burst != 0 || b.Slots != 0 || b.TTL != 0 {
			return fmt.Errorf("ratelimit: %s: a monthly counter has Limit and Cost only", b.Key)
		}
		if b.Limit < 1 || b.Cost < 0 || b.Limit >= maxExact || b.cost() >= maxExact {
			return fmt.Errorf("ratelimit: %s: Limit must be at least 1, and Cost at least 0, both below 2^53", b.Key)
		}
		return nil
	}
	if b.Limit != 0 {
		return fmt.Errorf("ratelimit: %s: only a monthly counter has a Limit", b.Key)
	}
	if b.concurrency() {
		if b.Rate != 0 || b.Window != 0 || b.Burst != 0 || b.Cost != 0 {
			return fmt.Errorf("ratelimit: %s: a concurrency bucket has Slots and TTL only", b.Key)
		}
		if b.TTL < time.Millisecond || b.TTL%time.Millisecond != 0 {
			return fmt.Errorf("ratelimit: %s: TTL must be whole milliseconds, at least 1", b.Key)
		}
		return nil
	}
	if b.TTL != 0 {
		return fmt.Errorf("ratelimit: %s: only a concurrency bucket has a TTL", b.Key)
	}
	if b.Rate < 1 || b.Burst < 1 || b.Cost < 0 {
		return fmt.Errorf("ratelimit: %s: Rate and Burst must be at least 1, and Cost at least 0", b.Key)
	}
	if b.Window < time.Millisecond || b.Window%time.Millisecond != 0 {
		return fmt.Errorf("ratelimit: %s: Window must be whole milliseconds, at least 1", b.Key)
	}
	w := b.Window.Milliseconds()
	if b.Burst > maxExact/w || b.cost() > maxExact/w || b.Rate > maxExact/w {
		return fmt.Errorf("ratelimit: %s: Burst, Cost and Rate times the window in milliseconds must stay below 2^53", b.Key)
	}
	return nil
}

// Decision is Allow's answer.
type Decision struct {
	Allowed bool
	// RetryAfter is how long until the request could be allowed: exact to the millisecond for
	// token buckets, and at most a second for a full concurrency bucket. 0 when allowed.
	RetryAfter time.Duration
	// Never is true when a token bucket's Cost is more than its Burst (or a monthly counter's
	// Cost more than its Limit): no wait helps.
	Never bool
	// FailedOpen is true when Redis didn't answer and the request was allowed unchecked. Its
	// slots may still have been taken (a reply slower than the timeout), so Release it as usual.
	FailedOpen bool
	// Denied are the Keys of the buckets that refused, in the order given: a product answers a
	// monthly counter's refusal differently from a rate's (MCP Central: quota_exceeded, not
	// rate_limited).
	Denied []string
	// Remaining is, for an allowed request, each token bucket's whole tokens left after it took
	// its Cost, by the bucket's Key (unprefixed): what a product reports in rate-limit headers
	// (EZGH Inference's x-ratelimit-remaining-*). Concurrency buckets and monthly counters aren't
	// in it. nil when refused, when it failed open, and when no token bucket was checked.
	Remaining map[string]int64

	// The concurrency slots an allowed request holds, for Release and Renew, with their TTLs.
	slots    []string
	slotTTLs []int64
	member   string
}

// Limiter checks buckets in one Redis.
type Limiter struct {
	// Redis is the product's rate-limit Redis.
	Redis *redis.Client
	// Prefix starts every key (REDIS_KEY_PREFIX + "ratelimit:", say), so products and tests don't
	// share buckets.
	Prefix string
	// Timeout bounds each Redis call. Default 50 ms.
	Timeout time.Duration
	// Now is the clock buckets refill by. Default time.Now. Replicas' clocks may differ a little:
	// a bucket never refills backwards.
	Now func() time.Time
	// FailOpens counts and logs requests allowed without an answer from Redis.
	FailOpens *FailOpens
}

// New is a Limiter with the defaults.
func New(r *redis.Client, prefix string, logger *slog.Logger) *Limiter {
	return &Limiter{Redis: r, Prefix: prefix, FailOpens: &FailOpens{Logger: logger}}
}

// discard counts fail-opens for a Limiter built without New.
var discard = &FailOpens{}

func (l *Limiter) failOpens() *FailOpens {
	if l.FailOpens != nil {
		return l.FailOpens
	}
	return discard
}

func (l *Limiter) timeout() time.Duration {
	if l.Timeout > 0 {
		return l.Timeout
	}
	return 50 * time.Millisecond
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// script checks every bucket and, only if all allow, takes from each. ARGV[1] is now in ms and
// ARGV[2] the slot member; then five values per KEYS[i]: "t", rate, window ms, burst, cost for a
// token bucket, "s", slots, TTL ms, 0, 0 for a concurrency bucket, or "m", limit, the month's end
// in ms, cost, 0 for a monthly counter (its key names the month). It answers
// {allowed, retry ms, never, the 1-based index of each bucket that refused…} when refused, and
// {1, 0, 0, each bucket's whole tokens left (-1 for a bucket that isn't a token bucket)…} when
// allowed.
var script = redis.NewScript(`
local now = tonumber(ARGV[1])
local member = ARGV[2]
local never, retry = 0, 0
local refused = {}
local plans = {}
for i, key in ipairs(KEYS) do
  local o = 2 + (i - 1) * 5
  local kind = ARGV[o + 1]
  local a, b, c, d = tonumber(ARGV[o + 2]), tonumber(ARGV[o + 3]), tonumber(ARGV[o + 4]), tonumber(ARGV[o + 5])
  local denied = false
  if kind == 't' then
    local cap, need = c * b, d * b
    local state = redis.call('HMGET', key, 'l', 't')
    local level, at = tonumber(state[1]), tonumber(state[2])
    if level == nil or at == nil then
      level, at = cap, now
    end
    if now > at then
      level = math.min(cap, level + (now - at) * a)
      at = now
    end
    if need > cap then
      denied, never = true, 1
    elseif level < need then
      denied = true
      local wait = math.ceil((need - level) / a)
      if wait > retry then retry = wait end
    end
    plans[i] = {'t', level - need, at, cap, a, b}
  elseif kind == 'm' then
    local count = tonumber(redis.call('GET', key) or '0')
    if c > a then
      denied, never = true, 1
    elseif count + c > a then
      denied = true
      local wait = math.max(b - now, 1)
      if wait > retry then retry = wait end
    end
    plans[i] = {'m', c, b}
  else
    redis.call('ZREMRANGEBYSCORE', key, '-inf', now)
    if redis.call('ZCARD', key) >= a then
      denied = true
      local first = redis.call('ZRANGE', key, 0, 0, 'WITHSCORES')
      local wait = math.min(math.max(tonumber(first[2]) - now, 1), 1000)
      if wait > retry then retry = wait end
    end
    plans[i] = {'s', b}
  end
  if denied then refused[#refused + 1] = i end
end
if #refused > 0 then
  local answer = {0, retry, never}
  for _, i in ipairs(refused) do answer[#answer + 1] = i end
  return answer
end
local answer = {1, 0, 0}
for i, key in ipairs(KEYS) do
  local p = plans[i]
  answer[3 + i] = -1
  if p[1] == 't' then
    redis.call('HSET', key, 'l', string.format('%d', p[2]), 't', string.format('%d', p[3]))
    redis.call('PEXPIRE', key, math.ceil((p[4] - p[2]) / p[5]) + 1000)
    answer[3 + i] = math.floor(p[2] / p[6])
  elseif p[1] == 'm' then
    redis.call('INCRBY', key, p[2])
    redis.call('PEXPIREAT', key, p[3])
  else
    redis.call('ZADD', key, now + p[2], member)
    -- The set lives as long as its longest lease: a shorter TTL never cuts a longer one short.
    local last = redis.call('ZRANGE', key, -1, -1, 'WITHSCORES')
    redis.call('PEXPIRE', key, tonumber(last[2]) - now + 1000)
  end
end
return answer
`)

// renewScript extends the slots a request holds: ARGV[1] is now in ms and ARGV[2] the member,
// then one TTL in ms per KEYS[i]. A slot that's gone (expired and taken back) isn't added again.
// It answers how many slots were gone.
var renewScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local member = ARGV[2]
local gone = 0
for i, key in ipairs(KEYS) do
  local ttl = tonumber(ARGV[2 + i])
  local held = redis.call('ZSCORE', key, member)
  if held == false or tonumber(held) <= now then
    gone = gone + 1
  else
    redis.call('ZADD', key, 'XX', now + ttl, member)
    local last = redis.call('ZRANGE', key, -1, -1, 'WITHSCORES')
    redis.call('PEXPIRE', key, tonumber(last[2]) - now + 1000)
  end
end
return gone
`)

// ErrSlotLost is Renew's answer when a slot expired before it was renewed: another request may
// hold it now, and the caller is over its concurrency limit until it finishes.
var ErrSlotLost = errors.New("ratelimit: a concurrency slot expired before it was renewed")

// MonthKey is a monthly counter's key for the UTC month of now: key + ":" + YYYY-MM.
func MonthKey(key string, now time.Time) string { return key + ":" + now.UTC().Format("2006-01") }

// monthEnd is the start of the next UTC month: when a monthly counter's count starts again.
func monthEnd(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m+1, 1, 0, 0, 0, 0, time.UTC)
}

// Allow checks every bucket and, only if all of them allow, takes Cost from each token bucket and
// a slot from each concurrency bucket, in one script. A refusal takes nothing. An invalid bucket
// is an error (the caller's bug); Redis failing never is: the request is allowed
// (Decision.FailedOpen), counted in ratelimit_fail_open_total.
func (l *Limiter) Allow(ctx context.Context, buckets []Bucket) (Decision, error) {
	seen := map[string]bool{}
	for _, b := range buckets {
		if err := b.validate(); err != nil {
			return Decision{}, err
		}
		if seen[b.Key] {
			return Decision{}, fmt.Errorf("ratelimit: bucket %s is listed twice", b.Key)
		}
		seen[b.Key] = true
	}
	if len(buckets) == 0 {
		return Decision{Allowed: true}, nil
	}
	member := ids.NewV7()
	now := l.now()
	keys := make([]string, len(buckets))
	args := make([]string, 0, 2+5*len(buckets))
	args = append(args, strconv.FormatInt(now.UnixMilli(), 10), member)
	var slots []string
	var slotTTLs []int64
	for i, b := range buckets {
		keys[i] = l.Prefix + b.Key
		if b.Monthly {
			keys[i] = l.Prefix + MonthKey(b.Key, now)
			args = append(args, "m", strconv.FormatInt(b.Limit, 10), strconv.FormatInt(monthEnd(now).UnixMilli(), 10), strconv.FormatInt(b.cost(), 10), "0")
			continue
		}
		if b.concurrency() {
			args = append(args, "s", strconv.FormatInt(b.Slots, 10), strconv.FormatInt(b.TTL.Milliseconds(), 10), "0", "0")
			slots = append(slots, keys[i])
			slotTTLs = append(slotTTLs, b.TTL.Milliseconds())
			continue
		}
		args = append(args, "t", strconv.FormatInt(b.Rate, 10), strconv.FormatInt(b.Window.Milliseconds(), 10), strconv.FormatInt(b.Burst, 10), strconv.FormatInt(b.cost(), 10))
	}
	if l.Redis == nil {
		l.failOpens().Record(ReasonUnconfigured, nil)
		return Decision{Allowed: true, FailedOpen: true}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()
	reply, err := l.Redis.Eval(ctx, script, keys, args...)
	// From here the script may have run even when no answer came back (a reply slower than the
	// timeout), taking the request's slots. A fail-open Decision keeps them and the member, so
	// Release and Renew reach them; removing a slot that was never taken is harmless.
	failedOpen := Decision{Allowed: true, FailedOpen: true, slots: slots, slotTTLs: slotTTLs, member: member}
	if err != nil {
		reason := ReasonUnavailable
		var replyErr redis.ReplyError
		if errors.As(err, &replyErr) {
			reason = ReasonError
		}
		l.failOpens().Record(reason, err)
		return failedOpen, nil
	}
	values, ok := reply.([]any)
	if !ok || len(values) < 3 {
		l.failOpens().Record(ReasonError, fmt.Errorf("ratelimit: unexpected reply %v", reply))
		return failedOpen, nil
	}
	l.failOpens().Answered()
	allowed, _ := values[0].(int64)
	retry, _ := values[1].(int64)
	never, _ := values[2].(int64)
	if allowed == 1 {
		d := Decision{Allowed: true, slots: slots, slotTTLs: slotTTLs, member: member}
		for i, v := range values[3:] {
			if left, ok := v.(int64); ok && left >= 0 && i < len(buckets) {
				if d.Remaining == nil {
					d.Remaining = map[string]int64{}
				}
				d.Remaining[buckets[i].Key] = left
			}
		}
		return d, nil
	}
	d := Decision{RetryAfter: time.Duration(retry) * time.Millisecond, Never: never == 1}
	for _, v := range values[3:] {
		if i, ok := v.(int64); ok && i >= 1 && int(i) <= len(buckets) {
			d.Denied = append(d.Denied, buckets[i-1].Key)
		}
	}
	return d, nil
}

// debitScript takes amount from a token bucket after the fact, refilling it first, never below
// -Burst (so a request it refuses waits at most (Burst + its Cost) × Window / Rate), or adds
// amount to a monthly counter. ARGV[1] is now in ms, then kind, a, b, c, amount: "t", rate, window ms, burst,
// amount for a token bucket; "m", 0, the month's end in ms, 0, amount for a monthly counter (its
// key names the month).
var debitScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local kind, a, b, c, amount = ARGV[2], tonumber(ARGV[3]), tonumber(ARGV[4]), tonumber(ARGV[5]), tonumber(ARGV[6])
local key = KEYS[1]
if kind == 'm' then
  redis.call('INCRBY', key, amount)
  redis.call('PEXPIREAT', key, b)
  return 0
end
local cap = c * b
local state = redis.call('HMGET', key, 'l', 't')
local level, at = tonumber(state[1]), tonumber(state[2])
if level == nil or at == nil then
  level, at = cap, now
end
if now > at then
  level = math.min(cap, level + (now - at) * a)
  at = now
end
level = math.max(-cap, level - amount * b)
redis.call('HSET', key, 'l', string.format('%d', level), 't', string.format('%d', at))
redis.call('PEXPIRE', key, math.ceil((cap - level) / a) + 1000)
return 0
`)

// Debit takes amount from a token bucket after the request it measures, or adds it to a monthly
// counter: a cost known only from the response (EZGH Inference's prompt tokens). A token bucket
// may go below zero, down to -Burst, and later requests wait for it to refill; a monthly counter
// may pass its Limit. A monthly debit counts in the UTC month the debit happens (Now when Debit
// runs), not the month the request was admitted in: a request straddling midnight at a month's
// end is charged to the new month. A concurrency bucket, an invalid bucket or a negative amount
// is an error (the caller's bug); an amount of 0 does nothing. Redis failing isn't an error: it's
// counted under ReasonDebit (not as a request), since a limit is protection and the request has
// already been served.
func (l *Limiter) Debit(ctx context.Context, b Bucket, amount int64) error {
	if err := b.validate(); err != nil {
		return err
	}
	if b.concurrency() {
		return fmt.Errorf("ratelimit: %s: Debit takes a token bucket or a monthly counter", b.Key)
	}
	if amount < 0 {
		return fmt.Errorf("ratelimit: %s: can't debit a negative amount", b.Key)
	}
	if amount == 0 {
		return nil
	}
	if l.Redis == nil {
		l.failOpens().RecordDebit(nil)
		return nil
	}
	now := l.now()
	key := l.Prefix + b.Key
	args := []string{strconv.FormatInt(now.UnixMilli(), 10)}
	if b.Monthly {
		// Past the Limit, the count only has to stay past it: keep it exact in Lua's doubles.
		amount = min(amount, b.Limit)
		key = l.Prefix + MonthKey(b.Key, now)
		args = append(args, "m", "0", strconv.FormatInt(monthEnd(now).UnixMilli(), 10), "0", strconv.FormatInt(amount, 10))
	} else {
		// No more than takes a full bucket to -Burst, so the token-milliseconds stay exact.
		amount = min(amount, 2*b.Burst)
		args = append(args, "t", strconv.FormatInt(b.Rate, 10), strconv.FormatInt(b.Window.Milliseconds(), 10), strconv.FormatInt(b.Burst, 10), strconv.FormatInt(amount, 10))
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()
	if _, err := l.Redis.Eval(ctx, debitScript, []string{key}, args...); err != nil {
		l.failOpens().RecordDebit(err)
		return nil
	}
	l.failOpens().Answered()
	return nil
}

// heldScript counts each KEYS[i]'s slots that haven't expired at ARGV[1] (now in ms).
var heldScript = redis.NewScript(`
local now = ARGV[1]
local out = {}
for i, key in ipairs(KEYS) do
  out[i] = redis.call('ZCOUNT', key, '(' .. now, '+inf')
end
return out
`)

// Held is how many live slots each concurrency bucket holds now, by Key (unprefixed), in one
// round trip: a load signal (EZGH Inference's router reads its backends' slots). A key nobody
// holds is 0. Without Redis, or with Redis down, it's an error: the caller decides how to do
// without (the router falls back to plain hashing).
func (l *Limiter) Held(ctx context.Context, keys []string) (map[string]int64, error) {
	if l.Redis == nil {
		return nil, errors.New("ratelimit: no Redis configured")
	}
	out := make(map[string]int64, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	prefixed := make([]string, len(keys))
	for i, k := range keys {
		prefixed[i] = l.Prefix + k
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()
	reply, err := l.Redis.Eval(ctx, heldScript, prefixed, strconv.FormatInt(l.now().UnixMilli(), 10))
	if err != nil {
		return nil, fmt.Errorf("ratelimit: counting slots: %w", err)
	}
	counts, ok := reply.([]any)
	if !ok || len(counts) != len(keys) {
		return nil, fmt.Errorf("ratelimit: counting slots: unexpected reply %v", reply)
	}
	for i, k := range keys {
		n, _ := counts[i].(int64)
		out[k] = n
	}
	return out, nil
}

// Renew extends the concurrency slots an allowed Decision holds by their buckets' TTLs from now,
// for a request that runs longer than a TTL (MCP Central's tool calls, up to the route timeout).
// A slot that already expired isn't taken again: ErrSlotLost, except for a Decision that failed
// open, whose slots may never have been taken (it renews those that were, and returns nil). Redis failing is its error; the
// request carries on (rate limits fail open), and the slot may expire under it.
func (l *Limiter) Renew(ctx context.Context, d Decision) error {
	if l.Redis == nil || len(d.slots) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()
	args := []string{strconv.FormatInt(l.now().UnixMilli(), 10), d.member}
	for _, ttl := range d.slotTTLs {
		args = append(args, strconv.FormatInt(ttl, 10))
	}
	reply, err := l.Redis.Eval(ctx, renewScript, d.slots, args...)
	if err != nil {
		return fmt.Errorf("ratelimit: renewing slots: %w", err)
	}
	if gone, _ := reply.(int64); gone > 0 && !d.FailedOpen {
		return ErrSlotLost
	}
	return nil
}

// Release gives back the concurrency slots an allowed Decision holds. Call it when the request is
// done, also when the Decision failed open: Redis may have taken the slots without answering in
// time, and the release gives them back once it answers. A slot that isn't released expires after
// its bucket's TTL.
func (l *Limiter) Release(ctx context.Context, d Decision) error {
	if l.Redis == nil || len(d.slots) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout())
	defer cancel()
	var errs []error
	for _, key := range d.slots {
		if _, err := l.Redis.Do(ctx, "ZREM", key, d.member); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RetryAfterSeconds is a Retry-After header's value for d: whole seconds, rounded up, 1 to 3600.
func RetryAfterSeconds(d time.Duration) int {
	s := int((d + time.Second - 1) / time.Second)
	return min(max(s, 1), 3600)
}
