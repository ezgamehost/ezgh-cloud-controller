package redis_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

// channel is a channel name of the test's own, so tests sharing a Redis never hear each other.
func channel(t *testing.T, name string) string {
	return fmt.Sprintf("kit-test:%s:%d:%s", t.Name(), time.Now().UnixNano(), name)
}

func subscribe(t *testing.T, c *redis.Client, channels ...string) *redis.Subscription {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.Subscribe(ctx, channels...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func receive(t *testing.T, s *redis.Subscription) redis.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := s.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Two channels, a publish to each: both arrive, each with its channel, and Publish counts the one
// subscriber.
func TestPublishReceive(t *testing.T) {
	c := client(t)
	a, b := channel(t, "a"), channel(t, "b")
	s := subscribe(t, c, a, b)
	ctx := context.Background()
	for _, m := range []redis.Message{{Channel: a, Payload: "one"}, {Channel: b, Payload: `{"seq":2}`}} {
		n, err := c.Publish(ctx, m.Channel, m.Payload)
		if err != nil || n != 1 {
			t.Fatalf("Publish = %d, %v", n, err)
		}
		if got := receive(t, s); got != m {
			t.Fatalf("got %+v, want %+v", got, m)
		}
	}
	// Payloads are bytes: empty, binary and CRLF survive.
	for _, payload := range []string{"", "line\r\nbreak", "\x00\xff"} {
		if _, err := c.Publish(ctx, a, payload); err != nil {
			t.Fatal(err)
		}
		if got := receive(t, s); got.Payload != payload {
			t.Fatalf("got %q, want %q", got.Payload, payload)
		}
	}
}

// Subscribe returns once the server has confirmed every channel, so a publish right after it is
// heard.
func TestSubscribeConfirms(t *testing.T) {
	c := client(t)
	names := []string{channel(t, "1"), channel(t, "2"), channel(t, "3")}
	s := subscribe(t, c, names...)
	ctx := context.Background()
	for _, name := range names {
		if n, err := c.Publish(ctx, name, name); err != nil || n != 1 {
			t.Fatalf("Publish to %s = %d, %v: not subscribed yet", name, n, err)
		}
	}
	for _, name := range names {
		if m := receive(t, s); m.Channel != name || m.Payload != name {
			t.Fatalf("got %+v", m)
		}
	}
	if _, err := c.Subscribe(ctx); err == nil {
		t.Fatal("subscribed to no channel")
	}
}

// A cancelled Receive returns the context's error promptly, and the subscription keeps working.
func TestReceiveContextCancel(t *testing.T) {
	c := client(t)
	name := channel(t, "x")
	s := subscribe(t, c, name)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.Receive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("took %s", took)
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := s.Receive(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.Publish(context.Background(), name, "after"); err != nil {
		t.Fatal(err)
	}
	if m := receive(t, s); m.Payload != "after" {
		t.Fatalf("got %+v", m)
	}
}

// pubsubIDs are the IDs of the server's pub/sub connections.
func pubsubIDs(t *testing.T, c *redis.Client) []string {
	t.Helper()
	reply, err := c.Do(context.Background(), "CLIENT", "LIST", "TYPE", "pubsub")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for line := range strings.SplitSeq(reply.(string), "\n") {
		for field := range strings.FieldsSeq(line) {
			if id, ok := strings.CutPrefix(field, "id="); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// A subscription whose connection the server kills ends: Receive returns an error, and keeps
// returning one. (The test kills only the connection it opened: the Redis may be shared.)
func TestKilledConnectionErrors(t *testing.T) {
	c := client(t)
	before := pubsubIDs(t, c)
	s := subscribe(t, c, channel(t, "k"))
	var ours []string
	for _, id := range pubsubIDs(t, c) {
		if !slices.Contains(before, id) {
			ours = append(ours, id)
		}
	}
	if len(ours) != 1 {
		t.Skipf("can't tell this test's connection apart: new pub/sub connections %v", ours)
	}
	if _, err := c.Do(context.Background(), "CLIENT", "KILL", "ID", ours[0]); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.Receive(ctx)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the connection's error", err)
	}
	if _, again := s.Receive(ctx); again == nil {
		t.Fatal("a dead subscription received")
	}
}

// With PingEvery at 50 ms and nothing published, the subscription pings the server and swallows
// its pongs for a second, and still hears a publish afterwards.
func TestKeepalive(t *testing.T) {
	c := client(t)
	name := channel(t, "idle")
	before := pubsubIDs(t, c)
	s := subscribe(t, c, name)
	s.PingEvery = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if m, err := s.Receive(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %+v, %v while idle", m, err)
	}
	// The server saw the pings: the connection's last command is ping.
	reply, err := c.Do(context.Background(), "CLIENT", "LIST", "TYPE", "pubsub")
	if err != nil {
		t.Fatal(err)
	}
	var ours []string
	for line := range strings.SplitSeq(reply.(string), "\n") {
		for field := range strings.FieldsSeq(line) {
			if id, ok := strings.CutPrefix(field, "id="); ok && !slices.Contains(before, id) {
				ours = append(ours, line)
			}
		}
	}
	if len(ours) == 1 && !strings.Contains(ours[0], " cmd=ping ") {
		t.Fatalf("no ping seen: %s", ours[0])
	}
	if _, err := c.Publish(context.Background(), name, "still here"); err != nil {
		t.Fatal(err)
	}
	if m := receive(t, s); m.Payload != "still here" {
		t.Fatalf("got %+v", m)
	}
}

// A subscription has its own connection: the pool keeps answering commands while it's open, and
// after it's closed, and a closed subscription receives nothing.
func TestSubscriptionNotPooled(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	name := channel(t, "p")
	s := subscribe(t, c, name)
	key := "kit-test:" + t.Name()
	for range 20 {
		if _, err := c.Do(ctx, "INCR", key); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := c.Do(ctx, "GET", key); err != nil || n != "20" {
		t.Fatalf("GET = %v, %v", n, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal("a second Close:", err)
	}
	if _, err := s.Receive(ctx); !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("got %v", err)
	}
	// The server drops the subscriber when it sees the connection close, which can trail Close by
	// a moment: wait for it rather than racing it.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := c.Publish(ctx, name, "x")
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Publish after Close still reaches %d subscriber(s)", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Do(ctx, "DEL", key)
}

// A server that confirms the subscription and then never answers a PING: the subscription is
// dead once PingEvery plus 5 s pass in silence (no Redis needed).
func TestSilentServerEndsTheSubscription(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	pings := make(chan struct{}, 100)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		rd := bufio.NewReader(nc)
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			switch strings.TrimSpace(line) {
			case "SUBSCRIBE":
				_, _ = rd.ReadString('\n') // $<len>
				_, _ = rd.ReadString('\n') // the channel
				_, _ = nc.Write([]byte("*3\r\n$9\r\nsubscribe\r\n$2\r\nch\r\n:1\r\n"))
			case "PING":
				pings <- struct{}{} // and never answer
			}
		}
	}()
	c, err := redis.New("redis://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := c.Subscribe(context.Background(), "ch")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.PingEvery = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err = s.Receive(ctx)
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the connection's timeout", err)
	}
	if took := time.Since(start); took < 5*time.Second || took > 8*time.Second {
		t.Fatalf("dead after %s, want PingEvery + 5 s", took)
	}
	if len(pings) == 0 {
		t.Fatal("never pinged")
	}
}
