package rabbitmq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// These run against a real RabbitMQ (RABBITMQ_TEST_URL: CI's service container, or the one
// scripts/test.sh starts on apple/container) and, for the relay, a real Postgres. Broker and
// network failures are a TCP proxy in front of the real broker that cuts every connection.

var bg = context.Background()

// brokerURL is RABBITMQ_TEST_URL. Tests skip without it, except in service-template's own CI:
// a product vendoring the kit (as internal/kit/rabbitmq) runs them only when its CI provides a
// broker, so products that don't use RabbitMQ needn't run one. The path is split so sync-kit's
// import rewrite leaves it alone.
func brokerURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("RABBITMQ_TEST_URL")
	if u == "" {
		source := "github.com/ezgamehost/service-template" + "/kit/go/rabbitmq"
		if os.Getenv("CI") != "" && reflect.TypeFor[Message]().PkgPath() == source {
			t.Fatal("RABBITMQ_TEST_URL isn't set")
		}
		t.Skip("RABBITMQ_TEST_URL isn't set; run scripts/test.sh")
	}
	return u
}

// broker is an admin channel for a test's own exchanges and queues.
type broker struct {
	url  string
	conn *amqp.Connection
	ch   *amqp.Channel
}

func newBroker(t *testing.T) *broker {
	t.Helper()
	u := brokerURL(t)
	conn, err := amqp.Dial(u)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	return &broker{url: u, conn: conn, ch: ch}
}

func unique(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "." + hex.EncodeToString(b)
}

// exchange declares a topic exchange of the test's own.
func (b *broker) exchange(t *testing.T) string {
	t.Helper()
	name := unique("kit-test")
	if err := b.ch.ExchangeDeclare(name, "topic", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.ch.ExchangeDelete(name, false, false) })
	return name
}

