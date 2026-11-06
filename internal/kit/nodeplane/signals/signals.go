// Package signals carries what nodes report from a product's mgmt to its controller, over Redis
// streams, not the broker: what reacts to a node failing must keep working with RabbitMQ down. A
// stream, so a controller that's restarting misses nothing, and a wake-up on pub/sub, so nothing
// polls. Lifted from databases-stack (9ff61c1; lb-stack's at b63684f), with two streams per product:
// what issues a certificate waits on Private CA for up to 30 s a signal, and nothing else may queue
// behind that.
package signals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

const (
	// MaxBody is the most of a node's frame that may reach Redis. With maxLen, two full streams hold
	// 64 MiB, under Redis's maxmemory, so one node can't take it down for everyone.
	MaxBody = 8 << 10
	// maxLen bounds a stream however long no controller reads it, and however much a node sends. A
	// trimmed signal costs nothing lasting: the controller catches up from the rows and the status
	// hash, and a node asks for its certificate again.
	maxLen = "4096"
	// claimAfter is when a consumer that vanished gives up what it was reading.
	claimAfter = time.Minute
	// handleWithin bounds one signal, so nothing stalls the ones behind it for long.
	handleWithin = 30 * time.Second
	// sweepEvery is how often unfinished reads are tried again, and a vanished replica's claimed.
	// Crash recovery only: a healthy stream goes by the wake-ups.
	sweepEvery = 30 * time.Second
	// maxAttempts is how many times a signal is tried. Then it's dropped with an error: a node
	// that's waiting asks again, and one bad signal must never hold up the rest.
	maxAttempts = 5
)

// Kinds of Signal beyond a node's own frames, whose kind is their frame type.
const (
	// KindSilent: a node stopped sending liveness, or its stream ended.
	KindSilent = "silent"
	// KindLive: a silent node is heard again.
	KindLive = "live"
	// KindSealKey: a node said which key to seal to.
	KindSealKey = "sealKey"
	// KindEnroll: a node's enrollment row wants issuing.
	KindEnroll = "enroll"
	// KindCSR: a node asked for a certificate on its stream (its csr frame).
	KindCSR = "csr"
)

// IssueStream is base's stream for what waits on Private CA or on sealing.
func IssueStream(base string) string { return base + ":issue" }

// issues is whether a kind of signal goes on the issue stream.
func issues(kind string) bool {
	return slices.Contains([]string{KindEnroll, KindSealKey, KindCSR}, kind)
}

