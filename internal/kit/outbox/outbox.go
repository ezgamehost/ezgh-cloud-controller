// Package outbox is the transactional outbox and its relay, shared by Trails (audit_outbox, to
// audit-api) and metering (usage_outbox, to metering-ingest): contracts/trails.md, "The relay",
// and contracts/metering.md, "The relay".
//
// A service writes each outgoing event into an outbox table in the same transaction as the change
// it describes, so "the change happened" and "the event exists" are one fact. The relay claims a
// batch with FOR UPDATE SKIP LOCKED and a lease, sends it, deletes what the receiver took, marks
// what it refused, and backs off on anything else. Several replicas can relay at once without
// sending a row twice concurrently; a relay that dies mid-batch leaves its rows to be claimed
// again once the lease runs out.
package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
)

// Defaults from the contracts.
const (
	DefaultBatchSize = 200
	// Lease is how long a claimed batch is held before another relay may take it.
	Lease = 60 * time.Second
	// SendTimeout bounds one POST.
	SendTimeout = 15 * time.Second
	// idleWait is the pause between passes when there's no backlog.
	idleWait = time.Second
)

// ErrUnknownTable is a Relay, CountsOf or Requeue given a table that isn't one of the outbox
// tables (audit_outbox, usage_outbox). It's returned before any SQL runs: the table name goes
// into the statements' text, so only these constants ever reach it.
var ErrUnknownTable = errors.New("outbox: not an outbox table")

// checkTable refuses anything but the outbox tables.
func checkTable(table string) error {
	switch table {
	case "audit_outbox", "usage_outbox":
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnknownTable, table)
}

// Issue is one problem the receiver found with an event.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Relay ships one outbox table to one receiver.
type Relay struct {
	DB *db.DB
	// Table is the outbox table: audit_outbox or usage_outbox (anything else is ErrUnknownTable).
	// A constant, never input.
	Table string
	// Name is for logs ("audit relay"); Target names the receiver ("audit-api").
	Name, Target string
	// URL is the receiver's ingest endpoint.
	URL string
	// Authorize sets the relay's credential on each request.
	Authorize func(*http.Request)
	// Order is a claimed event's sort key: batches are sent oldest first.
	Order func(event json.RawMessage) string
	// BatchSize is events per request (default 200).
	BatchSize int
	// Client sends the batches; nil is a client with SendTimeout.
	Client *http.Client
	Logger *slog.Logger
	// Explain says why the receiver rejected an event, for the log, when the relay recognizes
	// the issues (a producer bug, say).
	Explain func(issues []Issue) string
}

type row struct {
	id       string
	event    json.RawMessage
	attempts int
	order    string
}

// ingestResponse is how audit-api and metering-ingest answer a batch: rejected events are named
// by their index in the batch sent; everything else was stored (metering-ingest also counts
// duplicates, events it already had, which are just as acknowledged).
type ingestResponse struct {
	Accepted   *int `json:"accepted"`
	Duplicates int  `json:"duplicates"`
	Rejected   []struct {
		Index  int     `json:"index"`
		ID     *string `json:"id"`
		Issues []Issue `json:"issues"`
	} `json:"rejected"`
}

func (r *Relay) batchSize() int {
	if r.BatchSize < 1 {
		return DefaultBatchSize
	}
	return r.BatchSize
}

func (r *Relay) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: SendTimeout}
}

// Once claims one batch, sends it and settles it. It returns how many events it claimed.
func (r *Relay) Once(ctx context.Context) (int, error) {
	if err := checkTable(r.Table); err != nil {
		return 0, err
	}
	rows, err := r.claim(ctx)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	// Settling must finish even while the service shuts down: the batch was already sent (or
	// not), and leaving it half-settled only costs a resend after the lease.
	settleCtx := context.WithoutCancel(ctx)
	if err := r.settle(settleCtx, rows); err != nil {
		return len(rows), err
	}
	return len(rows), nil
}

