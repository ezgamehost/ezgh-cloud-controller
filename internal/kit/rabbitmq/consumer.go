package rabbitmq

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// ErrPoison marks a message that can never be applied (an unreadable body, say): Handle returns
// an error wrapping it, and the message is rejected without requeueing, to the queue's
// dead-letter exchange (<prefix>.dropped) if it has one.
var ErrPoison = errors.New("rabbitmq: poison message")

// ErrHeld means Handle kept the delivery to settle it later itself, with d.Ack, d.Nack or d.Reject,
// from any goroutine, and the consumer moves on to the next one. So one slow subject waits without
// holding up its queue, and one failure doesn't end the session: a held delivery counts against
// Prefetch until it's settled. If the session ends first it goes back to its queue with the
// others, and settling it then returns an error, which means it'll be delivered again.
var ErrHeld = errors.New("rabbitmq: delivery held")

// Handler applies one delivery.
type Handler func(ctx context.Context, d amqp.Delivery) error

// Dispatch is a Handle that routes each delivery by its AMQP type to handlers. A type with no
// handler is acknowledged and skipped: a shared queue carries types this consumer doesn't read.
func Dispatch(handlers map[string]Handler) Handler {
	return func(ctx context.Context, d amqp.Delivery) error {
		if h, ok := handlers[d.Type]; ok {
			return h(ctx, d)
		}
		return nil
	}
}

// Consumer reads a product's queues (its fixed shard queues: quorum queues, usually with
// x-single-active-consumer, declared by the chart) and applies each message.
//
// Each session opens one connection and one channel per queue, starts consuming every queue,
// then runs OnConnect (the catch-up from the producer's snapshot) before handling the first
// delivery: anything published during the catch-up is already queued, so nothing falls between
// the two. Each queue's deliveries are handled one at a time, in order; queues run in parallel,
// so Handle must be safe for concurrent use. Handle's answer settles the message:
//
//   - nil: acknowledged;
//   - an error wrapping ErrPoison: logged and rejected without requeueing (dead-lettered);
//   - an error wrapping ErrHeld: nothing yet, Handle settles it later;
//   - any other error: the session ends, returning it and every delivery it holds to their queues
//     with a nack (requeue), which on RabbitMQ 4.3 doesn't count toward a quorum queue's delivery
//     limit; the consumer reconnects with backoff and catches up again before they're
//     redelivered. Nothing newer overtakes a message that failed, and an outage of any length
//     dead-letters nothing.
//
// A lost connection or channel, or the broker cancelling a consumer (a deleted queue), ends the
// session the same way. Delivery is at least once: Handle must be idempotent (keep the highest
// version per subject).
type Consumer struct {
	// URL is amqp(s)://user:password@host:port/vhost (see URL).
	URL string
	// Queues are read on one connection, shown as Name in RabbitMQ's connection list.
	Queues []string
	Name   string
	// Prefetch is unacknowledged deliveries per queue (default 32).
	Prefetch int
	// OnConnect, when set, runs after every connect, before the first delivery is handled. An
	// error ends the session (and reconnects).
	OnConnect func(ctx context.Context) error
	// Handle applies one delivery (Dispatch routes by type).
	Handle Handler
	Logger *slog.Logger

	// minBackoff/maxBackoff pace reconnects; a session that lasted healthyAfter resets it.
	minBackoff, maxBackoff, healthyAfter time.Duration
}

func (c *Consumer) defaults() {
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	if c.Prefetch < 1 {
		c.Prefetch = 32
	}
	if c.minBackoff == 0 {
		c.minBackoff = time.Second
	}
	if c.maxBackoff == 0 {
		c.maxBackoff = 30 * time.Second
	}
	if c.healthyAfter == 0 {
		c.healthyAfter = time.Minute
	}
}

// Run consumes until ctx ends, reconnecting with backoff (1 s doubling to 30 s; reset by a
// session that lasted a minute).
func (c *Consumer) Run(ctx context.Context) {
	c.defaults()
	backoff := c.minBackoff
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) >= c.healthyAfter {
			backoff = c.minBackoff // it was healthy for a while: this is a new problem
		}
		c.Logger.Error("rabbitmq consumer: disconnected; reconnecting", "consumer", c.Name, "error", fmt.Sprint(err),
			"retrySeconds", backoff.Seconds())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, c.maxBackoff)
	}
}

