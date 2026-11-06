package delivery

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/rabbitmq"
)

// The settle rules: unacked after RetryAfter, hand off again, at most MaxRetries times, then park.
const (
	RetryAfter = 5 * time.Second
	MaxRetries = 5
)

// Holders is the part of Presence a dispatcher uses.
type Holders interface {
	Holders(ctx context.Context, scope string) (map[string]string, error)
	Clear(ctx context.Context, scope, node, pod string) (bool, error)
}

// Dispatcher consumes the shards. A pointer stays unacknowledged, held, until every connected node
// has acked its generation or a newer one, and each scope waits on its own timer: a shard
// is shared by about a sixteenth of them, so nothing here blocks a queue or ends a session for one
// scope's sake. At most one pointer per scope is held, so a stuck one costs one of
// the queue's Prefetch, however many changes it takes. Redis or the database failing does end the
// session: every shard stops, and changes wait in the queues.
//
// A retry hands off again in place, rather than going back through the queue: the message would
// only come back here, and neither the count nor the timer would survive the trip.
type Dispatcher struct {
	// Latest reads a scope's latest generation from the product's rows. A scope that's gone isn't found.
	Latest   func(ctx context.Context, scope string) (int64, bool, error)
	Presence Holders
	// Handoff answers false only for a pod that's provably gone.
	Handoff func(ctx context.Context, pod string, h Handoff) (bool, error)
	Logger  *slog.Logger
	// RetryAfter and MaxRetries: zero is the constants above.
	RetryAfter time.Duration
	MaxRetries int

	mu      sync.Mutex
	flights map[string]*flight
}

// waiting is a connected node a flight is waiting on.
type waiting struct {
	generation int64 // what it last reported
	stuck      int   // timeouts since it last moved
}

// flight is one scope's held pointer. A newer pointer takes it over, keeping its timer and
// each node's count: otherwise a scope that changes often would never park a stuck node.
type flight struct {
	delivery amqp.Delivery
	want     int64
	pending  map[string]*waiting // connected nodes that haven't acked
	done     map[string]bool     // nodes that have, so a retry leaves them be
	timer    *time.Timer
}

func (d *Dispatcher) retryAfter() time.Duration {
	if d.RetryAfter > 0 {
		return d.RetryAfter
	}
	return RetryAfter
}

func (d *Dispatcher) maxRetries() int {
	if d.MaxRetries > 0 {
		return d.MaxRetries
	}
	return MaxRetries
}

// Reset forgets every flight: the Consumer's OnConnect. A new session gets their pointers again.
func (d *Dispatcher) Reset(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.flights {
		f.timer.Stop()
	}
	d.flights = nil
	return nil
}

// Handle is the shard consumers' handler.
func (d *Dispatcher) Handle(ctx context.Context, m amqp.Delivery) error {
	var p Pointer
	if err := json.Unmarshal(m.Body, &p); err != nil || p.Scope == "" {
		return fmt.Errorf("%w: an unreadable pointer: %v", rabbitmq.ErrPoison, err)
	}
	scope := p.Scope
	d.mu.Lock()
	f := d.flights[scope]
	covered := f != nil && p.Generation <= f.want
	d.mu.Unlock()
	if covered {
		// What's in flight already covers it: latest wins, and this is what collapses a burst.
		return nil
	}
	latest, found, err := d.Latest(ctx, scope)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	holders, err := d.Presence.Holders(ctx, scope)
	if err != nil {
		return err
	}
	if len(holders) == 0 {
		return nil // nothing's connected: a node catches up when it connects
	}
	// In place before the first handoff, so an ack can't arrive ahead of its flight.
	d.mu.Lock()
	if d.flights == nil {
		d.flights = map[string]*flight{}
	}
	f = d.flights[scope]
	var superseded *amqp.Delivery
	if f == nil {
		f = &flight{}
		d.flights[scope] = f
		f.timer = time.AfterFunc(d.retryAfter(), func() { d.retry(scope, f) })
	} else {
		old := f.delivery
		superseded = &old // this pointer carries the obligation now
	}
	f.delivery, f.want, f.done = m, latest, map[string]bool{}
	previous := f.pending
	f.pending = make(map[string]*waiting, len(holders))
	for node := range holders {
		f.pending[node] = cmp.Or(previous[node], &waiting{})
	}
	d.mu.Unlock()
	if superseded != nil {
		_ = superseded.Ack(false)
	}
	if err := d.handOff(ctx, scope, f, holders); err != nil {
		d.mu.Lock()
		if d.flights[scope] == f {
			f.timer.Stop()
			delete(d.flights, scope)
		}
		d.mu.Unlock()
		return err // the kit returns everything to its queue, this delivery included
	}
	return rabbitmq.ErrHeld
}