// claim is service-kit's relayOnce claim: at most one batch of due, unrejected rows, oldest
// created first, leased and counted as an attempt.
func (r *Relay) claim(ctx context.Context) ([]row, error) {
	q := fmt.Sprintf(`
		UPDATE %[1]s SET available_at = now() + make_interval(secs => $2), attempts = attempts + 1
		WHERE id IN (
			SELECT id FROM %[1]s
			WHERE rejected_at IS NULL AND available_at <= now()
			ORDER BY created_at LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id::text, event::text, attempts`, db.QuoteIdent(r.Table))
	rs, err := r.DB.QueryContext(ctx, q, r.batchSize(), Lease.Seconds())
	if err != nil {
		return nil, err
	}
	var out []row
	for rs.Next() {
		var x row
		var event string
		if err := rs.Scan(&x.id, &event, &x.attempts); err != nil {
			_ = rs.Close()
			return nil, err
		}
		x.event = json.RawMessage(event)
		if r.Order != nil {
			x.order = r.Order(x.event)
		}
		out = append(out, x)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b row) int { return strings.Compare(a.order, b.order) })
	return out, nil
}

type outcome struct {
	body     *ingestResponse
	tooLarge bool
	err      string
	// loud marks a failure a person must fix (a bad credential, a body the receiver can't read),
	// logged as an error; outages that fix themselves are warnings.
	loud bool
}

