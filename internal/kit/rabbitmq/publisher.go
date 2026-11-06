// Package rabbitmq is how a product talks to the platform RabbitMQ broker: a confirming
// publisher, the transactional outbox relay that feeds it, and a consumer for a product's fixed
// shard queues.
//
// Messages are persistent JSON with the AMQP properties consumers rely on: type is the event
// type, message_id the event's ID, and headers such as subjectId and version (consumers keep the
// highest version per subject, so order and redelivery don't matter). Queues are quorum queues
// declared by the product's chart (the Messaging Topology Operator), never by this package:
// application users have no configure permission. A queue read by one consumer at a time
// declares x-single-active-consumer; the broker then hands it to one connected consumer and
// fails over to the next when that one goes away.
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

// Message is one message to publish.
type Message struct {
	// Exchange and RoutingKey are where it goes; RoutingKey is required.
	Exchange, RoutingKey string
	// Type is the AMQP type property (the event type); MessageID the message_id (the event's ID).
	Type, MessageID string
	// Headers are the message's headers (subjectId, version), or nil.
	Headers amqp.Table
	// Body is the JSON body.
	Body []byte
}

// RefusedError is the broker refusing a message for good: it closed the channel with
// ACCESS_REFUSED (no write permission on the exchange), NOT_FOUND (the exchange is missing) or
// PRECONDITION_FAILED (the message breaks a limit), or the message can't be sent at all (it has
// no routing key). Sending it again gets the same answer until someone changes the broker's
// topology or permissions. In a batch, MessageID is the first message not confirmed, which isn't
// necessarily the one refused: confirms are pipelined, so an earlier message may still have been
// waiting for its confirm when the channel closed. A batch of one names the refused message.
type RefusedError struct {
	MessageID string
	// Code is the AMQP reply code (403, 404 or 406), or 0 when the message couldn't be sent.
	Code   int
	Reason string
}

func (e *RefusedError) Error() string {
	if e.Code == 0 {
		return fmt.Sprintf("RabbitMQ refused message %s: %s", e.MessageID, e.Reason)
	}
	return fmt.Sprintf("RabbitMQ didn't confirm message %s: Exception (%d) Reason: %q", e.MessageID, e.Code, e.Reason)
}

// IsRefused reports whether err is (or wraps) a RefusedError.
func IsRefused(err error) bool {
	var r *RefusedError
	return errors.As(err, &r)
}

// refusal reports whether a channel closed with code refuses the message itself: anything else
// (a connection forced closed, an internal or resource error) is the broker's trouble, and the
// message may go once it's over.
func refusal(code int) bool {
	return code == amqp.AccessRefused || code == amqp.NotFound || code == amqp.PreconditionFailed
}

// Publisher sends messages to the broker.
type Publisher interface {
	// Publish sends msgs in order, persistent and as application/json, and waits for the
	// broker's confirms. It returns how many messages, from the first, were confirmed; an error
	// means the rest weren't, and the publisher is no longer usable (the caller closes it and
	// dials again).
	Publish(ctx context.Context, msgs []Message) (int, error)
	Close() error
}

// Dial returns a dialer for the Relay (or any publisher): each call opens one connection, shown
// as connectionName in RabbitMQ's connection list, with one confirm-mode channel. url is
// amqp(s)://user:password@host:port/vhost (see URL).
func Dial(url, connectionName string) func(ctx context.Context) (Publisher, error) {
	return func(ctx context.Context) (Publisher, error) {
		conn, err := connect(ctx, url, connectionName)
		if err != nil {
			return nil, err
		}
		ch, err := conn.Channel()
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("opening a channel: %w", err)
		}
		if err := ch.Confirm(false); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("enabling publisher confirms: %w", err)
		}
		return &publisher{conn: conn, ch: ch, closed: ch.NotifyClose(make(chan *amqp.Error, 1))}, nil
	}
}

// URL is base (amqp(s)://host:port) with user, password and vhost set: the form Dial and
// Consumer take.
func URL(base, user, password, vhost string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "amqp" && u.Scheme != "amqps") || u.Host == "" {
		return "", errors.New("a RabbitMQ URL must be amqp(s)://host:port")
	}
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	u.Path = "/" + vhost
	u.RawPath = "/" + url.PathEscape(vhost)
	return u.String(), nil
}