// queue declares a quorum queue, as a product's chart does, with args, bound to exchange with key.
func (b *broker) queue(t *testing.T, exchange, key string, args amqp.Table) string {
	t.Helper()
	name := unique("kit-test")
	all := amqp.Table{"x-queue-type": "quorum"}
	for k, v := range args {
		all[k] = v
	}
	if _, err := b.ch.QueueDeclare(name, true, false, false, false, all); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = b.ch.QueueDelete(name, false, false, false) })
	if exchange != "" {
		if err := b.ch.QueueBind(name, key, exchange, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

// listen reads a queue with automatic acknowledgements, on a connection of its own.
func (b *broker) listen(t *testing.T, queue string) <-chan amqp.Delivery {
	t.Helper()
	conn, err := amqp.Dial(b.url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	d, err := ch.Consume(queue, "", true, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// publish sends one persistent message straight to exchange.
func (b *broker) publish(t *testing.T, exchange, key, typ, id string) {
	t.Helper()
	if err := b.ch.PublishWithContext(bg, exchange, key, false, false, amqp.Publishing{
		DeliveryMode: amqp.Persistent, Type: typ, MessageId: id, Body: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, d <-chan amqp.Delivery) amqp.Delivery {
	t.Helper()
	select {
	case m, ok := <-d:
		if !ok {
			t.Fatal("the delivery channel closed")
		}
		return m
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for a message")
	}
	return amqp.Delivery{}
}

// nothing checks no message arrives for a while.
func nothing(t *testing.T, d <-chan amqp.Delivery, wait time.Duration) {
	t.Helper()
	select {
	case m := <-d:
		t.Fatalf("unexpected message %s", m.MessageId)
	case <-time.After(wait):
	}
}

// proxy is a TCP proxy to the broker whose cut drops every connection through it, as a broker
// restart or a network failure does.
type proxy struct {
	target string
	mu     sync.Mutex
	conns  []net.Conn
}

// newProxy returns the proxy and rawURL pointing at it.
func newProxy(t *testing.T, rawURL string) (*proxy, string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{target: u.Host}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.pipe(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); p.cut() })
	u.Host = ln.Addr().String()
	return p, u.String()
}

func (p *proxy) pipe(c net.Conn) {
	s, err := net.Dial("tcp", p.target)
	if err != nil {
		_ = c.Close()
		return
	}
	p.mu.Lock()
	p.conns = append(p.conns, c, s)
	p.mu.Unlock()
	go func() { _, _ = io.Copy(s, c); _ = s.Close(); _ = c.Close() }()
	_, _ = io.Copy(c, s)
	_ = c.Close()
	_ = s.Close()
}

func (p *proxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- The publisher.

func TestPublishProperties(t *testing.T) {
	b := newBroker(t)
	ex := b.exchange(t)
	q := b.queue(t, ex, "shard.3", nil)
	got := b.listen(t, q)
	pub, err := Dial(b.url, "kit-test publisher")(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pub.Close() }()
	n, err := pub.Publish(bg, []Message{
		{Exchange: ex, RoutingKey: "shard.3", Type: "firewall.ruleset.updated", MessageID: "e1",
			Headers: amqp.Table{"subjectId": "eni_a", "version": int64(42)}, Body: []byte(`{"version":42}`)},
		{Exchange: ex, RoutingKey: "shard.3", Type: "firewall.compile", MessageID: "e2", Body: []byte(`{}`)},
		{Exchange: ex, RoutingKey: "shard.4", Type: "firewall.compile", MessageID: "e3", Body: []byte(`{}`)},
	})
	if err != nil || n != 3 {
		t.Fatalf("Publish = %d, %v", n, err)
	}
	m := receive(t, got)
	if m.Type != "firewall.ruleset.updated" || m.MessageId != "e1" || m.DeliveryMode != amqp.Persistent ||
		m.ContentType != "application/json" || m.Headers["subjectId"] != "eni_a" || m.Headers["version"] != int64(42) ||
		string(m.Body) != `{"version":42}` {
		t.Fatalf("delivery: %+v", m)
	}
	if m := receive(t, got); m.MessageId != "e2" || len(m.Headers) != 0 {
		t.Fatalf("delivery: %+v", m)
	}
	nothing(t, got, 200*time.Millisecond) // e3's key isn't bound
}

// A missing exchange closes the channel: what came before it is confirmed, the rest isn't.
func TestPublishMissingExchange(t *testing.T) {
	b := newBroker(t)
	ex := b.exchange(t)
	pub, err := Dial(b.url, "kit-test publisher")(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pub.Close() }()
	n, err := pub.Publish(bg, []Message{
		{Exchange: ex, RoutingKey: "k", MessageID: "1", Body: []byte(`{}`)},
		{Exchange: ex + ".missing", RoutingKey: "k", MessageID: "2", Body: []byte(`{}`)},
		{Exchange: ex, RoutingKey: "k", MessageID: "3", Body: []byte(`{}`)},
	})
	if err == nil || n != 1 {
		t.Fatalf("Publish = %d, %v", n, err)
	}
	if n, err := pub.Publish(bg, []Message{{Exchange: ex, MessageID: "4", Body: []byte(`{}`)}}); err == nil || n != 0 {
		t.Fatalf("a message without a routing key: %d, %v", n, err)
	}
}

func TestURL(t *testing.T) {
	got, err := URL("amqps://rabbitmq.rabbitmq.svc:5671", "firewall", "p@ss/word", "firewall")
	if err != nil || got != "amqps://firewall:p%40ss%2Fword@rabbitmq.rabbitmq.svc:5671/firewall" {
		t.Fatalf("URL = %q, %v", got, err)
	}
	if got, _ := URL("amqp://h:5672", "", "", "/"); got != "amqp://h:5672/%2F" {
		t.Fatalf("the default vhost: %q", got)
	}
	for _, bad := range []string{"http://h:5672", "amqp://", "::"} {
		if _, err := URL(bad, "u", "p", "v"); err == nil {
			t.Fatalf("URL(%q) was accepted", bad)
		}
	}
}

// --- The relay.

const outboxDDL = `
CREATE TABLE event_outbox (
  seq bigserial PRIMARY KEY,
  exchange text NOT NULL,
  routing_key text NOT NULL,
  type text NOT NULL,
  message_id text NOT NULL,
  subject_id text,
  version bigint,
  body jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE FUNCTION event_outbox_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM pg_notify('kit_test_events', ''); RETURN NULL; END $$;
CREATE TRIGGER event_outbox_notify AFTER INSERT ON event_outbox
  FOR EACH STATEMENT EXECUTE FUNCTION event_outbox_notify();`

func outboxDB(t *testing.T) *testdb.DB {
	t.Helper()
	brokerURL(t) // skip before creating a database
	d := testdb.New(t)
	if _, err := d.DB.ExecContext(bg, outboxDDL); err != nil {
		t.Fatal(err)
	}
	return d
}

type row struct {
	exchange, key, typ, id string
	subject                *string
	version                *int64
}

func insert(t *testing.T, tx *db.Tx, rows ...row) {
	t.Helper()
	for _, r := range rows {
		if _, err := tx.ExecContext(bg, `INSERT INTO event_outbox (exchange, routing_key, type, message_id, subject_id, version, body)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, r.exchange, r.key, r.typ, r.id, r.subject, r.version, fmt.Sprintf(`{"id": %q}`, r.id)); err != nil {
			t.Fatal(err)
		}
	}
}

func commit(t *testing.T, d *testdb.DB, rows ...row) {
	t.Helper()
	if err := d.DB.RunTx(bg, nil, func(tx *db.Tx) error { insert(t, tx, rows...); return nil }); err != nil {
		t.Fatal(err)
	}
}

func outboxLen(t *testing.T, d *testdb.DB) int {
	t.Helper()
	var n int
	if err := d.DB.QueryRowContext(bg, `SELECT count(*) FROM event_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func startRelay(t *testing.T, r *Relay) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	stop = sync.OnceFunc(func() { cancel(); <-done })
	t.Cleanup(stop)
	return stop
}

func newRelay(d *testdb.DB, dial func(context.Context) (Publisher, error), locked chan<- struct{}) *Relay {
	return &Relay{DB: d.DB, Table: "event_outbox", Channel: "kit_test_events", Lock: "kit_test_events_relay",
		Dial: dial, Logger: testdb.Logger(), Batch: 2, locked: locked,
		minBackoff: 10 * time.Millisecond, maxBackoff: 100 * time.Millisecond}
}

func ids(t *testing.T, d <-chan amqp.Delivery, n int) []string {
	t.Helper()
	var out []string
	for range n {
		out = append(out, receive(t, d).MessageId)
	}
	return out
}

// Rows committed before the relay starts, then in two transactions, arrive in seq order, each
// only after its transaction commits, woken by NOTIFY alone (the health ping is an hour away), and
// the confirmed rows are deleted.
func TestRelayPublishesInSeqOrderAfterCommit(t *testing.T) {
	d := outboxDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	got := b.listen(t, b.queue(t, ex, "rulesets.shard.#", nil))
	subject, version := "eni_a", int64(7)
	r := func(id string) row {
		return row{exchange: ex, key: "rulesets.shard.5", typ: "firewall.ruleset.updated", id: id}
	}
	commit(t, d, r("a"), row{exchange: ex, key: "rulesets.shard.5", typ: "firewall.ruleset.updated", id: "b", subject: &subject, version: &version})

	locked := make(chan struct{}, 1)
	relay := newRelay(d, Dial(b.url, "kit-test relay"), locked)
	relay.healthEvery = time.Hour
	startRelay(t, relay)
	<-locked
	first := receive(t, got)
	second := receive(t, got)
	if first.MessageId != "a" || len(first.Headers) != 0 || first.Type != "firewall.ruleset.updated" || string(first.Body) != `{"id": "a"}` ||
		second.MessageId != "b" || second.Headers["subjectId"] != "eni_a" || second.Headers["version"] != int64(7) {
		t.Fatalf("deliveries: %+v %+v", first, second)
	}

	// A transaction still open publishes nothing; its commit wakes the relay.
	inserted, release, committed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		committed <- d.DB.RunTx(bg, nil, func(tx *db.Tx) error {
			insert(t, tx, r("c"), r("d"), r("e"))
			close(inserted)
			<-release
			return nil
		})
	}()
	<-inserted
	nothing(t, got, 300*time.Millisecond)
	close(release)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	commit(t, d, r("f"), r("g"))
	if got := ids(t, got, 5); !slices.Equal(got, []string{"c", "d", "e", "f", "g"}) {
		t.Fatalf("published %v", got)
	}
	eventually(t, "the confirmed rows deleted", func() bool { return outboxLen(t, d) == 0 })
}

// tagged counts what one relay's publisher published.
type tagged struct {
	Publisher
	mu  *sync.Mutex
	ids *[]string
}

func (p tagged) Publish(ctx context.Context, msgs []Message) (int, error) {
	n, err := p.Publisher.Publish(ctx, msgs)
	p.mu.Lock()
	for _, m := range msgs[:n] {
		*p.ids = append(*p.ids, m.MessageID)
	}
	p.mu.Unlock()
	return n, err
}

type tally struct {
	mu  sync.Mutex
	ids []string
}

func (c *tally) dial(url string) func(context.Context) (Publisher, error) {
	return func(ctx context.Context) (Publisher, error) {
		p, err := Dial(url, "kit-test relay")(ctx)
		if err != nil {
			return nil, err
		}
		return tagged{Publisher: p, mu: &c.mu, ids: &c.ids}, nil
	}
}

func (c *tally) published() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.ids)
}

// One relay holds the lock; a second waits on it and takes over when the first's context ends.
func TestRelayLockTakeover(t *testing.T) {
	d := outboxDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(id string) row { return row{exchange: ex, key: "k", typ: "t", id: id} }
	var a, bb tally
	lockedA, lockedB := make(chan struct{}, 1), make(chan struct{}, 1)
	stopA := startRelay(t, newRelay(d, a.dial(b.url), lockedA))
	<-lockedA
	startRelay(t, newRelay(d, bb.dial(b.url), lockedB))
	commit(t, d, r("1"), r("2"), r("3"))
	if got := ids(t, got, 3); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Fatalf("published %v", got)
	}
	select {
	case <-lockedB:
		t.Fatal("two relays hold the lock")
	case <-time.After(300 * time.Millisecond):
	}
	stopA()
	<-lockedB
	commit(t, d, r("4"))
	if got := ids(t, got, 1); got[0] != "4" {
		t.Fatalf("published %v", got)
	}
	// B records 4 once its confirm is back, which may be after the delivery.
	eventually(t, "B's confirm", func() bool { return len(bb.published()) == 1 })
	if pa, pb := a.published(), bb.published(); !slices.Equal(pa, []string{"1", "2", "3"}) || !slices.Equal(pb, []string{"4"}) {
		t.Fatalf("A published %v, B %v", pa, pb)
	}
}

// cutAfter publishes the first k messages of its first batch, then cuts every connection to the
// broker (a broker restart) and tries the rest.
type cutAfter struct {
	Publisher
	k    int
	p    *proxy
	once *sync.Once
}

func (c cutAfter) Publish(ctx context.Context, msgs []Message) (int, error) {
	cut := false
	c.once.Do(func() { cut = true })
	if !cut || len(msgs) <= c.k {
		return c.Publisher.Publish(ctx, msgs)
	}
	n, err := c.Publisher.Publish(ctx, msgs[:c.k])
	if err != nil {
		return n, err
	}
	c.p.cut()
	m, err := c.Publisher.Publish(ctx, msgs[c.k:])
	return n + m, err
}

// The broker going away mid-batch: the confirmed rows are deleted, and every unconfirmed one is
// published again on a new connection, in order (at least once).
func TestRelayBrokerRestartMidBatch(t *testing.T) {
	d := outboxDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	got := b.listen(t, b.queue(t, ex, "k", nil))
	p, viaProxy := newProxy(t, b.url)
	var once sync.Once
	var dials atomic.Int32
	dial := func(ctx context.Context) (Publisher, error) {
		dials.Add(1)
		pub, err := Dial(viaProxy, "kit-test relay")(ctx)
		if err != nil {
			return nil, err
		}
		return cutAfter{Publisher: pub, k: 1, p: p, once: &once}, nil
	}
	r := func(id string) row { return row{exchange: ex, key: "k", typ: "t", id: id} }
	commit(t, d, r("1"), r("2"), r("3"), r("4"), r("5"))
	locked := make(chan struct{}, 1)
	startRelay(t, newRelay(d, dial, locked))
	<-locked
	var seen []string
	for !slices.Contains(seen, "5") {
		id := receive(t, got).MessageId
		if !slices.Contains(seen, id) {
			seen = append(seen, id)
		}
	}
	if !slices.Equal(seen, []string{"1", "2", "3", "4", "5"}) {
		t.Fatalf("first deliveries %v", seen)
	}
	eventually(t, "the outbox emptied", func() bool { return outboxLen(t, d) == 0 })
	if n := dials.Load(); n < 2 {
		t.Fatalf("the relay didn't reconnect: %d dials", n)
	}
}

// A message the broker refuses mid-batch (its exchange is missing) holds back the rows after it;
// once the exchange exists they all go, the confirmed ones never again.
func TestRelayRetriesUnconfirmedRows(t *testing.T) {
	d := outboxDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	missing := unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, id string) row { return row{exchange: exchange, key: "k", typ: "t", id: id} }
	commit(t, d, r(ex, "1"), r(missing, "2"), r(ex, "3"))
	locked := make(chan struct{}, 1)
	// One row a batch: 1 is confirmed before 2 closes the channel. (In one batch, the quorum
	// queue's confirm for 1 may lose the race with the channel's close, and 1 goes again.)
	relay := newRelay(d, Dial(b.url, "kit-test relay"), locked)
	relay.Batch = 1
	startRelay(t, relay)
	<-locked
	if id := receive(t, got).MessageId; id != "1" {
		t.Fatalf("got %s", id)
	}
	nothing(t, got, 300*time.Millisecond)
	if err := b.ch.ExchangeDeclare(missing, "topic", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.ch.ExchangeDelete(missing, false, false) })
	if err := b.ch.QueueBind(b.queue(t, "", "", nil), "k", missing, false, nil); err != nil {
		t.Fatal(err)
	}
	if id := receive(t, got).MessageId; id != "3" {
		t.Fatalf("got %s", id)
	}
	eventually(t, "the outbox emptied", func() bool { return outboxLen(t, d) == 0 })
	nothing(t, got, 200*time.Millisecond) // 1 isn't published again
}

const parkedDDL = `
CREATE TABLE event_outbox_parked (
  seq bigint PRIMARY KEY,
  exchange text NOT NULL,
  routing_key text NOT NULL,
  type text NOT NULL,
  message_id text NOT NULL,
  subject_id text,
  version bigint,
  body jsonb NOT NULL,
  reason text NOT NULL,
  parked_at timestamptz NOT NULL DEFAULT now()
);`

type parkedRow struct {
	id, exchange, key, reason string
	version                   *int64
}

func parked(t *testing.T, d *testdb.DB) []parkedRow {
	t.Helper()
	rows, err := d.DB.QueryContext(bg, `SELECT message_id, exchange, routing_key, reason, version FROM event_outbox_parked ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []parkedRow
	for rows.Next() {
		var p parkedRow
		if err := rows.Scan(&p.id, &p.exchange, &p.key, &p.reason, &p.version); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// With Parked set, a message the broker refuses for good (its exchange is missing, or it has no
// routing key) is moved to the parked table with the reason, and the rows after it still go.
func TestRelayParksRefusedRows(t *testing.T) {
	d := outboxDB(t)
	if _, err := d.DB.ExecContext(bg, parkedDDL); err != nil {
		t.Fatal(err)
	}
	b := newBroker(t)
	ex := b.exchange(t)
	missing := unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, key, id string) row { return row{exchange: exchange, key: key, typ: "t", id: id} }
	version := int64(3)
	refused := r(missing, "k", "2")
	refused.version = &version
	commit(t, d, r(ex, "k", "1"), refused, r(ex, "k", "3"), r(ex, "", "4"), r(ex, "k", "5"), r(ex, "k", "6"))
	locked := make(chan struct{}, 1)
	relay := newRelay(d, Dial(b.url, "kit-test relay"), locked)
	relay.Parked, relay.ParkAfter = "event_outbox_parked", time.Millisecond
	relay.pingTimeout = time.Second
	startRelay(t, relay)
	<-locked
	// At least once: 1 may go again when 2's refusal closes the channel before 1's confirm.
	var seen []string
	for !slices.Contains(seen, "6") {
		id := receive(t, got).MessageId
		if !slices.Contains(seen, id) {
			seen = append(seen, id)
		}
	}
	if !slices.Equal(seen, []string{"1", "3", "5", "6"}) {
		t.Fatalf("published %v", seen)
	}
	eventually(t, "the outbox emptied", func() bool { return outboxLen(t, d) == 0 })
	p := parked(t, d)
	if len(p) != 2 || p[0].id != "2" || p[0].exchange != missing || p[0].key != "k" || p[0].version == nil || *p[0].version != 3 ||
		!strings.Contains(p[0].reason, "NOT_FOUND") || p[1].id != "4" || !strings.Contains(p[1].reason, "routing key") {
		t.Fatalf("parked %+v", p)
	}
	// The relay carries on: a later row goes too.
	commit(t, d, r(ex, "k", "7"))
	if id := receive(t, got).MessageId; id != "7" {
		t.Fatalf("got %s", id)
	}
}

func parkedDB(t *testing.T) *testdb.DB {
	t.Helper()
	d := outboxDB(t)
	if _, err := d.DB.ExecContext(bg, parkedDDL); err != nil {
		t.Fatal(err)
	}
	return d
}

func parkingRelay(d *testdb.DB, dial func(context.Context) (Publisher, error), locked chan<- struct{}, after time.Duration) *Relay {
	r := newRelay(d, dial, locked)
	r.Parked, r.ParkAfter = "event_outbox_parked", after
	r.pingTimeout = time.Second // a quiet gap of 1.2 s ends a refusal
	return r
}

// forbid passes messages to the real broker except those for its exchanges, which it answers as
// RabbitMQ answers a user without write permission: ACCESS_REFUSED (the test broker's guest user
// can't be given narrower permissions).
type forbid struct {
	Publisher
	exchanges []string
}

func (f forbid) Publish(ctx context.Context, msgs []Message) (int, error) {
	for i, m := range msgs {
		if slices.Contains(f.exchanges, m.Exchange) {
			n, err := 0, error(nil)
			if i > 0 {
				if n, err = f.Publisher.Publish(ctx, msgs[:i]); err != nil {
					return n, err
				}
			}
			return n, &RefusedError{MessageID: m.MessageID, Code: amqp.AccessRefused,
				Reason: "ACCESS_REFUSED - write access to exchange '" + m.Exchange + "' refused"}
		}
	}
	return f.Publisher.Publish(ctx, msgs)
}

func (f forbid) reopen() error { return f.Publisher.(*publisher).reopen() }

func forbidding(url string, exchanges ...string) func(context.Context) (Publisher, error) {
	return func(ctx context.Context) (Publisher, error) {
		p, err := Dial(url, "kit-test relay")(ctx)
		if err != nil {
			return nil, err
		}
		return forbid{Publisher: p, exchanges: exchanges}, nil
	}
}

// An ACCESS_REFUSED from a second exchange right after one was parked for ACCESS_REFUSED, with
// nothing confirmed between, is taken for the broker refusing everything (a permission mistake):
// it isn't parked until its own exchange has refused for twice ParkAfter, and the rows behind it
// wait in order meanwhile.
func TestRelayDoesntParkASystemicRefusal(t *testing.T) {
	d := parkedDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	forbiddenA, forbiddenB := unique("kit-test"), unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, id string) row { return row{exchange: exchange, key: "k", typ: "t", id: id} }
	commit(t, d, r(forbiddenA, "1"), r(forbiddenB, "2"), r(ex, "3"))
	locked := make(chan struct{}, 1)
	const after = 400 * time.Millisecond
	startRelay(t, parkingRelay(d, forbidding(b.url, forbiddenA, forbiddenB), locked, after))
	<-locked
	eventually(t, "the first refusal parked", func() bool { return len(parked(t, d)) == 1 })
	// B has refused for less than twice ParkAfter: it waits, and 3 with it.
	nothing(t, got, after/2)
	if p := parked(t, d); len(p) != 1 || p[0].id != "1" || outboxLen(t, d) != 2 {
		t.Fatalf("parked %+v, %d left", p, outboxLen(t, d))
	}
	// Then it's parked anyway, and 3 goes.
	if id := receive(t, got).MessageId; id != "3" {
		t.Fatalf("got %s", id)
	}
	if p := parked(t, d); len(p) != 2 || p[1].id != "2" || !strings.Contains(p[1].reason, "ACCESS_REFUSED") {
		t.Fatalf("parked %+v", p)
	}
}

// The guard is for permissions only: a missing exchange or a row without a routing key right
// after a park is that row's own fault, and is parked like any other.
func TestRelayParksRefusalsRightAfterAPark(t *testing.T) {
	d := parkedDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	missingA, missingB := unique("kit-test"), unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, key, id string) row { return row{exchange: exchange, key: key, typ: "t", id: id} }
	commit(t, d, r(missingA, "k", "1"), r(ex, "k", "2"), r(missingA, "k", "3"), r(ex, "", "4"), r(missingB, "k", "5"), r(ex, "k", "6"))
	locked := make(chan struct{}, 1)
	startRelay(t, parkingRelay(d, Dial(b.url, "kit-test relay"), locked, time.Millisecond))
	<-locked
	var seen []string
	for !slices.Contains(seen, "6") {
		if id := receive(t, got).MessageId; !slices.Contains(seen, id) {
			seen = append(seen, id)
		}
	}
	if !slices.Equal(seen, []string{"2", "6"}) {
		t.Fatalf("published %v", seen)
	}
	eventually(t, "the outbox emptied", func() bool { return outboxLen(t, d) == 0 })
	if p := parked(t, d); len(p) != 4 || p[0].id != "1" || p[1].id != "3" || p[2].id != "4" || p[3].id != "5" {
		t.Fatalf("parked %+v", p)
	}
}

// The grace measures one continuous refusal: an exchange that stopped refusing (its row was
// deleted by hand) and refuses again later starts it again.
func TestRelayGraceRestartsAfterAQuietGap(t *testing.T) {
	d := parkedDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	missing := unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, id string) row { return row{exchange: exchange, key: "k", typ: "t", id: id} }
	commit(t, d, r(missing, "1"))
	locked := make(chan struct{}, 1)
	const after = time.Second
	startRelay(t, parkingRelay(d, Dial(b.url, "kit-test relay"), locked, after))
	<-locked
	time.Sleep(300 * time.Millisecond)
	if _, err := d.DB.ExecContext(bg, `DELETE FROM event_outbox`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(after + 500*time.Millisecond)
	commit(t, d, r(missing, "2"), r(ex, "3"))
	nothing(t, got, after/2)
	if p := parked(t, d); len(p) != 0 {
		t.Fatalf("parked %+v before its own grace", p)
	}
	if id := receive(t, got).MessageId; id != "3" {
		t.Fatalf("got %s", id)
	}
	if p := parked(t, d); len(p) != 1 || p[0].id != "2" {
		t.Fatalf("parked %+v", p)
	}
}

// An exchange that refused a row into Parked and has had nothing confirmed since is still the
// same refusal: its next row, even after a quiet gap (a sparse outbox), is parked at once rather
// than holding every row behind it for another ParkAfter.
func TestRelayParksAgainAtOnceAfterAPark(t *testing.T) {
	d := parkedDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	missing := unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, id string) row { return row{exchange: exchange, key: "k", typ: "t", id: id} }
	commit(t, d, r(missing, "1"))
	locked := make(chan struct{}, 1)
	const after = time.Second
	startRelay(t, parkingRelay(d, Dial(b.url, "kit-test relay"), locked, after))
	<-locked
	eventually(t, "the first row parked", func() bool { return len(parked(t, d)) == 1 })
	time.Sleep(after + 500*time.Millisecond) // past the quiet gap
	commit(t, d, r(missing, "2"), r(ex, "3"))
	select {
	case m := <-got:
		if m.MessageId != "3" {
			t.Fatalf("got %s", m.MessageId)
		}
	case <-time.After(after / 2):
		t.Fatal("the row behind a parked exchange's next row waited for another grace")
	}
	if p := parked(t, d); len(p) != 2 || p[0].id != "1" || p[1].id != "2" {
		t.Fatalf("parked %+v", p)
	}
}

// A missing peer's rows interleaved with rows that go are all parked, whichever exchange each
// refused one was for.
func TestRelayParksInterleavedRefusals(t *testing.T) {
	d := parkedDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	missingA, missingB := unique("kit-test"), unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, id string) row { return row{exchange: exchange, key: "k", typ: "t", id: id} }
	commit(t, d, r(missingA, "1"), r(ex, "2"), r(missingB, "3"), r(ex, "4"), r(missingA, "5"), r(ex, "6"))
	locked := make(chan struct{}, 1)
	startRelay(t, parkingRelay(d, Dial(b.url, "kit-test relay"), locked, time.Millisecond))
	<-locked
	var seen []string
	for !slices.Contains(seen, "6") {
		if id := receive(t, got).MessageId; !slices.Contains(seen, id) {
			seen = append(seen, id)
		}
	}
	if !slices.Equal(seen, []string{"2", "4", "6"}) {
		t.Fatalf("published %v", seen)
	}
	eventually(t, "the outbox emptied", func() bool { return outboxLen(t, d) == 0 })
	if p := parked(t, d); len(p) != 3 || p[0].id != "1" || p[1].id != "3" || p[2].id != "5" {
		t.Fatalf("parked %+v", p)
	}
}

// An exchange refusing for less than ParkAfter holds its row back, as without Parked; once it
// exists the row and those after it go, and nothing is parked.
func TestRelayParksOnlyAfterTheGrace(t *testing.T) {
	d := parkedDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	missing := unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, id string) row { return row{exchange: exchange, key: "k", typ: "t", id: id} }
	commit(t, d, r(missing, "1"), r(ex, "2"))
	locked := make(chan struct{}, 1)
	startRelay(t, parkingRelay(d, Dial(b.url, "kit-test relay"), locked, time.Hour))
	<-locked
	nothing(t, got, 500*time.Millisecond)
	if p := parked(t, d); len(p) != 0 || outboxLen(t, d) != 2 {
		t.Fatalf("parked %+v, %d left", p, outboxLen(t, d))
	}
	if err := b.ch.ExchangeDeclare(missing, "topic", true, false, false, false, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.ch.ExchangeDelete(missing, false, false) })
	if err := b.ch.QueueBind(b.queue(t, "", "", nil), "k", missing, false, nil); err != nil {
		t.Fatal(err)
	}
	if id := receive(t, got).MessageId; id != "2" {
		t.Fatalf("got %s", id)
	}
	eventually(t, "the outbox emptied", func() bool { return outboxLen(t, d) == 0 })
	if p := parked(t, d); len(p) != 0 {
		t.Fatalf("parked %+v", p)
	}
}

// A broker restart mid-batch isn't a refusal: with Parked set, nothing is parked and every row
// goes.
func TestRelayParksNothingOnABrokerRestart(t *testing.T) {
	d := parkedDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	got := b.listen(t, b.queue(t, ex, "k", nil))
	p, viaProxy := newProxy(t, b.url)
	var once sync.Once
	dial := func(ctx context.Context) (Publisher, error) {
		pub, err := Dial(viaProxy, "kit-test relay")(ctx)
		if err != nil {
			return nil, err
		}
		return cutAfter{Publisher: pub, k: 1, p: p, once: &once}, nil
	}
	r := func(id string) row { return row{exchange: ex, key: "k", typ: "t", id: id} }
	commit(t, d, r("1"), r("2"), r("3"), r("4"))
	locked := make(chan struct{}, 1)
	startRelay(t, parkingRelay(d, dial, locked, time.Millisecond))
	<-locked
	var seen []string
	for !slices.Contains(seen, "4") {
		if id := receive(t, got).MessageId; !slices.Contains(seen, id) {
			seen = append(seen, id)
		}
	}
	if !slices.Equal(seen, []string{"1", "2", "3", "4"}) {
		t.Fatalf("published %v", seen)
	}
	eventually(t, "the outbox emptied", func() bool { return outboxLen(t, d) == 0 })
	if p := parked(t, d); len(p) != 0 {
		t.Fatalf("parked %+v", p)
	}
}

// sizes records the size of every Publish, over every connection it dials, and passes a channel
// reopen through to the kit's publisher.
type sizes struct {
	mu    sync.Mutex
	got   []int
	dials int
}

type sized struct {
	Publisher
	s *sizes
}

func (p sized) Publish(ctx context.Context, msgs []Message) (int, error) {
	p.s.mu.Lock()
	p.s.got = append(p.s.got, len(msgs))
	p.s.mu.Unlock()
	return p.Publisher.Publish(ctx, msgs)
}

func (p sized) reopen() error { return p.Publisher.(*publisher).reopen() }

func (s *sizes) dial(url string) func(context.Context) (Publisher, error) {
	return func(ctx context.Context) (Publisher, error) {
		pub, err := Dial(url, "kit-test relay")(ctx)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		s.dials++
		s.mu.Unlock()
		return sized{Publisher: pub, s: s}, nil
	}
}

// After a refusal the rest of that batch goes one row at a time, on a new channel of the same
// connection; the rows after it go in batches again.
func TestRelayBatchesAgainAfterParking(t *testing.T) {
	d := parkedDB(t)
	b := newBroker(t)
	ex := b.exchange(t)
	missing := unique("kit-test")
	got := b.listen(t, b.queue(t, ex, "k", nil))
	r := func(exchange, id string) row { return row{exchange: exchange, key: "k", typ: "t", id: id} }
	commit(t, d, r(missing, "1"), r(ex, "2"), r(ex, "3"), r(ex, "4"), r(ex, "5"), r(ex, "6"), r(ex, "7"))
	var s sizes
	locked := make(chan struct{}, 1)
	startRelay(t, parkingRelay(d, s.dial(b.url), locked, time.Millisecond))
	<-locked
	if got := ids(t, got, 6); !slices.Equal(got, []string{"2", "3", "4", "5", "6", "7"}) {
		t.Fatalf("published %v", got)
	}
	eventually(t, "the outbox emptied", func() bool { return outboxLen(t, d) == 0 })
	s.mu.Lock()
	defer s.mu.Unlock()
	// [1 2] refused; 1 alone, refused until it parks; 2 alone (the rest of the batch); then
	// [3 4] [5 6] [7].
	n := len(s.got)
	if n < 4 || !slices.Equal(s.got[n-4:], []int{1, 2, 2, 1}) || s.got[0] != 2 {
		t.Fatalf("publishes of %v", s.got)
	}
	if s.dials != 1 {
		t.Fatalf("%d dials: a refusal reopens the channel, not the connection", s.dials)
	}
}

// A nack or a lost connection isn't a refusal: only a channel the broker closed with
// ACCESS_REFUSED, NOT_FOUND or PRECONDITION_FAILED, or a message that can't be sent, is.
func TestIsRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&RefusedError{MessageID: "m", Code: amqp.NotFound, Reason: "NOT_FOUND - no exchange"}, true},
		{fmt.Errorf("wrapped: %w", &RefusedError{MessageID: "m", Code: amqp.AccessRefused}), true},
		{errors.New("RabbitMQ didn't confirm message m"), false},
		{amqp.ErrClosed, false},
		{context.DeadlineExceeded, false},
		{nil, false},
	} {
		if got := IsRefused(c.err); got != c.want {
			t.Errorf("IsRefused(%v) = %v", c.err, got)
		}
	}
	// A channel's close is a refusal only while its connection is open: a connection closing takes
	// its channels down with its own reason.
	if err := unconfirmed("m", &amqp.Error{Code: amqp.NotFound, Reason: "NOT_FOUND"}, false); !IsRefused(err) {
		t.Errorf("a 404 on an open connection: %v", err)
	}
	if err := unconfirmed("m", &amqp.Error{Code: amqp.NotFound, Reason: "NOT_FOUND"}, true); IsRefused(err) || err == nil {
		t.Errorf("a 404 on a closed connection: %v", err)
	}
	if err := unconfirmed("m", &amqp.Error{Code: amqp.ConnectionForced}, false); IsRefused(err) || err == nil {
		t.Errorf("a forced close: %v", err)
	}
	if err := unconfirmed("m", nil, false); IsRefused(err) || err == nil {
		t.Errorf("a nack: %v", err)
	}
	for code, want := range map[int]bool{amqp.AccessRefused: true, amqp.NotFound: true, amqp.PreconditionFailed: true,
		amqp.ConnectionForced: false, amqp.InternalError: false, amqp.ResourceError: false, amqp.ChannelError: false} {
		if got := refusal(code); got != want {
			t.Errorf("refusal(%d) = %v", code, got)
		}
	}
}

func TestRelayRefusesUnsafeNames(t *testing.T) {
	for _, r := range []Relay{
		{Table: `event_outbox"; DROP TABLE x; --`, Channel: "c", Lock: "l"},
		{Table: "event_outbox", Channel: "Bad-Channel", Lock: "l"},
		{Table: "event_outbox", Channel: "c"},
		{Table: "event_outbox", Channel: "c", Lock: "l", Parked: `p"; --`},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%+v was accepted", r)
				}
			}()
			r.Run(bg)
		}()
	}
}

