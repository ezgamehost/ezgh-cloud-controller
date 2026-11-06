package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/rabbitmq"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

const dbi = "dbi_aaaaaaaaaaaa"

// settled records how a delivery ended.
type settled struct {
	mu      sync.Mutex
	acks    int
	nacks   int
	rejects int
}

func (s *settled) Ack(uint64, bool) error { s.mu.Lock(); defer s.mu.Unlock(); s.acks++; return nil }
func (s *settled) Nack(uint64, bool, bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nacks++
	return nil
}
func (s *settled) Reject(uint64, bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejects++
	return nil
}
func (s *settled) counts() (acks, nacks, rejects int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acks, s.nacks, s.rejects
}

type stand struct {
	mu         sync.Mutex
	generation int64
	holders    map[string]string
	dead       map[string]bool
	sent       []Handoff
	pods       []string
	reads      atomic.Int64
	failAt     error
}

func (s *stand) Holders(context.Context, string) (map[string]string, error) {
	s.reads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAt != nil {
		return nil, s.failAt
	}
	out := make(map[string]string, len(s.holders))
	for k, v := range s.holders {
		out[k] = v
	}
	return out, nil
}

func (s *stand) Clear(_ context.Context, _, node, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.holders, node)
	return true, nil
}

func (s *stand) dispatcher() *Dispatcher {
	return &Dispatcher{
		Presence: s, Logger: testdb.Logger(), RetryAfter: 20 * time.Millisecond,
		Latest: func(context.Context, string) (int64, bool, error) {
			s.reads.Add(1)
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.generation, true, nil
		},
		Handoff: func(_ context.Context, pod string, h Handoff) (bool, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.sent, s.pods = append(s.sent, h), append(s.pods, pod)
			return !s.dead[pod], nil
		},
	}
}

func (s *stand) sends() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func pointer(t *testing.T, generation int64, a amqp.Acknowledger) amqp.Delivery {
	t.Helper()
	body, err := json.Marshal(Pointer{Scope: dbi, Generation: generation})
	if err != nil {
		t.Fatal(err)
	}
	return amqp.Delivery{Acknowledger: a, Body: body}
}

func until(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("never: %s", what)
}

func TestPointerShardIsFNV(t *testing.T) {
	// FNV-1a 32 of "dbi_aaaaaaaaaaaa", worked out by hand from the published offset and prime.
	const offset, prime = uint32(2166136261), uint32(16777619)
	h := offset
	for _, b := range []byte(dbi) {
		h = (h ^ uint32(b)) * prime
	}
	if got, want := Shard(dbi, 16), int(h%16); got != want {
		t.Errorf("shard %d, want %d", got, want)
	}
	if got := RoutingKey(dbi, 16); got != fmt.Sprintf("change.%d", h%16) {
		t.Errorf("routing key %s", got)
	}
	if queues := ShardQueues("databases", 16); len(queues) != 16 || queues[15] != "databases.changes.15" {
		t.Errorf("%v", queues)
	}
}