// connect opens a connection named name, its TCP connect bounded by ctx and 10 s, and the TLS and
// AMQP handshakes by 10 s more.
func connect(ctx context.Context, rawURL, name string) (*amqp.Connection, error) {
	conn, err := amqp.DialConfig(rawURL, amqp.Config{
		Heartbeat:  10 * time.Second,
		Locale:     "en_US",
		Properties: amqp.Table{"connection_name": name},
		Dial: func(network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			// amqp091 clears the deadline once the connection is open; heartbeats take over.
			if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				_ = c.Close()
				return nil, err
			}
			return c, nil
		},
	})
	if err != nil {
		// amqp091's dial errors don't include the URL's credentials.
		return nil, fmt.Errorf("connecting to RabbitMQ: %w", err)
	}
	return conn, nil
}

type publisher struct {
	conn   *amqp.Connection
	ch     *amqp.Channel
	closed <-chan *amqp.Error
}

// reason is why the broker closed the channel, if it has (nil if it hasn't). The channel
// reports it before it fails the outstanding confirms.
func (p *publisher) reason() *amqp.Error {
	select {
	case e, ok := <-p.closed:
		if ok {
			return e
		}
	default:
	}
	return nil
}

func (p *publisher) Publish(ctx context.Context, msgs []Message) (int, error) {
	// amqp091 checks ctx only before each write: a write stuck on a connection RabbitMQ has
	// blocked (a memory or disk alarm) would outlive the deadline and shutdown. Closing the
	// connection when ctx ends unblocks it; the caller then dials again.
	stop := context.AfterFunc(ctx, func() { _ = p.conn.CloseDeadline(time.Now()) })
	defer stop()
	confirms := make([]*amqp.DeferredConfirmation, 0, len(msgs))
	var sendErr error
	for _, m := range msgs {
		if m.RoutingKey == "" {
			sendErr = &RefusedError{MessageID: m.MessageID, Reason: "the message has no routing key"}
			break
		}
		dc, err := p.ch.PublishWithDeferredConfirmWithContext(ctx, m.Exchange, m.RoutingKey, false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    m.MessageID,
			Type:         m.Type,
			Headers:      m.Headers,
			Body:         m.Body,
		})
		if err != nil {
			sendErr = err
			break
		}
		confirms = append(confirms, dc)
	}
	for i, dc := range confirms {
		acked, err := dc.WaitContext(ctx)
		if err != nil {
			return i, err
		}
		if !acked {
			// A nack, or the channel closed (a missing exchange, say): the reason is on the
			// channel's close.
			return i, unconfirmed(msgs[i].MessageID, p.reason(), p.conn.IsClosed())
		}
	}
	return len(confirms), sendErr
}

// unconfirmed is the error for message id not confirmed, e the channel's close (nil for a nack)
// and connClosed whether its connection is closed too.
func unconfirmed(id string, e *amqp.Error, connClosed bool) error {
	switch {
	case e == nil:
		return fmt.Errorf("RabbitMQ didn't confirm message %s", id)
	case refusal(e.Code) && !connClosed:
		// The broker closed this channel over this message; a connection closing takes its
		// channels down with its own reason, which says nothing about the message.
		return &RefusedError{MessageID: id, Code: e.Code, Reason: e.Reason}
	default:
		return fmt.Errorf("RabbitMQ didn't confirm message %s: %w", id, e)
	}
}

// reopen replaces a channel the broker closed (a refusal closes the channel, not the connection)
// with a new confirm-mode one on the same connection, so that a refused message costs a channel,
// not a connection. A channel still open is kept.
func (p *publisher) reopen() error {
	if !p.ch.IsClosed() {
		return nil
	}
	if p.conn.IsClosed() {
		return amqp.ErrClosed
	}
	ch, err := p.conn.Channel()
	if err != nil {
		return fmt.Errorf("opening a channel: %w", err)
	}
	if err := ch.Confirm(false); err != nil {
		_ = ch.Close()
		return fmt.Errorf("enabling publisher confirms: %w", err)
	}
	p.ch, p.closed = ch, ch.NotifyClose(make(chan *amqp.Error, 1))
	return nil
}

// Close waits at most 2 s for the broker's close-ok.
func (p *publisher) Close() error { return p.conn.CloseDeadline(time.Now().Add(2 * time.Second)) }