// --- The consumer.

// record is a concurrency-safe log of what happened, in order.
type record struct {
	mu  sync.Mutex
	log []string
	ch  chan string
}

func newRecord() *record { return &record{ch: make(chan string, 100)} }

func (r *record) add(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
	r.ch <- s
}

func (r *record) next(t *testing.T) string {
	t.Helper()
	select {
	case s := <-r.ch:
		return s
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for the consumer")
	}
	return ""
}

func (r *record) nextN(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for range n {
		out = append(out, r.next(t))
	}
	return out
}

func startConsumer(t *testing.T, c *Consumer) (stop func()) {
	t.Helper()
	c.Logger = testdb.Logger()
	c.minBackoff, c.maxBackoff = 10*time.Millisecond, 100*time.Millisecond
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	stop = sync.OnceFunc(func() { cancel(); <-done })
	t.Cleanup(stop)
	return stop
}

// OnConnect runs before the first delivery is handled, again after a reconnect, and a message
// published during the catch-up is handled after it.
func TestConsumerOnConnectBeforeDeliveries(t *testing.T) {
	b := newBroker(t)
	ex := b.exchange(t)
	q := b.queue(t, ex, "shard.0", amqp.Table{"x-single-active-consumer": true})
	b.publish(t, ex, "shard.0", "t", "1")
	b.publish(t, ex, "shard.0", "t", "2")
	p, viaProxy := newProxy(t, b.url)
	rec := newRecord()
	connects := 0
	startConsumer(t, &Consumer{URL: viaProxy, Queues: []string{q}, Name: "kit-test consumer",
		OnConnect: func(ctx context.Context) error {
			connects++
			if connects == 1 {
				b.publish(t, ex, "shard.0", "t", "during")
			}
			rec.add(fmt.Sprintf("connect %d", connects))
			return nil
		},
		Handle: func(ctx context.Context, d amqp.Delivery) error { rec.add(d.MessageId); return nil }})
	if got := rec.nextN(t, 4); !slices.Equal(got, []string{"connect 1", "1", "2", "during"}) {
		t.Fatalf("got %v", got)
	}
	p.cut()
	if got := rec.next(t); got != "connect 2" {
		t.Fatalf("after the reconnect: %s", got)
	}
	b.publish(t, ex, "shard.0", "t", "3")
	// Messages whose acknowledgements were lost in the cut come back first (at least once).
	for got := rec.next(t); got != "3"; got = rec.next(t) {
		if !slices.Contains([]string{"1", "2", "during"}, got) {
			t.Fatalf("after the reconnect: %s", got)
		}
	}
}