func (c *Consumer) session(ctx context.Context) error {
	if len(c.Queues) == 0 {
		return errors.New("no queues to consume")
	}
	conn, err := connect(ctx, c.URL, c.Name)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseDeadline(time.Now().Add(2 * time.Second)) }()
	type sub struct {
		queue, tag string
		ch         *amqp.Channel
		deliveries <-chan amqp.Delivery
	}
	subs := make([]sub, 0, len(c.Queues))
	// However the session ends, every delivery it holds goes back to its queue with a nack
	// (requeue), not by closing the connection: on RabbitMQ 4.3 a nack doesn't count toward a
	// quorum queue's delivery limit, and a connection closing with unacknowledged deliveries
	// does. So an outage of any length (a database, a catch-up source, a rolling restart) never
	// dead-letters a valid message. The consumers are cancelled first, so the broker doesn't
	// hand the returned messages straight back to this session. (A connection that's already
	// lost can't nack: those deliveries return counted.)
	defer func() {
		for _, s := range subs {
			_ = s.ch.Cancel(s.tag, false)
		}
		for _, s := range subs {
			_ = s.ch.Nack(0, true, true)
		}
	}()
	for _, q := range c.Queues {
		ch, err := conn.Channel()
		if err != nil {
			return fmt.Errorf("opening a channel for %s: %w", q, err)
		}
		if err := ch.Qos(c.Prefetch, 0, false); err != nil {
			return fmt.Errorf("setting %s's prefetch: %w", q, err)
		}
		tag := consumerTag(q)
		d, err := ch.Consume(q, tag, false, false, false, false, nil)
		if err != nil {
			return fmt.Errorf("consuming %s: %w", q, err)
		}
		subs = append(subs, sub{q, tag, ch, d})
	}
	// Consuming first, then the snapshot (the consumer contract).
	if c.OnConnect != nil {
		if err := c.OnConnect(ctx); err != nil {
			return fmt.Errorf("catching up failed (deliveries returned to their queues; retrying): %w", err)
		}
	}
	c.Logger.Info("rabbitmq consumer: consuming", "consumer", c.Name, "queues", len(subs))
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ended := make(chan error, len(subs))
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Go(func() {
			if err := c.consume(sctx, s.queue, s.deliveries); err != nil {
				ended <- err
				cancel()
			}
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return <-ended
}

// consume handles one queue's deliveries in order until ctx ends (nil) or the session must end.
func (c *Consumer) consume(ctx context.Context, queue string, deliveries <-chan amqp.Delivery) error {
	for {
		var d amqp.Delivery
		var ok bool
		select {
		case <-ctx.Done():
			return nil
		case d, ok = <-deliveries:
		}
		if !ok {
			return fmt.Errorf("%s: the channel closed or the broker cancelled the consumer", queue)
		}
		err := c.Handle(ctx, d)
		switch {
		case err == nil:
			if err := d.Ack(false); err != nil {
				return fmt.Errorf("%s: acknowledging: %w", queue, err)
			}
		case errors.Is(err, ErrHeld):
		case errors.Is(err, ErrPoison):
			c.Logger.Error("rabbitmq consumer: a poison message was rejected", "consumer", c.Name, "queue", queue,
				"type", d.Type, "messageId", d.MessageId, "error", err.Error())
			if err := d.Reject(false); err != nil {
				return fmt.Errorf("%s: rejecting: %w", queue, err)
			}
		case ctx.Err() != nil:
			return nil // the session is ending; the message goes back
		default:
			return fmt.Errorf("%s: handling a %s message failed (returned to the queue; retrying): %w", queue, d.Type, err)
		}
	}
}

// consumerTag is a consumer tag unique to this session's consumer of queue.
func consumerTag(queue string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "kit." + queue + "." + hex.EncodeToString(b)
}