// Signal is one thing the controller should hear.
type Signal struct {
	Kind  string          `json:"kind"`
	Scope string          `json:"scope"`
	Node  string          `json:"node"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// ReadStatus is a product's status hash (field: instance, value: JSON of T) for one scope.
func ReadStatus[T any](ctx context.Context, r *redis.Client, key string) (map[string]T, error) {
	reply, err := r.Do(ctx, "HGETALL", key)
	if err != nil {
		return nil, err
	}
	fields, ok := reply.([]any)
	if !ok || len(fields)%2 != 0 {
		return nil, fmt.Errorf("signals: HGETALL answered %v", reply)
	}
	out := make(map[string]T, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		instance, nameOK := fields[i].(string)
		body, bodyOK := fields[i+1].(string)
		if !nameOK || !bodyOK {
			return nil, fmt.Errorf("signals: HGETALL answered %v", reply)
		}
		var s T
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			return nil, fmt.Errorf("signals: instance %s's status: %w", instance, err)
		}
		out[instance] = s
	}
	return out, nil
}

// Publisher sends signals.
type Publisher struct {
	Redis *redis.Client
	// Stream is the product's signals stream (<prefix>:signals). What issues goes on IssueStream(Stream).
	Stream string
}

// Publish adds s to its stream and wakes the controller.
func (p *Publisher) Publish(ctx context.Context, s Signal) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	stream := p.Stream
	if issues(s.Kind) {
		stream = IssueStream(stream)
	}
	if _, err := p.Redis.Do(ctx, "XADD", stream, "MAXLEN", "~", maxLen, "*", "data", string(body)); err != nil {
		return err
	}
	_, err = p.Redis.Publish(ctx, stream+":wake", "")
	return err
}

// Consumer reads one stream in a product's controller, and each replica gets its own share. A
// stream has its own Consumer, and so its own reader.
type Consumer struct {
	Redis *redis.Client
	// Stream is the Publisher's Stream, or IssueStream of it for what issues.
	Stream string
	// Group is the consumer group, the controller's name (kubernetes-controller).
	Group string
	// Name is this replica's.
	Name string
	// Handle applies a signal. An error leaves it unread, for the next pass.
	Handle func(ctx context.Context, s Signal) error
	Logger *slog.Logger
	// SweepEvery and ClaimAfter: zero is 30 s and a minute.
	SweepEvery, ClaimAfter time.Duration
	// OnConnect runs each time the stream is (re)joined, before it's read: what changed while Redis
	// was away reached nobody. nil does nothing.
	OnConnect func(ctx context.Context) error

	attempts map[string]int
}

// Run reads until ctx ends, subscribing again when Redis goes away.
func (c *Consumer) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= time.Minute {
			backoff = time.Second
		}
		c.Logger.Error("signals: disconnected; reconnecting", "error", fmt.Sprint(err), "retrySeconds", backoff.Seconds())
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

func (c *Consumer) session(ctx context.Context) error {
	stream := c.Stream
	sub, err := c.Redis.Subscribe(ctx, stream+":wake")
	if err != nil {
		return err
	}
	defer func() { _ = sub.Close() }()
	// From the start: what a node said before the first controller came up still counts.
	if _, err := c.Redis.Do(ctx, "XGROUP", "CREATE", stream, c.Group, "0", "MKSTREAM"); err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	if c.OnConnect != nil {
		if err := c.OnConnect(ctx); err != nil {
			return err
		}
	}
	if c.attempts == nil {
		c.attempts = map[string]int{}
	}
	if err := c.sweep(ctx); err != nil {
		return err
	}
	swept := time.Now()
	for {
		if err := c.drain(ctx, ">"); err != nil {
			return err
		}
		// By the clock, not on a quiet moment: a busy stream has none.
		if time.Since(swept) >= c.sweepEvery() {
			if err := c.sweep(ctx); err != nil {
				return err
			}
			swept = time.Now()
		}
		wait, cancel := context.WithDeadline(ctx, swept.Add(c.sweepEvery()))
		_, err := sub.Receive(wait)
		timedOut := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if err != nil && !timedOut {
			return err
		}
	}
}

func (c *Consumer) sweepEvery() time.Duration {
	if c.SweepEvery > 0 {
		return c.SweepEvery
	}
	return sweepEvery
}

// sweep takes over what a vanished replica left, following XAUTOCLAIM's cursor, and tries again
// what this replica hasn't finished.
func (c *Consumer) sweep(ctx context.Context) error {
	stream := c.Stream
	idle := c.ClaimAfter
	if idle == 0 {
		idle = claimAfter
	}
	for cursor := "0"; ; {
		reply, err := c.Redis.Do(ctx, "XAUTOCLAIM", stream, c.Group, c.Name, strconv.FormatInt(idle.Milliseconds(), 10), cursor, "COUNT", "256")
		if err != nil {
			return err
		}
		fields, ok := reply.([]any)
		if !ok || len(fields) < 1 {
			return errors.New("signals: an unreadable XAUTOCLAIM reply")
		}
		if cursor, ok = fields[0].(string); !ok || cursor == "0-0" {
			break
		}
	}
	return c.drain(ctx, "0")
}

// drain reads until nothing's left: from "0", what this replica holds unacknowledged, and from ">",
// what's new. A signal that fails stays unread for the next sweep, and the ones behind it go on.
func (c *Consumer) drain(ctx context.Context, from string) error {
	stream := c.Stream
	after := from
	for {
		reply, err := c.Redis.Do(ctx, "XREADGROUP", "GROUP", c.Group, c.Name, "COUNT", "64", "STREAMS", stream, after)
		if err != nil {
			return err
		}
		entries, err := parse(reply)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		for _, e := range entries {
			if from == "0" {
				after = e.id // an id, unlike ">", doesn't move by itself
			}
			if !c.apply(ctx, e) {
				continue
			}
			delete(c.attempts, e.id)
			if _, err := c.Redis.Do(ctx, "XACK", stream, c.Group, e.id); err != nil {
				return err
			}
		}
	}
}

// apply handles one entry, and answers whether it's finished with.
func (c *Consumer) apply(ctx context.Context, e entry) bool {
	var s Signal
	if err := json.Unmarshal([]byte(e.data), &s); err != nil {
		c.Logger.Error("signals: dropping an unreadable signal", "id", e.id, "error", err.Error())
		return true
	}
	bounded, cancel := context.WithTimeout(ctx, handleWithin)
	defer cancel()
	err := c.Handle(bounded, s)
	if err == nil {
		return true
	}
	c.attempts[e.id]++
	if c.attempts[e.id] >= maxAttempts {
		c.Logger.Error("signals: dropping a signal that keeps failing", "kind", s.Kind, "scope", s.Scope, "node", s.Node, "error", err.Error())
		return true
	}
	c.Logger.Warn("signals: a signal failed; it'll be tried again", "kind", s.Kind, "scope", s.Scope, "node", s.Node, "error", err.Error())
	return false
}

type entry struct{ id, data string }

// parse reads XREADGROUP's [[stream, [[id, [field, value...]]...]]], or nil.
func parse(reply any) ([]entry, error) {
	if reply == nil {
		return nil, nil
	}
	bad := errors.New("signals: an unreadable XREADGROUP reply")
	streams, ok := reply.([]any)
	if !ok || len(streams) != 1 {
		return nil, bad
	}
	named, ok := streams[0].([]any)
	if !ok || len(named) != 2 {
		return nil, bad
	}
	rows, ok := named[1].([]any)
	if !ok {
		return nil, bad
	}
	out := make([]entry, 0, len(rows))
	for _, row := range rows {
		fields, ok := row.([]any)
		if !ok || len(fields) != 2 {
			return nil, bad
		}
		id, ok := fields[0].(string)
		if !ok {
			return nil, bad
		}
		e := entry{id: id}
		// An entry trimmed by MAXLEN has no fields.
		if pairs, ok := fields[1].([]any); ok && len(pairs) == 2 {
			e.data, _ = pairs[1].(string)
		}
		out = append(out, e)
	}
	return out, nil
}