// A failed message isn't overtaken: the session ends, the consumer catches up again, and it and
// the ones after it come back in order. Queues are handled in parallel, each in order.
func TestConsumerErrorRedeliversInOrder(t *testing.T) {
	b := newBroker(t)
	ex := b.exchange(t)
	q0 := b.queue(t, ex, "shard.0", nil)
	q1 := b.queue(t, ex, "shard.1", nil)
	for _, id := range []string{"1", "2", "3"} {
		b.publish(t, ex, "shard.0", "t", id)
	}
	rec := newRecord()
	failed := false
	var mu sync.Mutex
	startConsumer(t, &Consumer{URL: b.url, Queues: []string{q0, q1}, Name: "kit-test consumer",
		OnConnect: func(ctx context.Context) error { rec.add("connect"); return nil },
		Handle: func(ctx context.Context, d amqp.Delivery) error {
			mu.Lock()
			defer mu.Unlock()
			if d.MessageId == "2" && !failed {
				failed = true
				return errors.New("the database is unavailable")
			}
			rec.add(d.MessageId)
			return nil
		}})
	if got := rec.nextN(t, 5); !slices.Equal(got, []string{"connect", "1", "connect", "2", "3"}) {
		t.Fatalf("got %v", got)
	}
	b.publish(t, ex, "shard.1", "t", "other-queue")
	if got := rec.next(t); got != "other-queue" {
		t.Fatalf("got %s", got)
	}
}

