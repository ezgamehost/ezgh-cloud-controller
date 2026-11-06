package redis

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Message is one message published on a channel the subscription listens to.
type Message struct {
	Channel, Payload string
}

// ErrClosed is what Receive returns once the subscription is closed.
var ErrClosed = errors.New("redis: subscription closed")

// pongGrace is how long past PingEvery a silent connection lives: a PING written when the
// connection went idle must be answered within it.
const pongGrace = 5 * time.Second

// Subscription is a SUBSCRIBE on its own connection, never the pool's (Client.Subscribe). One
// goroutine reads it, started by the first Receive; while it's idle for PingEvery it writes PING,
// and a connection silent for PingEvery plus 5 s is dead. Any error ends it for good: the caller
// subscribes again (and catches up on what it may have missed).
type Subscription struct {
	// PingEvery is how long the connection may stay idle before the subscription pings the
	// server. Default 15 s. Set it before the first Receive.
	PingEvery time.Duration

	cn       *conn
	start    sync.Once
	msgs     chan Message
	dead     chan struct{} // closed when the reader stops; err says why
	err      error
	closing  chan struct{}
	close    sync.Once
	activity atomic.Int64 // UnixNano of the last frame read
}

// Subscribe dials a connection of its own, subscribes it to channels and returns once the server
// has confirmed every one, so a message published after it returns is received. The context (and
// the client's Timeout) bounds only the subscribing; Close the subscription when done.
func (c *Client) Subscribe(ctx context.Context, channels ...string) (*Subscription, error) {
	if len(channels) == 0 {
		return nil, errors.New("redis: subscribe to at least one channel")
	}
	deadline := time.Now().Add(c.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	cn, err := c.dial(deadline)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = cn.SetDeadline(time.Now()) })
	err = subscribeOn(cn, channels)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = cn.SetDeadline(time.Time{})
	}
	if err != nil {
		_ = cn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	s := &Subscription{
		PingEvery: 15 * time.Second,
		cn:        cn,
		msgs:      make(chan Message, 64),
		dead:      make(chan struct{}),
		closing:   make(chan struct{}),
	}
	s.activity.Store(time.Now().UnixNano())
	return s, nil
}

// subscribeOn sends SUBSCRIBE and reads one confirmation per channel, in order.
func subscribeOn(cn *conn, channels []string) error {
	if _, err := cn.Write(encode(append([]string{"SUBSCRIBE"}, channels...))); err != nil {
		return err
	}
	for _, want := range channels {
		reply, err := readReply(cn.rd)
		if err != nil {
			return err
		}
		frame, ok := reply.([]any)
		if !ok || len(frame) != 3 || frame[0] != "subscribe" || frame[1] != want {
			return fmt.Errorf("redis: expected the subscription to %q to be confirmed, got %v", want, reply)
		}
	}
	return nil
}

// Receive waits for the next message. An error other than the context's means the subscription
// is dead (the connection failed or the server went silent, or it was closed): every later call
// returns it too, and the caller subscribes again.
func (s *Subscription) Receive(ctx context.Context) (Message, error) {
	s.start.Do(s.run)
	select {
	case m := <-s.msgs:
		return m, nil
	default:
	}
	select {
	case m := <-s.msgs:
		return m, nil
	case <-s.dead:
		// Messages read before the connection died come first.
		select {
		case m := <-s.msgs:
			return m, nil
		default:
		}
		return Message{}, s.err
	case <-ctx.Done():
		return Message{}, ctx.Err()
	}
}

// Close ends the subscription and closes its connection. It's safe to call more than once.
func (s *Subscription) Close() error {
	s.close.Do(func() {
		close(s.closing)
		_ = s.cn.Close()
	})
	return nil
}

// run starts the reader and the pinger.
func (s *Subscription) run() {
	every := s.PingEvery
	if every <= 0 {
		every = 15 * time.Second
	}
	go s.read(every)
	go s.ping(every)
}

// read reads frames until the connection fails: messages go to msgs, confirmations and pongs are
// skipped, and anything else ends the subscription.
func (s *Subscription) read(every time.Duration) {
	err := func() error {
		for {
			if err := s.cn.SetReadDeadline(time.Now().Add(every + pongGrace)); err != nil {
				return err
			}
			reply, err := readReply(s.cn.rd)
			if err != nil {
				return err
			}
			s.activity.Store(time.Now().UnixNano())
			frame, _ := reply.([]any)
			kind := ""
			if len(frame) > 0 {
				kind, _ = frame[0].(string)
			}
			switch {
			case kind == "message" && len(frame) == 3:
				channel, ok1 := frame[1].(string)
				payload, ok2 := frame[2].(string)
				if !ok1 || !ok2 {
					return fmt.Errorf("redis: malformed message %v", reply)
				}
				select {
				case s.msgs <- Message{Channel: channel, Payload: payload}:
				case <-s.closing:
					return ErrClosed
				}
			case kind == "subscribe" || kind == "pong":
			default:
				return fmt.Errorf("redis: unexpected frame on a subscription: %v", reply)
			}
		}
	}()
	select {
	case <-s.closing:
		err = ErrClosed
	default:
	}
	s.err = err
	close(s.dead)
	_ = s.cn.Close()
}

// ping writes PING whenever the connection has been idle for every, until the subscription ends.
func (s *Subscription) ping(every time.Duration) {
	ping := encode([]string{"PING"})
	timer := time.NewTimer(every)
	defer timer.Stop()
	for {
		select {
		case <-s.closing:
			return
		case <-s.dead:
			return
		case <-timer.C:
		}
		idle := time.Since(time.Unix(0, s.activity.Load()))
		if idle < every {
			timer.Reset(every - idle)
			continue
		}
		if err := s.cn.SetWriteDeadline(time.Now().Add(pongGrace)); err == nil {
			_, err = s.cn.Write(ping)
			if err != nil {
				// The reader sees the closed connection and ends the subscription.
				_ = s.cn.Close()
				return
			}
		}
		timer.Reset(every)
	}
}

// Publish sends payload on channel and returns how many subscribers received it.
func (c *Client) Publish(ctx context.Context, channel, payload string) (receivers int64, err error) {
	reply, err := c.Do(ctx, "PUBLISH", channel, payload)
	if err != nil {
		return 0, err
	}
	n, ok := reply.(int64)
	if !ok {
		return 0, fmt.Errorf("redis: PUBLISH answered %v", reply)
	}
	return n, nil
}
