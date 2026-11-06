package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/wire"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

// Presence says which pod holds which node's stream. Redis keeps no copy across a restart, so each
// pod puts its streams back whenever it subscribes (Bus.Run's onConnect), then catches them up.
type Presence struct {
	Redis *redis.Client
	// Prefix starts every key: the product's namespace (databases, kubernetes).
	Prefix string
}

func (p *Presence) key(scope string) string { return p.Prefix + ":presence:" + scope }

// Set records that pod holds node's stream.
func (p *Presence) Set(ctx context.Context, scope, node, pod string) error {
	_, err := p.Redis.Do(ctx, "HSET", p.key(scope), node, pod)
	return err
}

// SetNew records that pod holds node's stream, unless a pod is already listed: Rebuild's, whose
// stream may be a dead one for a node that has since moved.
func (p *Presence) SetNew(ctx context.Context, scope, node, pod string) error {
	_, err := p.Redis.Do(ctx, "HSETNX", p.key(scope), node, pod)
	return err
}

// storeIfHeld is atomic with a reconnect: a pod that has lost a node can't write over what the new
// pod just read.
var storeIfHeld = redis.NewScript(`local holder = redis.call('HGET', KEYS[1], ARGV[1])
if holder and holder ~= ARGV[2] then return 0 end
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
return 1`)

// Store writes status, JSON, as node's field of the hash statusKey (the product's status hash for
// scope), and answers false if another pod holds the node: then pod's stream is a dead one, and has
// nothing to say.
func (p *Presence) Store(ctx context.Context, scope, node, pod, statusKey string, status any) (bool, error) {
	body, err := json.Marshal(status)
	if err != nil {
		return false, err
	}
	reply, err := p.Redis.Eval(ctx, storeIfHeld, []string{p.key(scope), statusKey}, node, pod, string(body))
	return reply == int64(1), err
}

// clearIfHeld deletes a stream's entry only while pod still holds it: a node that has since
// reconnected to another pod stays visible.
var clearIfHeld = redis.NewScript(`if redis.call('HGET', KEYS[1], ARGV[1]) == ARGV[2] then return redis.call('HDEL', KEYS[1], ARGV[1]) end return 0`)

// Clear forgets node's stream on pod, and answers whether pod still held it.
func (p *Presence) Clear(ctx context.Context, scope, node, pod string) (bool, error) {
	reply, err := p.Redis.Eval(ctx, clearIfHeld, []string{p.key(scope)}, node, pod)
	return reply == int64(1), err
}

// Holders are scope's connected nodes, with the pod holding each.
func (p *Presence) Holders(ctx context.Context, scope string) (map[string]string, error) {
	reply, err := p.Redis.Do(ctx, "HGETALL", p.key(scope))
	if err != nil {
		return nil, err
	}
	fields, ok := reply.([]any)
	if !ok || len(fields)%2 != 0 {
		return nil, fmt.Errorf("delivery: HGETALL answered %v", reply)
	}
	out := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		node, nodeOK := fields[i].(string)
		pod, podOK := fields[i+1].(string)
		if !nodeOK || !podOK {
			return nil, fmt.Errorf("delivery: HGETALL answered %v", reply)
		}
		out[node] = pod
	}
	return out, nil
}

// Handoff is a generation for a holding pod to bring its nodes up to.
type Handoff struct {
	Scope string `json:"scope"`
	// Nodes are the ones this pod is listed as holding. One it doesn't hold is a stale entry, and
	// it says so, or a pointer would wait on it for nothing.
	Nodes      []string `json:"nodes"`
	Generation int64    `json:"generation"`
}

// NodeFrame is a frame for one node's stream. Frame is wire.Encode's output, with any seq: the
// stream numbers what it writes.
type NodeFrame struct {
	Scope string          `json:"scope"`
	Node  string          `json:"node"`
	Frame json.RawMessage `json:"frame"`
}

// podMessage is what travels on a pod's channel: one of these.
type podMessage struct {
	Handoff *Handoff   `json:"handoff,omitempty"`
	Frame   *NodeFrame `json:"frame,omitempty"`
}

// Ack is a node's state after a handoff, from the pod holding it: its applied generation, or Gone
// when its stream ended, since a node with no connection needs nothing. Every handoff gets one for
// every node on the pod, even a node that's already current.
type Ack struct {
	Scope      string `json:"scope"`
	Node       string `json:"node"`
	Generation int64  `json:"generation"`
	Gone       bool   `json:"gone"`
}

// acksChannel is one channel for every pod: a pod ignores acks it isn't waiting for.
func (b *Bus) acksChannel() string { return b.Prefix + ":acks" }

func (b *Bus) podChannel(pod string) string { return b.Prefix + ":pod:" + pod }

func (b *Bus) aliveKey(pod string) string { return b.Prefix + ":alive:" + pod }

// A pod refreshes its liveness key often enough to survive two misses.
const (
	aliveEvery = 10 * time.Second
	aliveFor   = 30 * time.Second
)

// Bus carries handoffs and acks between a product's mgmt pods, by Redis pub/sub.
type Bus struct {
	Redis *redis.Client
	// Prefix starts every key and channel, as Presence's.
	Prefix string
	// Pod is this pod's name.
	Pod    string
	Logger *slog.Logger
}

