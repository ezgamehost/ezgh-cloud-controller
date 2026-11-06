package db

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

// Listener is one connection of its own, outside the pool, for the session state database/sql
// can't carry: LISTEN/NOTIFY, and a blocking session advisory lock held for as long as the
// session lasts (an outbox relay's). Its TCP keepalives notice a dead peer while it waits on the
// lock or for a notification. Not safe for concurrent use.
type Listener struct{ conn *pgx.Conn }

// OpenListener connects a Listener to the pool's database, as applicationName.
func (d *DB) OpenListener(ctx context.Context, applicationName string) (*Listener, error) {
	cfg := d.Pool.Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = applicationName
	cfg.ConnectTimeout = 10 * time.Second
	cfg.DialFunc = (&net.Dialer{Timeout: 10 * time.Second, KeepAliveConfig: net.KeepAliveConfig{
		Enable: true, Idle: 15 * time.Second, Interval: 5 * time.Second, Count: 3}}).DialContext
	// Its waits end at their deadline: an idle connection has no statement for CancelHandler's
	// server-side cancel to end, so a Wait would outlast its deadline by CancelDeadlineDelay.
	// Expiring the socket's deadline leaves the connection usable after a timeout; a Lock or Ping
	// its context ends is followed by Close.
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.DeadlineContextWatcherHandler{Conn: c.Conn()}
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Listener{conn: conn}, nil
}

// Lock takes the session advisory lock named name (hashtextextended(name, 0)), waiting for as
// long as another session holds it. It's released when the Listener closes.
func (l *Listener) Lock(ctx context.Context, name string) error {
	if _, err := l.conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, name); err != nil {
		return fmt.Errorf("taking the advisory lock %s: %w", name, err)
	}
	return nil
}

// Listen subscribes to channel.
func (l *Listener) Listen(ctx context.Context, channel string) error {
	if _, err := l.conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return fmt.Errorf("LISTEN %s: %w", channel, err)
	}
	return nil
}

// Wait blocks until a notification arrives on a channel it listens to, and returns that
// channel's name. Notifications are delivered at their transaction's commit. An error is ctx's
// or a failed connection (the Listener is then unusable).
func (l *Listener) Wait(ctx context.Context) (string, error) {
	n, err := l.conn.WaitForNotification(ctx)
	if err != nil {
		return "", err
	}
	return n.Channel, nil
}

// Ping checks the connection answers.
func (l *Listener) Ping(ctx context.Context) error { return l.conn.Ping(ctx) }

// Close closes the connection (releasing its locks and subscriptions), waiting at most 2 s.
func (l *Listener) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = l.conn.Close(ctx)
}