// handOff tells every pod holding a node f waits on, once. A pod that's gone took its streams.
func (d *Dispatcher) handOff(ctx context.Context, scope string, f *flight, holders map[string]string) error {
	d.mu.Lock()
	byPod := map[string][]string{}
	for node := range f.pending {
		if pod, ok := holders[node]; ok {
			byPod[pod] = append(byPod[pod], node)
		}
	}
	want := f.want
	d.mu.Unlock()
	for pod, onPod := range byPod {
		delivered, err := d.Handoff(ctx, pod, Handoff{Scope: scope, Nodes: onPod, Generation: want})
		if err != nil {
			return err
		}
		if delivered {
			continue
		}
		for _, node := range onPod {
			if _, err := d.Presence.Clear(ctx, scope, node, pod); err != nil {
				return err
			}
			d.Acked(Ack{Scope: scope, Node: node, Gone: true})
		}
	}
	return nil
}

// Acked takes a node's report: Bus.Run's onAck.
func (d *Dispatcher) Acked(a Ack) {
	d.mu.Lock()
	f := d.flights[a.Scope]
	if f == nil {
		d.mu.Unlock()
		return
	}
	if w := f.pending[a.Node]; w != nil {
		if a.Generation > w.generation {
			w.generation, w.stuck = a.Generation, 0
		}
		if a.Gone || a.Generation >= f.want {
			delete(f.pending, a.Node)
			f.done[a.Node] = true
		}
	}
	d.settleIfDone(a.Scope, f)
}

// settleIfDone acknowledges f's pointer when nothing's pending. It's called with d.mu held, and
// releases it: settling is a write to the broker's socket, which mustn't stop every shard.
func (d *Dispatcher) settleIfDone(scope string, f *flight) {
	if len(f.pending) > 0 {
		d.mu.Unlock()
		return
	}
	f.timer.Stop()
	delete(d.flights, scope)
	d.mu.Unlock()
	if err := f.delivery.Ack(false); err != nil {
		d.Logger.Warn("delivery: settling a pointer failed; it'll come back", "scope", scope, "error", err.Error())
	}
}

// retry runs when a flight's timer ends: hand off again, or park it.
func (d *Dispatcher) retry(scope string, f *flight) {
	d.mu.Lock()
	if d.flights[scope] != f {
		d.mu.Unlock()
		return
	}
	var stuck []string
	for node, w := range f.pending {
		if w.stuck++; w.stuck > d.maxRetries() {
			stuck = append(stuck, node)
		}
	}
	if len(stuck) > 0 {
		delete(d.flights, scope)
		generation, delivery := f.want, f.delivery
		d.mu.Unlock()
		slices.Sort(stuck)
		// The alert (NC9). The queue dead-letters it into the dropped queue.
		d.Logger.Error("delivery: nodes didn't ack a generation; parked", "scope", scope, "generation", generation, "nodes", stuck)
		if err := delivery.Reject(false); err != nil {
			d.Logger.Warn("delivery: parking a pointer failed; it'll come back", "scope", scope, "error", err.Error())
		}
		return
	}
	f.timer.Reset(d.retryAfter())
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), d.retryAfter())
	defer cancel()
	err := d.redispatch(ctx, scope, f)
	if err == nil {
		return
	}
	// Redis or the database is failing: this scope stops counting toward a park, and its
	// pointer goes back, which ends the session when Handle meets the same failure.
	d.Logger.Warn("delivery: a retry failed; returning the pointer", "scope", scope, "error", err.Error())
	d.mu.Lock()
	current := d.flights[scope] == f
	if current {
		f.timer.Stop()
		delete(d.flights, scope)
	}
	delivery := f.delivery
	d.mu.Unlock()
	if current {
		_ = delivery.Nack(false, true)
	}
}

// redispatch asks the connected nodes that haven't acked. Nodes may have come or gone since.
func (d *Dispatcher) redispatch(ctx context.Context, scope string, f *flight) error {
	holders, err := d.Presence.Holders(ctx, scope)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if d.flights[scope] != f {
		d.mu.Unlock()
		return nil
	}
	for node := range f.pending {
		if _, connected := holders[node]; !connected {
			delete(f.pending, node)
		}
	}
	for node := range holders {
		if f.pending[node] == nil && !f.done[node] {
			f.pending[node] = &waiting{}
		}
	}
	if len(f.pending) == 0 {
		d.settleIfDone(scope, f)
		return nil
	}
	d.mu.Unlock()
	return d.handOff(ctx, scope, f, holders)
}