// Handoff tells pod about h, and answers false only when the pod is gone. Nothing receiving it
// isn't proof: a live pod can be between subscriptions, and only its liveness key's expiry is.
func (b *Bus) Handoff(ctx context.Context, pod string, h Handoff) (bool, error) {
	body, err := json.Marshal(podMessage{Handoff: &h})
	if err != nil {
		return false, err
	}
	n, err := b.Redis.Publish(ctx, b.podChannel(pod), string(body))
	if err != nil || n > 0 {
		return n > 0, err
	}
	reply, err := b.Redis.Do(ctx, "EXISTS", b.aliveKey(pod))
	if err != nil {
		return false, err
	}
	return reply != int64(0), nil
}

// SendFrame gives pod a frame for a node it holds, and answers whether anything received it. What
// a lost frame carries comes again: a retried command or renewal, or a seal on connect.
func (b *Bus) SendFrame(ctx context.Context, pod string, f NodeFrame) (bool, error) {
	body, err := json.Marshal(podMessage{Frame: &f})
	if err != nil {
		return false, err
	}
	n, err := b.Redis.Publish(ctx, b.podChannel(pod), string(body))
	return n > 0, err
}

// ErrNotConnected is a node with no stream, or whose pod didn't hear.
var ErrNotConnected = errors.New("delivery: the node isn't connected")

// Frames is how the controller and the API reach connected nodes.
type Frames interface {
	Send(ctx context.Context, scope, node string, m wire.Message) error
}

// Sender is Frames over Presence and the Bus.
type Sender struct {
	Presence *Presence
	Bus      *Bus
}

// Send hands a frame to the pod holding a node's stream, and answers ErrNotConnected if nothing got it.
func (s *Sender) Send(ctx context.Context, scope, node string, m wire.Message) error {
	holders, err := s.Presence.Holders(ctx, scope)
	if err != nil {
		return err
	}
	pod, connected := holders[node]
	if !connected {
		return ErrNotConnected
	}
	frame, err := wire.Encode(0, m)
	if err != nil {
		return err
	}
	received, err := s.Bus.SendFrame(ctx, pod, NodeFrame{Scope: scope, Node: node, Frame: frame})
	if err == nil && !received {
		return ErrNotConnected
	}
	return err
}

// Ack reports a node's state to whichever pod is waiting.
func (b *Bus) Ack(ctx context.Context, a Ack) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = b.Redis.Publish(ctx, b.acksChannel(), string(body))
	return err
}

// Handlers are what a pod does with the bus. Any may be nil.
type Handlers struct {
	// OnConnect runs after each subscribe, before anything's received: a pod puts its streams back
	// in Presence and then checks them against the latest versions, in that order, so nothing
	// committed around a Redis restart is missed.
	OnConnect func(context.Context) error
	Handoff   func(Handoff)
	Frame     func(NodeFrame)
	Ack       func(Ack)
}

// Run receives until ctx ends, subscribing again when Redis goes away. A message lost in between is
// a redelivery, not a stale node.
func (b *Bus) Run(ctx context.Context, h Handlers) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := b.session(ctx, h)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= time.Minute {
			backoff = time.Second // it was healthy for a while: this is a new problem
		}
		b.Logger.Error("delivery: the bus disconnected; reconnecting", "error", fmt.Sprint(err), "retrySeconds", backoff.Seconds())
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

// keepAlive refreshes the liveness key until ctx ends.
func (b *Bus) keepAlive(ctx context.Context) {
	t := time.NewTicker(aliveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := b.alive(ctx); err != nil && ctx.Err() == nil {
				b.Logger.Warn("delivery: refreshing this pod's liveness failed", "error", err.Error())
			}
		}
	}
}

func (b *Bus) alive(ctx context.Context) error {
	_, err := b.Redis.Do(ctx, "SET", b.aliveKey(b.Pod), "1", "EX", strconv.Itoa(int(aliveFor.Seconds())))
	return err
}

func (b *Bus) session(ctx context.Context, h Handlers) error {
	if err := b.alive(ctx); err != nil {
		return err
	}
	sub, err := b.Redis.Subscribe(ctx, b.podChannel(b.Pod), b.acksChannel())
	if err != nil {
		return err
	}
	defer func() { _ = sub.Close() }()
	if h.OnConnect != nil {
		if err := h.OnConnect(ctx); err != nil {
			return err
		}
	}
	alive, stop := context.WithCancel(ctx)
	defer stop()
	go b.keepAlive(alive)
	for {
		m, err := sub.Receive(ctx)
		if err != nil {
			return err
		}
		if m.Channel == b.acksChannel() {
			var a Ack
			if err := json.Unmarshal([]byte(m.Payload), &a); err != nil {
				b.Logger.Warn("delivery: an unreadable ack", "error", err.Error())
				continue
			}
			if h.Ack != nil {
				h.Ack(a)
			}
			continue
		}
		var message podMessage
		if err := json.Unmarshal([]byte(m.Payload), &message); err != nil {
			b.Logger.Warn("delivery: an unreadable pod message", "error", err.Error())
			continue
		}
		switch {
		case message.Handoff != nil && h.Handoff != nil:
			h.Handoff(*message.Handoff)
		case message.Frame != nil && h.Frame != nil:
			h.Frame(*message.Frame)
		}
	}
}