func (r *Relay) send(ctx context.Context, batch []row) outcome {
	var buf bytes.Buffer
	buf.WriteString(`{"events":[`)
	for i, x := range batch {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(x.event)
	}
	buf.WriteString(`]}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, &buf)
	if err != nil {
		return outcome{err: err.Error(), loud: true}
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Authorize != nil {
		r.Authorize(req)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return outcome{err: fmt.Sprintf("%s unreachable: %v", r.Target, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		return outcome{tooLarge: true}
	}
	if resp.StatusCode != http.StatusOK {
		loud := resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
		return outcome{err: fmt.Sprintf("%s answered %d", r.Target, resp.StatusCode), loud: loud}
	}
	var parsed ingestResponse
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Accepted == nil || parsed.Rejected == nil {
		return outcome{err: r.Target + " answered with an unexpected body", loud: true}
	}
	return outcome{body: &parsed}
}

func (r *Relay) settle(ctx context.Context, batch []row) error {
	sent := r.send(ctx, batch)
	switch {
	case sent.tooLarge:
		// Over the receiver's size limit: send each half on its own, down to single events. An
		// event still refused alone can never be sent, so it's rejected, not retried forever.
		if len(batch) > 1 {
			half := (len(batch) + 1) / 2
			if err := r.settle(ctx, batch[:half]); err != nil {
				return err
			}
			return r.settle(ctx, batch[half:])
		}
		return r.reject(ctx, batch[0].id, []Issue{{Path: "", Message: "payload_too_large: " + r.Target + " won't take this event even on its own"}})
	case sent.err != "":
		level := slog.LevelWarn
		if sent.loud {
			level = slog.LevelError
		}
		r.Logger.Log(ctx, level, r.Name+": send failed, will retry", "error", sent.err, "events", len(batch))
		return r.backOff(ctx, ids(batch), sent.err)
	}

	// A rejection names its event by index; when it also names an ID, the two must agree. One
	// that doesn't match the batch can't be trusted for any event, so those are retried rather
	// than deleted or rejected.
	rejected := map[string][]Issue{}
	unclear := map[string]bool{}
	for _, rej := range sent.body.Rejected {
		if rej.Index >= 0 && rej.Index < len(batch) && (rej.ID == nil || *rej.ID == batch[rej.Index].id) {
			rejected[batch[rej.Index].id] = rej.Issues
			continue
		}
		r.Logger.Error(r.Name+": "+r.Target+" rejected an event that doesn't match the batch", "index", rej.Index, "id", rej.ID)
		if rej.Index >= 0 && rej.Index < len(batch) {
			unclear[batch[rej.Index].id] = true
		}
		if rej.ID != nil {
			for _, x := range batch {
				if x.id == *rej.ID {
					unclear[x.id] = true
				}
			}
		}
	}
	var done []string
	for _, x := range batch {
		if _, bad := rejected[x.id]; !bad && !unclear[x.id] {
			done = append(done, x.id)
		}
	}
	if len(done) > 0 {
		q := fmt.Sprintf(`DELETE FROM %s WHERE id = ANY($1)`, db.QuoteIdent(r.Table))
		if _, err := r.DB.ExecContext(ctx, q, done); err != nil {
			return err
		}
	}
	for id, issues := range rejected {
		if unclear[id] {
			continue
		}
		if err := r.reject(ctx, id, issues); err != nil {
			return err
		}
	}
	if len(unclear) > 0 {
		var retry []string
		for id := range unclear {
			retry = append(retry, id)
		}
		return r.backOff(ctx, retry, r.Target+" answered with a rejection that doesn't match the batch")
	}
	return nil
}

// backOff pushes rows out by 5 s × 2^(attempts − 1), capped at 5 minutes, per row.
func (r *Relay) backOff(ctx context.Context, rowIDs []string, reason string) error {
	q := fmt.Sprintf(`
		UPDATE %s
		SET available_at = now() + least(interval '5 minutes', interval '5 seconds' * power(2, least(attempts - 1, 10))),
		    last_error = $2
		WHERE id = ANY($1)`, db.QuoteIdent(r.Table))
	_, err := r.DB.ExecContext(ctx, q, rowIDs, reason)
	return err
}

// reject keeps the event for a person and never resends it: a rejection is a bug in the
// producer (or a registry change it's waiting on), not an outage.
func (r *Relay) reject(ctx context.Context, id string, issues []Issue) error {
	args := []any{"id", id, "issues", issues}
	if r.Explain != nil {
		if why := r.Explain(issues); why != "" {
			args = append(args, "why", why)
		}
	}
	r.Logger.Error(r.Name+": "+r.Target+" rejected an event", args...)
	reason, _ := json.Marshal(issues)
	q := fmt.Sprintf(`UPDATE %s SET rejected_at = now(), last_error = $2 WHERE id = $1 AND rejected_at IS NULL`, db.QuoteIdent(r.Table))
	_, err := r.DB.ExecContext(ctx, q, id, string(reason))
	return err
}

func ids(batch []row) []string {
	out := make([]string, len(batch))
	for i, x := range batch {
		out[i] = x.id
	}
	return out
}

// Run relays until ctx is done: straight on while there's a backlog (a full batch was claimed),
// otherwise once a second. Errors (the database being down) are logged and retried; they never
// stop the service. On shutdown it returns after the batch in hand is settled.
func (r *Relay) Run(ctx context.Context) {
	for {
		claimed, err := r.Once(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			r.Logger.Error(r.Name+" failed", "error", err.Error())
		}
		if ctx.Err() != nil {
			return
		}
		if claimed >= r.batchSize() {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(idleWait):
		}
	}
}

// Counts is an outbox's state, for metrics and alerts: pending (waiting, not rejected), due now,
// rejected (alert on any), and the oldest pending row's age.
type Counts struct {
	Pending, Due, Rejected int
	OldestPending          *time.Time
}

// CountsOf reads an outbox's counts. table is audit_outbox or usage_outbox.
func CountsOf(ctx context.Context, d *db.DB, table string) (Counts, error) {
	var c Counts
	if err := checkTable(table); err != nil {
		return c, err
	}
	q := fmt.Sprintf(`
		SELECT count(*) FILTER (WHERE rejected_at IS NULL),
		       count(*) FILTER (WHERE rejected_at IS NULL AND available_at <= now()),
		       count(*) FILTER (WHERE rejected_at IS NOT NULL),
		       min(created_at) FILTER (WHERE rejected_at IS NULL)
		FROM %s`, db.QuoteIdent(table))
	err := d.QueryRowContext(ctx, q).Scan(&c.Pending, &c.Due, &c.Rejected, &c.OldestPending)
	return c, err
}

// Requeue puts rejected events back in line (after the receiver's rules or the producer were
// fixed), with their attempts reset. Never give a rejected usage event a new ID instead.
func Requeue(ctx context.Context, d *db.DB, table string, rowIDs []string) (int64, error) {
	if err := checkTable(table); err != nil {
		return 0, err
	}
	q := fmt.Sprintf(`UPDATE %s SET rejected_at = NULL, available_at = now(), attempts = 0 WHERE rejected_at IS NOT NULL AND id = ANY($1)`, db.QuoteIdent(table))
	return db.RowsAffected(d.ExecContext(ctx, q, rowIDs))
}
