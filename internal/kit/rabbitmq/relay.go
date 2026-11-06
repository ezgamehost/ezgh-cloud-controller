package rabbitmq

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
)

// Relay publishes a product's RabbitMQ outbox: the rows its changes write in their own
// transactions, so "the change happened" and "the event exists" are one fact.
//
// The table has the columns seq (bigserial, the order rows are sent in), exchange, routing_key,
// type, message_id, subject_id and version (both nullable: the subjectId and version headers when
// set) and body (jsonb), and an insert trigger that NOTIFYs Channel:
//
//	CREATE FUNCTION event_outbox_notify() RETURNS trigger LANGUAGE plpgsql AS $$
//	BEGIN PERFORM pg_notify('firewall_events', ''); RETURN NULL; END $$;
//	CREATE TRIGGER event_outbox_notify AFTER INSERT ON event_outbox
//	  FOR EACH STATEMENT EXECUTE FUNCTION event_outbox_notify();
//
// Every replica runs a Relay, and one at a time holds the advisory lock Lock on a connection of
// its own; the others wait on that lock, so a standby takes over as soon as the holder's session
// ends. The holder LISTENs on Channel (a notification arrives at the inserting transaction's
// commit) and publishes every row in seq order with publisher confirms, then deletes the
// confirmed rows. It reads the table only when woken, once after taking the lock and once after
// each RabbitMQ reconnect: there's no polling; its one timer is a health ping on the idle
// connection. A RabbitMQ outage only delays events: they wait in the outbox and the relay retries
// the broker with backoff, publishing again whatever wasn't confirmed (delivery is at least once).
//
// A row whose transaction commits after a later seq's was sent goes out when its own commit wakes
// the relay: consumers order by each subject's version, not by arrival.
//
// A row the broker refuses for good (see RefusedError: its exchange is missing, the relay's user
// may not write to it, it has no routing key) is retried like an outage unless Parked is set, and
// then holds back every row after it. With Parked set, once the row's exchange has refused
// continuously for ParkAfter (no confirm to it, no quiet gap of more than twice the relay's
// longest backoff between refusals), the relay moves the row there instead, logs an error (the
// alert) and carries on with the rest; until then it waits and is retried, so a topology that's
// only late (an exchange or a permission yet to be applied) recovers by itself. Other failures
// still retry in order. Once one of an exchange's rows is parked, its next refused row (with the
// same reply code, nothing confirmed to it since) is parked at once, however long after. An
// ACCESS_REFUSED from a second exchange right after a row was parked for ACCESS_REFUSED, with
// nothing confirmed between, looks like the relay's user lost its permissions everywhere rather
// than one message's fault: that row waits, logged as an error, until its own exchange has
// refused for twice ParkAfter, and then is parked anyway. What the
// relay knows of refusals is in memory and starts again each time it takes the lock. Parked has
// the outbox's columns, seq kept, and the reason:
//
//	CREATE TABLE event_outbox_parked (
//	  seq bigint PRIMARY KEY, exchange text NOT NULL, routing_key text NOT NULL, type text NOT NULL,
//	  message_id text NOT NULL, subject_id text, version bigint, body jsonb NOT NULL,
//	  reason text NOT NULL, parked_at timestamptz NOT NULL DEFAULT now());
//
// Once the topology or permissions are fixed, parked rows are replayed by inserting them into the
// outbox again (new seqs; the insert's NOTIFY wakes the relay) and deleting them from Parked. A
// permission mistake that outlasts the guard above parks every row it refuses, each with its
// error log; the same statement, for every exchange, brings them back:
//
//	WITH p AS (DELETE FROM event_outbox_parked WHERE exchange = $1 RETURNING *)
//	INSERT INTO event_outbox (exchange, routing_key, type, message_id, subject_id, version, body)
//	SELECT exchange, routing_key, type, message_id, subject_id, version, body FROM p ORDER BY seq;
type Relay struct {
	DB *db.DB
	// Table is the outbox table, Channel what its trigger NOTIFYs, Lock the advisory lock's name
	// (also the relay connection's application_name). Constants, never input: Table and Channel
	// must be lower-case SQL identifiers ([a-z_][a-z0-9_]*), or Run panics.
	Table, Channel, Lock string
	// Parked, when set, is the table refused rows move to (a constant, as Table is). Empty, a
	// refused row is retried until the broker takes it.
	Parked string
	// ParkAfter is how long an exchange refuses, continuously, before its rows are parked
	// (default 5 minutes). It's kept in memory: a restart starts it again.
	ParkAfter time.Duration
	// Dial connects to the broker (Dial(url, connectionName)).
	Dial   func(ctx context.Context) (Publisher, error)
	Logger *slog.Logger
	// Batch is rows per publish (default 200).
	Batch int

	// healthEvery: after that long idle the relay pings its connection; pingTimeout bounds it.
	healthEvery, pingTimeout time.Duration
	// minBackoff/maxBackoff pace broker and Postgres reconnects.
	minBackoff, maxBackoff time.Duration
	// locked, when set (tests), receives once each time this relay takes the lock.
	locked chan<- struct{}
}