// ErrPoison rejects without requeueing: the message lands in the dead-letter queue and the next
// one is handled.
func TestConsumerPoisonDeadLetters(t *testing.T) {
	b := newBroker(t)
	ex := b.exchange(t)
	dropped := b.exchange(t)
	dead := b.listen(t, b.queue(t, dropped, "#", nil))
	q := b.queue(t, ex, "shard.0", amqp.Table{"x-dead-letter-exchange": dropped})
	b.publish(t, ex, "shard.0", "network.interface.v1", "bad")
	b.publish(t, ex, "shard.0", "network.interface.v1", "good")
	rec := newRecord()
	startConsumer(t, &Consumer{URL: b.url, Queues: []string{q}, Name: "kit-test consumer",
		Handle: func(ctx context.Context, d amqp.Delivery) error {
			if d.MessageId == "bad" {
				return fmt.Errorf("%w: unreadable body", ErrPoison)
			}
			rec.add(d.MessageId)
			return nil
		}})
	if got := rec.next(t); got != "good" {
		t.Fatalf("got %s", got)
	}
	if m := receive(t, dead); m.MessageId != "bad" {
		t.Fatalf("dead-lettered %s", m.MessageId)
	}
}

// A single-active-consumer queue is read by one consumer; when it stops, the next takes over.
// A held delivery doesn't stop its queue, and only its holder settles it while the session lasts.
// One held when the session ends goes back to the queue, and settling it then fails.
func TestConsumerHeld(t *testing.T) {
	b := newBroker(t)
	ex := b.exchange(t)
	q := b.queue(t, ex, "shard.0", nil)
	b.publish(t, ex, "shard.0", "t", "held")
	b.publish(t, ex, "shard.0", "t", "next")
	seen := make(chan amqp.Delivery, 8)
	stop := startConsumer(t, &Consumer{URL: b.url, Queues: []string{q}, Name: "kit-test consumer",
		Handle: func(ctx context.Context, d amqp.Delivery) error {
			seen <- d
			if d.MessageId == "held" {
				return ErrHeld
			}
			return nil
		}})
	receive := func() amqp.Delivery {
		t.Helper()
		select {
		case d := <-seen:
			return d
		case <-time.After(10 * time.Second):
			t.Fatal("no delivery")
			return amqp.Delivery{}
		}
	}
	if held, next := receive(), receive(); held.MessageId != "held" || next.MessageId != "next" {
		t.Fatalf("%s, %s", held.MessageId, next.MessageId)
	} else if err := held.Nack(false, true); err != nil {
		t.Fatal(err)
	}
	again := receive()
	if again.MessageId != "held" || !again.Redelivered {
		t.Fatalf("after a nack: %s, redelivered %v", again.MessageId, again.Redelivered)
	}
	stop()
	if err := again.Ack(false); err == nil {
		t.Error("acknowledged a delivery its session returned")
	}
	if d := <-b.listen(t, q); d.MessageId != "held" {
		t.Errorf("the queue holds %s", d.MessageId)
	}
}

