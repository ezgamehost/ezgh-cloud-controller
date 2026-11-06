// Package commands runs a verb on a node, over Redis alone, so a repair works with RabbitMQ down and
// a product's API can ask a node something from inside a request. Lifted from databases-stack
// (9ff61c1); the Command frame stays each product's, built by Sender.Command.
package commands

import (
	"context"
	"encoding/json"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/delivery"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/wire"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

// ErrNotConnected is a node with no stream: there's no one to ask, so try later.
var ErrNotConnected = delivery.ErrNotConnected

// Request is one verb for one node. Key is its command ID: the same Key is a replay, answered from
// the agent's last results without running twice, so a retry after a lost result keeps it, and a
// verb that must really run again needs a new one. Generation is the product's fencing number (a
// database's epoch), for products whose Command carries one.
type Request struct {
	Scope, Instance string
	Generation      int64
	Verb            string
	Args            any
	Key             string
}

// Result is a node's answer, as every product's result frame spells it.
type Result struct {
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Data  json.RawMessage `json:"data"`
}

// Sender sends commands.
type Sender struct {
	Redis  *redis.Client
	Frames delivery.Frames
	// Prefix starts the result channels, as Presence's.
	Prefix string
	// Command is the product's command frame for r, its args already JSON.
	Command func(r Request, args json.RawMessage) wire.Message
}

// New is a Sender over r. It joins no bus: it only publishes.
func New(r *redis.Client, prefix string, command func(Request, json.RawMessage) wire.Message) *Sender {
	return &Sender{Redis: r, Prefix: prefix, Command: command,
		Frames: &delivery.Sender{Presence: &delivery.Presence{Redis: r, Prefix: prefix}, Bus: &delivery.Bus{Redis: r, Prefix: prefix}}}
}

// ResultChannel is where a node's result comes back. It names the node, whose word is its
// certificate's, so a node can't answer for another.
func ResultChannel(prefix, scope, instance, id string) string {
	return prefix + ":result:" + scope + ":" + instance + ":" + id
}

// Send runs r and waits for its result until ctx ends. Nothing here retries: a caller comes back
// with the same Key at its own pace.
func (s *Sender) Send(ctx context.Context, r Request) (*Result, error) {
	args := r.Args
	if args == nil {
		args = struct{}{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	// Subscribe before sending, or the answer could go by first.
	sub, err := s.Redis.Subscribe(ctx, ResultChannel(s.Prefix, r.Scope, r.Instance, r.Key))
	if err != nil {
		return nil, err
	}
	defer func() { _ = sub.Close() }()
	if err := s.Frames.Send(ctx, r.Scope, r.Instance, s.Command(r, raw)); err != nil {
		return nil, err
	}
	m, err := sub.Receive(ctx)
	if err != nil {
		return nil, err
	}
	var result Result
	if err := json.Unmarshal([]byte(m.Payload), &result); err != nil {
		return nil, err
	}
	return &result, nil
}