const (
	defaultBatch = 200
	// publishTimeout bounds one batch's publish and confirms.
	publishTimeout = 30 * time.Second
)

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func (r *Relay) defaults() {
	if !identifier.MatchString(r.Table) || !identifier.MatchString(r.Channel) || r.Lock == "" ||
		(r.Parked != "" && !identifier.MatchString(r.Parked)) {
		panic(fmt.Sprintf("rabbitmq.Relay: Table %q, Channel %q and Parked %q must be [a-z_][a-z0-9_]* and Lock set", r.Table, r.Channel, r.Parked))
	}
	if r.Logger == nil {
		r.Logger = slog.New(slog.DiscardHandler)
	}
	if r.Batch < 1 {
		r.Batch = defaultBatch
	}
	if r.healthEvery == 0 {
		r.healthEvery = 30 * time.Second
	}
	if r.pingTimeout == 0 {
		r.pingTimeout = 5 * time.Second
	}
	if r.minBackoff == 0 {
		r.minBackoff = 500 * time.Millisecond
	}
	if r.maxBackoff == 0 {
		r.maxBackoff = 30 * time.Second
	}
	if r.ParkAfter <= 0 {
		r.ParkAfter = defaultParkAfter
	}
}

// defaultParkAfter is ParkAfter's default.
const defaultParkAfter = 5 * time.Minute

// refusals is what the relay knows of the broker's refusals, in one session.
type refusals struct {
	// since is when each exchange started refusing, and last when it last refused: a confirm to
	// it, or a quiet gap (quietGap), ends the refusal.
	since, last map[string]time.Time
	// parkedCodes are the exchanges that had a row parked, with its reply code, and nothing
	// confirmed to them since: the same refusal goes on, so their next refused row parks at once
	// (a quiet gap means only that nothing was sent there).
	parkedCodes map[string]int
	// parked and parkedCode are the exchange and reply code of the row last parked; confirmed
	// whether a row was confirmed since.
	parked     string
	parkedCode int
	confirmed  bool
	// singles is how many more rows go one at a time, after a refusal in a batch.
	singles int
}

// Run relays until ctx ends. A lost Postgres connection (and with it the lock) is retried with
// backoff; the next session reads the table again.
func (r *Relay) Run(ctx context.Context) {
	r.defaults()
	backoff := r.minBackoff
	for ctx.Err() == nil {
		healthy, err := r.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if healthy {
			backoff = r.minBackoff
		}
		r.Logger.Warn("event relay: database session ended; reconnecting", "table", r.Table, "error", fmt.Sprint(err),
			"backoffSeconds", backoff.Seconds())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, r.maxBackoff)
	}
}

// brokerError is a failure on the RabbitMQ side: the relay keeps its lock and reconnects.
type brokerError struct{ err error }

func (e brokerError) Error() string { return e.err.Error() }
func (e brokerError) Unwrap() error { return e.err }

// refusedWait is a refused row that may not be parked yet: the relay backs off and retries it,
// keeping its connection (drain already gave the publisher a new channel).
type refusedWait struct{ err error }

func (e refusedWait) Error() string { return e.err.Error() }
func (e refusedWait) Unwrap() error { return e.err }