func TestConsumerSingleActive(t *testing.T) {
	b := newBroker(t)
	ex := b.exchange(t)
	q := b.queue(t, ex, "shard.0", amqp.Table{"x-single-active-consumer": true})
	recA, recB := newRecord(), newRecord()
	handle := func(r *record) Handler {
		return func(ctx context.Context, d amqp.Delivery) error { r.add(d.MessageId); return nil }
	}
	connected := make(chan struct{}, 1)
	stopA := startConsumer(t, &Consumer{URL: b.url, Queues: []string{q}, Name: "kit-test A", Handle: handle(recA),
		OnConnect: func(context.Context) error { connected <- struct{}{}; return nil }})
	<-connected
	startConsumer(t, &Consumer{URL: b.url, Queues: []string{q}, Name: "kit-test B", Handle: handle(recB),
		OnConnect: func(context.Context) error { connected <- struct{}{}; return nil }})
	<-connected
	for _, id := range []string{"1", "2", "3"} {
		b.publish(t, ex, "shard.0", "t", id)
	}
	if got := recA.nextN(t, 3); !slices.Equal(got, []string{"1", "2", "3"}) {
		t.Fatalf("A got %v", got)
	}
	stopA()
	b.publish(t, ex, "shard.0", "t", "4")
	if got := recB.next(t); got != "4" {
		t.Fatalf("B got %s", got)
	}
	recB.mu.Lock()
	defer recB.mu.Unlock()
	if !slices.Equal(recB.log, []string{"4"}) {
		t.Fatalf("B got %v", recB.log)
	}
}