func TestCollapse(t *testing.T) {
	s := &stand{generation: 100, holders: map[string]string{"ins_a": "pod-1"}}
	d := s.dispatcher()
	d.RetryAfter = time.Hour
	first := &settled{}
	if err := d.Handle(context.Background(), pointer(t, 1, first)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatalf("the first pointer: %v", err)
	}
	reads := s.reads.Load()
	for generation := int64(2); generation <= 100; generation++ {
		if err := d.Handle(context.Background(), pointer(t, generation, &settled{})); err != nil {
			t.Fatalf("generation %d: %v", generation, err)
		}
	}
	if got := s.sends(); got != 1 {
		t.Errorf("%d sends", got)
	}
	if got := s.reads.Load(); got != reads {
		t.Errorf("collapsed pointers made %d reads", got-reads)
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_a", Generation: 100})
	if acks, _, _ := first.counts(); acks != 1 {
		t.Errorf("%d acks", acks)
	}
}

// A newer change while one is held takes over from it, so one busy database holds one slot of
// the queue's prefetch.
func TestNewerPointerTakesOver(t *testing.T) {
	s := &stand{generation: 1, holders: map[string]string{"ins_a": "pod-1"}}
	d := s.dispatcher()
	d.RetryAfter = time.Hour
	older, newer := &settled{}, &settled{}
	if err := d.Handle(context.Background(), pointer(t, 1, older)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.generation = 2
	s.mu.Unlock()
	if err := d.Handle(context.Background(), pointer(t, 2, newer)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	if acks, _, _ := older.counts(); acks != 1 {
		t.Error("the older pointer is still held")
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_a", Generation: 1})
	if acks, _, _ := newer.counts(); acks != 0 {
		t.Error("settled by an ack of the older version")
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_a", Generation: 2})
	if acks, _, _ := newer.counts(); acks != 1 {
		t.Error("not settled by the newest version's ack")
	}
}

func TestAckOnlyWhenAllConnectedNodesAcked(t *testing.T) {
	s := &stand{generation: 5, holders: map[string]string{"ins_a": "pod-1", "ins_b": "pod-2"}}
	d := s.dispatcher()
	d.RetryAfter = time.Hour
	a := &settled{}
	if err := d.Handle(context.Background(), pointer(t, 5, a)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	if got := s.sends(); got != 2 {
		t.Errorf("%d sends", got)
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_a", Generation: 5})
	d.Acked(Ack{Scope: dbi, Node: "ins_b", Generation: 4}) // behind
	if acks, _, _ := a.counts(); acks != 0 {
		t.Fatal("settled with a node behind")
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_b", Generation: 6}) // a newer one counts
	if acks, _, _ := a.counts(); acks != 1 {
		t.Fatal("not settled")
	}
}

// A database that keeps changing can't hide a stuck node: a takeover keeps the count.
func TestTakeoverKeepsTheCount(t *testing.T) {
	s := &stand{generation: 1, holders: map[string]string{"ins_a": "pod-1"}}
	d := s.dispatcher()
	d.RetryAfter = 10 * time.Millisecond
	a := &settled{}
	if err := d.Handle(context.Background(), pointer(t, 1, a)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	last := &settled{}
	for generation := int64(2); ; generation++ {
		s.mu.Lock()
		s.generation = generation
		s.mu.Unlock()
		last = &settled{}
		if err := d.Handle(context.Background(), pointer(t, generation, last)); err != nil && !errors.Is(err, rabbitmq.ErrHeld) {
			t.Fatal(err)
		}
		time.Sleep(4 * time.Millisecond)
		if _, _, rejects := last.counts(); rejects == 1 {
			return
		}
		d.mu.Lock()
		parked := d.flights[dbi] == nil
		d.mu.Unlock()
		if parked {
			return
		}
		if generation > 500 {
			t.Fatal("never parked")
		}
	}
}

// A node that's moving isn't stuck, however busy its database, and a node that has acked
// isn't asked again.
func TestProgressIsNotStuck(t *testing.T) {
	s := &stand{generation: 9, holders: map[string]string{"ins_a": "pod-1", "ins_b": "pod-2"}}
	d := s.dispatcher()
	d.RetryAfter = 10 * time.Millisecond
	a := &settled{}
	if err := d.Handle(context.Background(), pointer(t, 9, a)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_a", Generation: 9})
	for target := int64(1); target < 9; target++ {
		time.Sleep(12 * time.Millisecond)
		d.Acked(Ack{Scope: dbi, Node: "ins_b", Generation: target})
	}
	if _, _, rejects := a.counts(); rejects != 0 {
		t.Error("parked a node that was moving")
	}
	s.mu.Lock()
	asked := 0
	for _, pod := range s.pods {
		if pod == "pod-1" {
			asked++
		}
	}
	s.mu.Unlock()
	if asked != 1 {
		t.Errorf("the node that acked was asked %d times", asked)
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_b", Generation: 9})
	if acks, _, _ := a.counts(); acks != 1 {
		t.Error("not settled")
	}
}

// A new session gets every held pointer again, so nothing of the old one may remain.
func TestResetForgetsFlights(t *testing.T) {
	s := &stand{generation: 1, holders: map[string]string{"ins_a": "pod-1"}}
	d := s.dispatcher()
	old := &settled{}
	if err := d.Handle(context.Background(), pointer(t, 1, old)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	if err := d.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * d.RetryAfter)
	if got := s.sends(); got != 1 {
		t.Errorf("%d sends after a reset", got)
	}
	fresh := &settled{}
	d.RetryAfter = time.Hour
	if err := d.Handle(context.Background(), pointer(t, 1, fresh)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatalf("the same pointer again: %v", err)
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_a", Generation: 1})
	if acks, _, _ := fresh.counts(); acks != 1 {
		t.Error("the fresh delivery wasn't settled")
	}
	if acks, nacks, rejects := old.counts(); acks+nacks+rejects != 0 {
		t.Errorf("the old session's delivery was settled: %d, %d, %d", acks, nacks, rejects)
	}
}

// TestRequeueAfter5s: the plan's name. The retry is in place, so nothing else on the shard moves,
// and after MaxRetries the pointer is parked.
func TestRequeueAfter5s(t *testing.T) {
	if RetryAfter != 5*time.Second || MaxRetries != 5 {
		t.Errorf("%v, %d", RetryAfter, MaxRetries)
	}
	s := &stand{generation: 1, holders: map[string]string{"ins_a": "pod-1"}}
	d := s.dispatcher()
	stuck, neighbour := &settled{}, &settled{}
	if err := d.Handle(context.Background(), pointer(t, 1, stuck)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	// Another database on the shard isn't held up meanwhile.
	body, err := json.Marshal(Pointer{Scope: "dbi_bbbbbbbbbbbb", Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Handle(context.Background(), amqp.Delivery{Acknowledger: neighbour, Body: body}); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatalf("the neighbour: %v", err)
	}
	d.Acked(Ack{Scope: "dbi_bbbbbbbbbbbb", Node: "ins_a", Generation: 1})
	if acks, _, _ := neighbour.counts(); acks != 1 {
		t.Error("the neighbour was held up")
	}
	until(t, "the pointer is parked", func() bool { _, _, rejects := stuck.counts(); return rejects == 1 })
	if got := s.sends(); got != 2+MaxRetries { // the two first sends, then the retries
		t.Errorf("%d sends", got)
	}
	if acks, nacks, _ := stuck.counts(); acks != 0 || nacks != 0 {
		t.Errorf("%d acks, %d nacks", acks, nacks)
	}
}

func TestDisconnectedNodeNeedsNothing(t *testing.T) {
	s := &stand{generation: 1, holders: map[string]string{}}
	d := s.dispatcher()
	if err := d.Handle(context.Background(), pointer(t, 1, &settled{})); err != nil {
		t.Errorf("with nothing connected: %v", err) // nil: the kit acknowledges it
	}
	// A pod that's gone: nothing receives the handoff.
	s.holders["ins_a"] = "pod-gone"
	s.dead = map[string]bool{"pod-gone": true}
	a := &settled{}
	if err := d.Handle(context.Background(), pointer(t, 1, a)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	if acks, _, _ := a.counts(); acks != 1 {
		t.Error("waiting on a pod that's gone")
	}
	if len(s.holders) != 0 {
		t.Errorf("presence kept %v", s.holders)
	}
	// A stream that ends while it's waited on.
	s.dead = nil
	s.holders["ins_b"] = "pod-1"
	b := &settled{}
	d.RetryAfter = time.Hour
	if err := d.Handle(context.Background(), pointer(t, 1, b)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_b", Gone: true})
	if acks, _, _ := b.counts(); acks != 1 {
		t.Error("waiting on a node that's gone")
	}
}

func TestIdleFleetReadsNothing(t *testing.T) {
	s := &stand{generation: 1, holders: map[string]string{"ins_a": "pod-1"}}
	d := s.dispatcher()
	a := &settled{}
	if err := d.Handle(context.Background(), pointer(t, 1, a)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	d.Acked(Ack{Scope: dbi, Node: "ins_a", Generation: 1})
	reads := s.reads.Load()
	time.Sleep(10 * d.RetryAfter)
	if got := s.reads.Load(); got != reads {
		t.Errorf("settled, then %d reads", got-reads)
	}
	if got := s.sends(); got != 1 {
		t.Errorf("%d sends", got)
	}
}

// A failure of Redis is everyone's: the session ends, and the queue keeps the messages.
func TestRedisDownStopsTheShard(t *testing.T) {
	s := &stand{generation: 1, failAt: errors.New("redis is down")}
	d := s.dispatcher()
	err := d.Handle(context.Background(), pointer(t, 1, &settled{}))
	if err == nil || errors.Is(err, rabbitmq.ErrHeld) || errors.Is(err, rabbitmq.ErrPoison) {
		t.Errorf("%v", err)
	}
	// In a retry: the pointer goes back rather than counting toward a park.
	s.failAt = nil
	s.holders = map[string]string{"ins_a": "pod-1"}
	a := &settled{}
	if err := d.Handle(context.Background(), pointer(t, 1, a)); !errors.Is(err, rabbitmq.ErrHeld) {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.failAt = errors.New("redis is down")
	s.mu.Unlock()
	until(t, "the pointer goes back", func() bool { _, nacks, _ := a.counts(); return nacks == 1 })
	if _, _, rejects := a.counts(); rejects != 0 {
		t.Error("parked over an outage")
	}
}

func TestUnreadablePointerIsPoison(t *testing.T) {
	d := (&stand{}).dispatcher()
	for _, body := range []string{"", "nonsense", `{"generation":1}`} {
		if err := d.Handle(context.Background(), amqp.Delivery{Body: []byte(body)}); !errors.Is(err, rabbitmq.ErrPoison) {
			t.Errorf("%q: %v", body, err)
		}
	}
}

// The real Redis: presence, and a handoff and an ack over the bus.
func TestPresenceAndBus(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	p := &Presence{Redis: client, Prefix: "test"}
	unique := fmt.Sprintf("dbi_%d", time.Now().UnixNano())
	for node, pod := range map[string]string{"ins_a": "pod-1", "ins_b": "pod-2"} {
		if err := p.Set(ctx, unique, node, pod); err != nil {
			t.Fatal(err)
		}
	}
	if held, err := p.Clear(ctx, unique, "ins_b", "pod-2"); err != nil || !held {
		t.Fatal(held, err)
	}
	// A pod that lost a stream doesn't clear what another pod has taken over, and hears so.
	if held, err := p.Clear(ctx, unique, "ins_a", "pod-2"); err != nil || held {
		t.Fatal(held, err)
	}
	holders, err := p.Holders(ctx, unique)
	if err != nil || len(holders) != 1 || holders["ins_a"] != "pod-1" {
		t.Fatalf("%v, %v", holders, err)
	}
	pod := fmt.Sprintf("pod-%d", time.Now().UnixNano())
	bus := &Bus{Redis: client, Prefix: "test", Pod: pod, Logger: testdb.Logger()}
	handoffs, acks := make(chan Handoff, 1), make(chan Ack, 1)
	connected := make(chan struct{}, 8)
	go bus.Run(ctx, Handlers{
		OnConnect: func(context.Context) error { connected <- struct{}{}; return nil },
		Handoff:   func(h Handoff) { handoffs <- h }, Ack: func(a Ack) { acks <- a },
	})
	<-connected
	want := Handoff{Scope: unique, Nodes: []string{"ins_a"}, Generation: 3}
	until(t, "a pod receives a handoff", func() bool {
		ok, err := bus.Handoff(ctx, pod, want)
		return err == nil && ok
	})
	if got := <-handoffs; !reflect.DeepEqual(got, want) {
		t.Errorf("%+v", got)
	}
	if ok, err := bus.Handoff(ctx, "a-pod-thats-gone", want); ok || err != nil {
		t.Errorf("a pod that's gone: %v, %v", ok, err)
	}
	// A live pod between subscriptions: nothing receives it, and it isn't gone.
	between := &Bus{Redis: client, Prefix: "test", Pod: pod + "-between", Logger: testdb.Logger()}
	if err := between.alive(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, err := bus.Handoff(ctx, between.Pod, want); !ok || err != nil {
		t.Errorf("a pod between subscriptions: %v, %v", ok, err)
	}
	if err := bus.Ack(ctx, Ack{Scope: unique, Node: "ins_a", Generation: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-acks:
		if a.Generation != 2 || a.Node != "ins_a" {
			t.Errorf("%+v", a)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no ack")
	}
}

// The real broker and database: a committed pointer reaches its shard, through the relay, and the
// consumer holds it until the node acks.
func TestPointerThroughTheBroker(t *testing.T) {
	url := os.Getenv("RABBITMQ_TEST_URL")
	if url == "" {
		t.Skip("RABBITMQ_TEST_URL isn't set; run scripts/test.sh")
	}
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	const exchange = "kit.delivery.test"
	if err := ch.ExchangeDeclare(exchange, "topic", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	queue := fmt.Sprintf("databases.test.%d", time.Now().UnixNano())
	args := amqp.Table{"x-queue-type": "quorum", "x-single-active-consumer": true}
	if _, err := ch.QueueDeclare(queue, true, false, false, false, args); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = ch.QueueDelete(queue, false, false, false) })
	if err := ch.QueueBind(queue, RoutingKey(dbi, 16), exchange, false, nil); err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	s := &stand{generation: 1, holders: map[string]string{"ins_a": "pod-1"}}
	session := func(settle func(*Dispatcher)) {
		t.Helper()
		sctx, stop := context.WithCancel(ctx)
		dispatcher := s.dispatcher()
		dispatcher.RetryAfter = time.Hour
		done := make(chan struct{})
		sends := s.sends()
		go func() {
			defer close(done)
			(&rabbitmq.Consumer{URL: url, Queues: []string{queue}, Name: "databases-test", OnConnect: dispatcher.Reset, Handle: dispatcher.Handle, Logger: testdb.Logger()}).Run(sctx)
		}()
		until(t, "the pointer reaches the dispatcher", func() bool { return s.sends() > sends })
		settle(dispatcher)
		stop()
		<-done
	}
	depth := func() int {
		q, err := ch.QueueDeclarePassive(queue, true, false, false, false, args)
		if err != nil {
			t.Fatal(err)
		}
		return q.Messages
	}
	body, err := json.Marshal(Pointer{Scope: dbi, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	// What a product's outbox relay publishes.
	if err := ch.PublishWithContext(ctx, exchange, RoutingKey(dbi, 16), true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, Body: body}); err != nil {
		t.Fatal(err)
	}
	// Unsettled when its session ends: it was held, so it goes back to the queue.
	session(func(*Dispatcher) {})
	until(t, "the held pointer returns to the queue", func() bool { return depth() == 1 })
	// Settled, it's gone.
	session(func(dispatcher *Dispatcher) { dispatcher.Acked(Ack{Scope: dbi, Node: "ins_a", Generation: 1}) })
	until(t, "the settled pointer leaves the queue", func() bool { return depth() == 0 })
}