// session takes the lock on its own connection and relays until the connection fails. healthy
// reports whether it drained the outbox at least once: only then does Run reset its backoff, so
// a failure right after taking the lock doesn't reconnect every 0.5 s forever.
func (r *Relay) session(ctx context.Context) (healthy bool, err error) {
	l, err := r.DB.OpenListener(ctx, r.Lock)
	if err != nil {
		return false, err
	}
	defer l.Close()
	// Waits for as long as another replica relays; its session ending releases the lock.
	if err := l.Lock(ctx, r.Lock); err != nil {
		return false, err
	}
	if err := l.Listen(ctx, r.Channel); err != nil {
		return false, err
	}
	r.Logger.Info("event relay: holding the relay lock", "table", r.Table)
	if r.locked != nil {
		r.locked <- struct{}{}
	}
	refused := &refusals{since: map[string]time.Time{}, last: map[string]time.Time{}, parkedCodes: map[string]int{}}
	var pub Publisher
	defer func() {
		if pub != nil {
			_ = pub.Close()
		}
	}()
	pending := true // everything committed before the lock was taken
	var retryAt time.Time
	backoff := r.minBackoff
	lastTrip := time.Now()
	for {
		if pending && !time.Now().Before(retryAt) {
			err := error(nil)
			if pub == nil {
				// A new broker connection: everything still in the table goes (again).
				if pub, err = r.Dial(ctx); err != nil {
					pub, err = nil, brokerError{err}
				}
			}
			if err == nil {
				err = r.drain(ctx, &pub, refused)
			}
			var be brokerError
			var wait refusedWait
			switch {
			case err == nil:
				pending, backoff, healthy = false, r.minBackoff, true
			case ctx.Err() != nil:
				return healthy, ctx.Err()
			case errors.As(err, &wait):
				r.Logger.Warn("event relay: RabbitMQ refuses a message; it and the rows after it wait", "table", r.Table,
					"error", err.Error(), "retrySeconds", backoff.Seconds())
				retryAt = time.Now().Add(backoff)
				backoff = min(2*backoff, r.maxBackoff)
			case errors.As(err, &be):
				r.Logger.Warn("event relay: RabbitMQ unavailable; events wait in the outbox", "table", r.Table,
					"error", err.Error(), "retrySeconds", backoff.Seconds())
				if pub != nil {
					_ = pub.Close()
					pub = nil
				}
				retryAt = time.Now().Add(backoff)
				backoff = min(2*backoff, r.maxBackoff)
			default:
				return healthy, err
			}
		}
		due := lastTrip.Add(r.healthEvery)
		if !time.Now().Before(due) {
			pctx, cancel := context.WithTimeout(ctx, r.pingTimeout)
			err := l.Ping(pctx)
			cancel()
			if err != nil {
				return healthy, fmt.Errorf("ping: %w", err)
			}
			lastTrip = time.Now()
			continue
		}
		if pending && retryAt.Before(due) {
			due = retryAt
		}
		wctx, cancel := context.WithDeadline(ctx, due)
		_, err := l.Wait(wctx)
		timedOut := errors.Is(wctx.Err(), context.DeadlineExceeded)
		cancel()
		switch {
		case err == nil:
			lastTrip = time.Now()
			pending = true
		case ctx.Err() != nil:
			return healthy, ctx.Err()
		case timedOut:
			// The ping or a broker retry is due.
		default:
			return healthy, err
		}
	}
}