func TestDispatch(t *testing.T) {
	var got []string
	h := Dispatch(map[string]Handler{
		"network.interface.v1": func(_ context.Context, d amqp.Delivery) error {
			got = append(got, "interface "+d.MessageId)
			return nil
		},
		"network.interfaceObserved.v1": func(_ context.Context, d amqp.Delivery) error {
			return fmt.Errorf("%w: no version", ErrPoison)
		},
	})
	if err := h(bg, amqp.Delivery{Type: "network.interface.v1", MessageId: "1"}); err != nil || !slices.Equal(got, []string{"interface 1"}) {
		t.Fatalf("%v %v", err, got)
	}
	if err := h(bg, amqp.Delivery{Type: "network.interfaceObserved.v1"}); !errors.Is(err, ErrPoison) {
		t.Fatalf("%v", err)
	}
	if err := h(bg, amqp.Delivery{Type: "network.binding.ready"}); err != nil {
		t.Fatalf("an unread type: %v", err)
	}
}

// A quorum queue's delivery limit counts deliveries returned by a closing connection (and, before
// RabbitMQ 4.3, nacks). The consumer returns what it holds with a nack, so with a delivery limit
// of 1, a message whose handling fails three times (an outage), whose catch-up fails twice, and
// whose session is shut down mid-handle is still handled, never dead-lettered. Needs RabbitMQ
// 4.3 or later (CI's and scripts/test.sh's).
func TestConsumerFailuresDontCountTowardDeliveryLimit(t *testing.T) {
	b := newBroker(t)
	ex := b.exchange(t)
	dropped := b.exchange(t)
	dead := b.listen(t, b.queue(t, dropped, "#", nil))
	q := b.queue(t, ex, "shard.0", amqp.Table{"x-delivery-limit": 1, "x-dead-letter-exchange": dropped,
		"x-single-active-consumer": true})
	b.publish(t, ex, "shard.0", "t", "1")
	b.publish(t, ex, "shard.0", "t", "2")

	// Shut down mid-handle, three times: each session's context ends while the handler holds 1.
	for range 3 {
		holding := make(chan struct{})
		stop := startConsumer(t, &Consumer{URL: b.url, Queues: []string{q}, Name: "kit-test shutdown",
			Handle: func(ctx context.Context, d amqp.Delivery) error {
				close(holding)
				<-ctx.Done()
				return ctx.Err()
			}})
		select {
		case <-holding:
		case <-time.After(15 * time.Second):
			t.Fatal("no delivery")
		}
		stop()
	}

	rec := newRecord()
	var mu sync.Mutex
	failures, catchUps := 0, 0
	startConsumer(t, &Consumer{URL: b.url, Queues: []string{q}, Name: "kit-test outage",
		OnConnect: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if catchUps++; catchUps <= 2 {
				return errors.New("the snapshot source is down")
			}
			return nil
		},
		Handle: func(ctx context.Context, d amqp.Delivery) error {
			mu.Lock()
			defer mu.Unlock()
			if d.MessageId == "1" && failures < 3 {
				failures++
				return errors.New("the database is down")
			}
			rec.add(d.MessageId)
			return nil
		}})
	if got := rec.nextN(t, 2); !slices.Equal(got, []string{"1", "2"}) {
		t.Fatalf("got %v", got)
	}
	nothing(t, dead, 300*time.Millisecond)
}