// drain publishes every row in seq order, in batches, deleting each batch's confirmed rows.
// Database errors are returned as they are; broker errors as brokerError. With Parked set, a
// refusal sends the rest of that batch one row at a time on a new channel, so that the row refused
// is known, and parks it once it may (see Relay). *pub is the open publisher; drain replaces it
// after a refusal (nil if it can't dial again).
func (r *Relay) drain(ctx context.Context, pub *Publisher, refused *refusals) error {
	query := `SELECT seq, exchange, routing_key, type, message_id, subject_id, version, body FROM "` + r.Table + `" ORDER BY seq LIMIT $1`
	remove := `DELETE FROM "` + r.Table + `" WHERE seq = ANY($1)`
	published := 0
	for {
		type row struct {
			seq int64
			msg Message
		}
		limit := r.Batch
		if refused.singles > 0 {
			limit = 1
		}
		rows, err := r.DB.QueryContext(ctx, query, limit)
		batch, err := db.Collect(rows, err, func(rs *sql.Rows) (row, error) {
			var x row
			var subject sql.NullString
			var version sql.NullInt64
			var body []byte
			err := rs.Scan(&x.seq, &x.msg.Exchange, &x.msg.RoutingKey, &x.msg.Type, &x.msg.MessageID, &subject, &version, &body)
			if subject.Valid || version.Valid {
				x.msg.Headers = amqp.Table{}
				if subject.Valid {
					x.msg.Headers["subjectId"] = subject.String
				}
				if version.Valid {
					x.msg.Headers["version"] = version.Int64
				}
			}
			x.msg.Body = body
			return x, err
		})
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			refused.singles = 0
			break
		}
		msgs := make([]Message, len(batch))
		for i, x := range batch {
			msgs[i] = x.msg
		}
		pctx, cancel := context.WithTimeout(ctx, publishTimeout)
		n, perr := (*pub).Publish(pctx, msgs)
		cancel()
		if n > 0 {
			seqs := make([]int64, n)
			for i := range seqs {
				seqs[i] = batch[i].seq
				delete(refused.since, batch[i].msg.Exchange)
				delete(refused.last, batch[i].msg.Exchange)
				delete(refused.parkedCodes, batch[i].msg.Exchange)
			}
			refused.confirmed = true
			// Deleting finishes even while shutting down: the broker has the messages.
			if _, err := r.DB.ExecContext(context.WithoutCancel(ctx), remove, seqs); err != nil {
				return err
			}
			published += n
		}
		if perr != nil && r.Parked != "" && IsRefused(perr) && ctx.Err() == nil {
			if len(batch) > 1 {
				refused.singles = len(batch) - n
			} else {
				x := batch[0]
				var re *RefusedError
				errors.As(perr, &re)
				now := time.Now()
				first, ok := refused.since[x.msg.Exchange]
				if !ok || now.Sub(refused.last[x.msg.Exchange]) > r.quietGap() {
					first = now
					refused.since[x.msg.Exchange] = now
				}
				refused.last[x.msg.Exchange] = now
				refusing := now.Sub(first)
				wait := refusing < r.ParkAfter // not refusing for long enough: it waits, as without Parked
				if code, ok := refused.parkedCodes[x.msg.Exchange]; ok && code == re.Code {
					wait = false // still the refusal that parked its last row
				}
				if !wait && re.Code == amqp.AccessRefused && refused.parkedCode == amqp.AccessRefused &&
					refused.parked != x.msg.Exchange && !refused.confirmed && refusing < 2*r.ParkAfter {
					r.Logger.Error("event relay: RabbitMQ refuses a second exchange's writes with nothing confirmed since the last row parked; not parking yet (a permission mistake?), retrying in order",
						"table", r.Table, "seq", x.seq, "exchange", x.msg.Exchange, "lastParked", refused.parked, "reason", perr.Error())
					wait = true
				}
				if wait {
					if err := r.renew(ctx, pub); err != nil {
						return err
					}
					return refusedWait{perr}
				}
				if err := r.park(ctx, x.seq, x.msg, perr); err != nil {
					return err
				}
				refused.parked, refused.parkedCode, refused.confirmed = x.msg.Exchange, re.Code, false
				refused.parkedCodes[x.msg.Exchange] = re.Code
				refused.singles = max(refused.singles-1, 0)
			}
			if err := r.renew(ctx, pub); err != nil {
				return err
			}
			continue
		}
		if perr != nil {
			return brokerError{perr}
		}
		if n < len(batch) {
			return brokerError{errors.New("publisher confirmed only " + strconv.Itoa(n) + " of " + strconv.Itoa(len(batch)) + " messages")}
		}
		if refused.singles > 0 {
			refused.singles--
			continue
		}
		if len(batch) < limit {
			break
		}
	}
	if published > 0 {
		r.Logger.Debug("event relay: published", "table", r.Table, "events", published)
	}
	return nil
}

// quietGap is how long an exchange may go without refusing before its refusal counts as over: a
// row waiting on it is retried at least every maxBackoff, plus a health ping's pingTimeout.
func (r *Relay) quietGap() time.Duration { return 2*r.maxBackoff + r.pingTimeout }

// renew makes *pub usable after a refusal: a new channel on the same connection when the
// publisher can (the kit's can), else, after minBackoff, a new connection.
func (r *Relay) renew(ctx context.Context, pub *Publisher) error {
	if p, ok := (*pub).(interface{ reopen() error }); ok && p.reopen() == nil {
		return nil
	}
	_ = (*pub).Close()
	*pub = nil
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(r.minBackoff):
	}
	p, err := r.Dial(ctx)
	if err != nil {
		return brokerError{err}
	}
	*pub = p
	return nil
}

// park moves a refused row from the outbox to Parked, and logs it at error level: an operator
// fixes the topology or permissions and replays it (see Relay).
func (r *Relay) park(ctx context.Context, seq int64, msg Message, refused error) error {
	cols := `seq, exchange, routing_key, type, message_id, subject_id, version, body`
	moved, err := r.DB.ExecContext(context.WithoutCancel(ctx), `WITH m AS (DELETE FROM "`+r.Table+`" WHERE seq = $1 RETURNING `+cols+`)
		INSERT INTO "`+r.Parked+`" (`+cols+`, reason) SELECT `+cols+`, $2 FROM m`, seq, refused.Error())
	if err != nil {
		return err
	}
	if n, err := moved.RowsAffected(); err == nil && n == 0 {
		// Gone already (another holder can't exist while we hold the lock; a manual delete).
		return nil
	}
	r.Logger.Error("event relay: RabbitMQ refused a message; it's parked and the relay carries on", "table", r.Table,
		"parked", r.Parked, "seq", seq, "exchange", msg.Exchange, "routingKey", msg.RoutingKey, "type", msg.Type,
		"messageId", msg.MessageID, "reason", refused.Error())
	return nil
}
