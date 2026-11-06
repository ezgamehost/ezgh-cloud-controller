package metering

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/decimal"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/organizationclosure"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/usageoutbox"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/usage"
)

// Watermarks sends the hourly "complete through" reports for one region (contracts/metering.md,
// "Watermarks"). Metering can't tell "no usage" from "the producer is behind" by looking at
// events, so each hour, once the producer's own hourly work for it has committed and the outbox
// holds nothing pending from before it, the producer says so, with control totals ingest checks
// against what it stored.
type Watermarks struct {
	DB       *db.DB
	Client   *Client
	Producer string
	Region   string
	// Meters are the meters the producer owns; each gets a count, zero included.
	Meters []string
	// Ready reports whether the product's hourly work for the hour ending at completeThrough
	// (interval checkpoints, gauge snapshots) has committed.
	Ready func(ctx context.Context, completeThrough time.Time) (bool, error)
	// Grace is how long after an hour ends before its watermark is considered, so a change that
	// read the clock just before the boundary has committed (default one minute).
	Grace  time.Duration
	Logger *slog.Logger
}

// Once sends every watermark that's due, oldest first, then re-sends any hour already
// watermarked whose control totals have changed since (Resend), and returns how many it sent. A
// watermark that isn't ready yet stops the forward pass; the next pass tries again.
func (w *Watermarks) Once(ctx context.Context, now time.Time) (int, error) {
	sent, err := w.forward(ctx, now)
	if err != nil {
		return sent, err
	}
	resent, err := w.Resend(ctx)
	return sent + resent, err
}

func (w *Watermarks) forward(ctx context.Context, now time.Time) (int, error) {
	grace := w.Grace
	if grace == 0 {
		grace = time.Minute
	}
	next := now.Add(-grace).UTC().Truncate(time.Hour)
	last, err := entdb.Client(w.DB).UsageWatermark.Get(ctx, w.Region)
	switch {
	case err == nil:
		next = last.CompleteThrough.UTC().Add(time.Hour)
	case !ent.IsNotFound(err): // not found: nothing sent yet
		return 0, err
	}
	sent := 0
	for !next.Add(grace).After(now) {
		if w.Ready != nil {
			ready, err := w.Ready(ctx, next)
			if err != nil || !ready {
				return sent, err
			}
		}
		drained, err := drainedBefore(ctx, w.DB, next)
		if err != nil || !drained {
			return sent, err
		}
		if err := w.Send(ctx, next); err != nil {
			return sent, err
		}
		sent++
		next = next.Add(time.Hour)
	}
	return sent, nil
}

// Send posts the watermark for completeThrough with the control totals of the hour before it,
// and records it. It's also how an hour is re-sent after a correction: the same completeThrough,
// new counts (the watermark itself never moves back).
func (w *Watermarks) Send(ctx context.Context, completeThrough time.Time) error {
	// Taken before the totals are read: a change committed after the read has a later changed_at
	// (within resendMargin), so Resend looks at the hour again.
	var checked time.Time
	if err := w.DB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&checked); err != nil {
		return err
	}
	counts, err := ControlTotals(ctx, w.DB, completeThrough.Add(-time.Hour), w.Meters)
	if err != nil {
		return err
	}
	return w.send(ctx, completeThrough, counts, checked)
}

func (w *Watermarks) send(ctx context.Context, completeThrough time.Time, counts map[string]Counts, checked time.Time) error {
	wm := Watermark{Producer: w.Producer, Region: w.Region, CompleteThrough: usage.IDTime(completeThrough), Counts: counts}
	if err := w.Client.SendWatermark(ctx, wm); err != nil {
		var refused *RefusedError
		if errors.As(err, &refused) && refused.Status == http.StatusBadRequest {
			w.Logger.Error("metering-ingest refused a watermark; that's a producer bug", "completeThrough", wm.CompleteThrough, "error", err.Error())
		}
		return err
	}
	if err := recordWatermark(ctx, w.DB, w.Region, completeThrough); err != nil {
		return err
	}
	body, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	if _, err := w.DB.ExecContext(ctx, `INSERT INTO usage_watermark_hours (region, complete_through, counts, sent_at, checked_at)
		VALUES ($1, $2, $3::text::jsonb, now(), $4)
		ON CONFLICT (region, complete_through) DO UPDATE SET counts = excluded.counts, sent_at = now(), checked_at = excluded.checked_at`,
		w.Region, completeThrough, string(body), checked); err != nil {
		return err
	}
	w.Logger.Info("watermark sent", "completeThrough", wm.CompleteThrough, "counts", counts)
	return nil
}

// resendMargin is how far before an hour's last check a change still counts as possibly unseen:
// a transaction that read the clock before the check may commit after it. Longer than any
// producer transaction that writes usage.
const resendMargin = 5 * time.Minute

// Resend re-sends the watermark of every hour already watermarked whose control totals changed
// since it was last checked (a correction, a late event, a withdrawal) and now differ from what
// its watermark said (contracts/metering.md, "Watermarks": the same completeThrough, new counts;
// the watermark itself never moves back). An hour with usage still pending in the outbox waits,
// so ingest compares counts it has. Returns how many it re-sent.
func (w *Watermarks) Resend(ctx context.Context) (int, error) {
	rows, err := w.DB.QueryContext(ctx, `SELECT DISTINCT c.usage_hour + interval '1 hour'
		FROM usage_control_totals c
		JOIN usage_watermarks m ON m.region = $1
		LEFT JOIN usage_watermark_hours h ON h.region = $1 AND h.complete_through = c.usage_hour + interval '1 hour'
		WHERE c.changed_at > '-infinity' AND c.usage_hour + interval '1 hour' <= m.complete_through
			AND (h.checked_at IS NULL OR c.changed_at > h.checked_at - $2 * interval '1 millisecond')
		ORDER BY 1 LIMIT 200`, w.Region, resendMargin.Milliseconds())
	hours, err := db.Collect(rows, err, func(r *sql.Rows) (time.Time, error) {
		var t time.Time
		err := r.Scan(&t)
		return t.UTC(), err
	})
	if err != nil {
		return 0, err
	}
	resent := 0
	for _, through := range hours {
		pending, err := pendingIn(ctx, w.DB, through.Add(-time.Hour), through)
		if err != nil {
			return resent, err
		}
		if pending {
			continue // the next pass, once it's in metering's hands
		}
		var checked time.Time
		if err := w.DB.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&checked); err != nil {
			return resent, err
		}
		counts, err := ControlTotals(ctx, w.DB, through.Add(-time.Hour), w.Meters)
		if err != nil {
			return resent, err
		}
		var sentBody []byte
		err = w.DB.QueryRowContext(ctx, `SELECT counts::text FROM usage_watermark_hours WHERE region = $1 AND complete_through = $2`,
			w.Region, through).Scan(&sentBody)
		if err != nil && !db.IsNoRows(err) {
			return resent, err
		}
		var sent map[string]Counts
		if err == nil {
			if err := json.Unmarshal(sentBody, &sent); err != nil {
				return resent, err
			}
		}
		if err == nil && reflect.DeepEqual(sent, counts) {
			if _, err := w.DB.ExecContext(ctx, `UPDATE usage_watermark_hours SET checked_at = $3 WHERE region = $1 AND complete_through = $2`,
				w.Region, through, checked); err != nil {
				return resent, err
			}
			continue
		}
		if err := w.send(ctx, through, counts, checked); err != nil {
			return resent, err
		}
		w.Logger.Info("watermark re-sent: the hour's control totals changed", "completeThrough", usage.IDTime(through))
		resent++
	}
	return resent, nil
}

// pendingIn reports whether the outbox holds usage (not rejected) in [from, to).
func pendingIn(ctx context.Context, d *db.DB, from, to time.Time) (bool, error) {
	var pending bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM usage_outbox WHERE rejected_at IS NULL AND `+usageTimeSQL+` >= $1 AND `+usageTimeSQL+` < $2)`,
		from, to).Scan(&pending)
	return pending, err
}

// Withdrawn is what Withdraw took out: events and, per meter, their count and quantity.
type Withdrawn struct {
	Events int
	Counts map[string]Counts
}

// Withdraw drops the organization's usage events that can't be sent any more from the outbox
// and from the control totals, in tx: those not yet relayed, and those ingest rejected with one of
// rejectedCodes (organization_closed, unknown_project). For an organization org-api reports
// deleted, ingest refuses its usage whatever its time (organization_closed), so it's billed by a
// staff adjustment if at all (contracts/metering.md, "The ingest API"): the caller logs what was
// withdrawn for that person. The hours whose totals changed get their watermarks re-sent
// (Resend). Withdrawing again finds nothing.
func Withdraw(ctx context.Context, tx *db.Tx, organizationID string, rejectedCodes ...string) (Withdrawn, error) {
	out := Withdrawn{Counts: map[string]Counts{}}
	patterns := make([]string, len(rejectedCodes))
	for i, code := range rejectedCodes {
		// The relay stores a rejection's issues as JSON: [{"path":…,"message":"<code>: …"}].
		patterns[i] = `%"message":"` + code + `:%`
	}
	rows, err := tx.QueryContext(ctx, `DELETE FROM usage_outbox
		WHERE event->>'organizationId' = $1 AND (rejected_at IS NULL OR last_error LIKE ANY($2::text[]))
		RETURNING event::text`, organizationID, patterns)
	events, err := db.Collect(rows, err, func(r *sql.Rows) (usage.Event, error) {
		var raw string
		var e usage.Event
		if err := r.Scan(&raw); err != nil {
			return e, err
		}
		return e, json.Unmarshal([]byte(raw), &e)
	})
	if err != nil {
		return out, err
	}
	type key struct {
		hour  time.Time
		meter string
	}
	sums := map[key]Counts{}
	for _, e := range events {
		k := key{e.UsageTime().Truncate(time.Hour), e.Meter}
		q, ok := decimal.Parse(e.Quantity)
		if !ok {
			return out, fmt.Errorf("metering: %s's quantity %q isn't a decimal", e.ID, e.Quantity)
		}
		sums[k] = add(sums[k], q)
		out.Counts[e.Meter] = add(out.Counts[e.Meter], q)
		out.Events++
	}
	for k, c := range sums {
		if _, err := tx.ExecContext(ctx, `UPDATE usage_control_totals SET events = events - $3, quantity = quantity - $4::numeric, changed_at = now()
			WHERE usage_hour = $1 AND meter = $2`, k.hour, k.meter, c.Events, c.Quantity); err != nil {
			return out, err
		}
	}
	return out, nil
}

// add counts one more event of quantity q.
func add(c Counts, q decimal.Decimal) Counts {
	total := q
	if c.Quantity != "" {
		total = decimal.MustParse(c.Quantity).Add(q)
	}
	return Counts{Events: c.Events + 1, Quantity: total.String()}
}

// recordWatermark records that the region's watermark was sent through completeThrough. It
// never moves back: re-sending an earlier hour keeps the later one, and only sent_at changes.
func recordWatermark(ctx context.Context, q db.Driverer, region string, completeThrough time.Time) error {
	return entdb.Client(q).UsageWatermark.Create().
		SetID(region).
		SetCompleteThrough(completeThrough).
		OnConflict(
			entsql.ConflictColumns("region"),
			entsql.ResolveWith(func(u *entsql.UpdateSet) {
				u.Set("complete_through", entsql.Expr("greatest(usage_watermarks.complete_through, excluded.complete_through)"))
				u.Set("sent_at", entsql.Expr("now()"))
			}),
		).
		Exec(ctx) // an unconditional DO UPDATE: the row is always there to return
}

// usageTimeSQL is an outbox event's usage time: its window's start or its instant.
const usageTimeSQL = `(coalesce(event #>> '{window,start}', event ->> 'occurredAt'))::timestamptz`

// drainedBefore reports whether the outbox holds nothing pending (not rejected) with usage
// before t: everything before t is in metering's hands. Rejected events are a person's problem,
// alerted on separately; they don't hold the watermark back.
func drainedBefore(ctx context.Context, d *db.DB, t time.Time) (bool, error) {
	var pending bool
	err := d.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM usage_outbox WHERE rejected_at IS NULL AND `+usageTimeSQL+` < $1)`, t).Scan(&pending)
	return !pending, err
}

// ---- Organization closures ----------------------------------------------------------------

// MarkClosurePending records, in the transaction that removed the producer's last resource in a
// deleted organization (or found it had none), that the producer is done with it as of
// completeThrough. The closure is sent once the outbox has drained. Marking again changes
// nothing.
func MarkClosurePending(ctx context.Context, tx *db.Tx, organizationID string, completeThrough time.Time) error {
	return markClosurePending(ctx, tx, organizationID, completeThrough)
}

func markClosurePending(ctx context.Context, q db.Driverer, organizationID string, completeThrough time.Time) error {
	_, err := entdb.Client(q).OrganizationClosure.Create().
		SetID(organizationID).
		SetCompleteThrough(completeThrough).
		OnConflictColumns("organization_id").
		DoNothing().
		ID(ctx)
	_, err = db.Inserted(err) // already pending (or sent): the first mark stands
	return err
}

// OrganizationNotClosed is a predicate for any product query: the organization in col (a column
// of the query's table, qualified with it) has no closure, pending or sent. As
// predicate.X(metering.OrganizationNotClosed(x.FieldOrganizationID)) it renders
// NOT EXISTS (SELECT 1 FROM organization_closures oc WHERE oc.organization_id = "x"."organization_id").
func OrganizationNotClosed(col string) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("NOT EXISTS (SELECT 1 FROM organization_closures oc WHERE oc.organization_id = ").
				Ident(s.C(col)).
				WriteByte(')')
		}))
	}
}

// ClosureRecord is an organization's closure as the producer recorded it: pending until SentAt
// is set. (Closure is what's sent.)
type ClosureRecord struct {
	OrganizationID  string
	CompleteThrough time.Time
	CreatedAt       time.Time
	// SentAt is when metering-ingest accepted the closure; nil while it's pending.
	SentAt *time.Time
}

// Sent reports whether the closure has been sent.
func (r *ClosureRecord) Sent() bool { return r.SentAt != nil }

// ClosureOf is the organization's closure, or nil when the producer hasn't closed it.
func ClosureOf(ctx context.Context, q db.Querier, organizationID string) (*ClosureRecord, error) {
	c, err := entdb.Client(q).OrganizationClosure.Get(ctx, organizationID)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &ClosureRecord{OrganizationID: c.ID, CompleteThrough: c.CompleteThrough, CreatedAt: c.CreatedAt, SentAt: c.SentAt}, nil
}

// Closures sends pending organization closures (contracts/metering.md, "Organization closure").
// The final invoice waits for a closure from every registered producer.
type Closures struct {
	DB       *db.DB
	Client   *Client
	Producer string
	Logger   *slog.Logger
}

// Once sends each pending closure whose organization's usage has all been relayed.
func (c *Closures) Once(ctx context.Context) (int, error) {
	list, err := pendingClosures(ctx, c.DB)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, p := range list {
		waiting, err := usagePending(ctx, c.DB, p.ID)
		if err != nil {
			return sent, err
		}
		if waiting {
			continue
		}
		cl := Closure{Producer: c.Producer, OrganizationID: p.ID, CompleteThrough: usage.IDTime(p.CompleteThrough)}
		if err := c.Client.SendClosure(ctx, cl); err != nil {
			c.Logger.Warn("sending an organization closure failed; will retry", "organizationId", p.ID, "error", err.Error())
			continue
		}
		if err := closureSent(ctx, c.DB, p.ID); err != nil {
			return sent, err
		}
		c.Logger.Info("organization closure sent", "organizationId", p.ID, "completeThrough", cl.CompleteThrough)
		sent++
	}
	return sent, nil
}

// pendingClosures are the closures not sent yet, oldest first, at most 100 a pass.
func pendingClosures(ctx context.Context, q db.Driverer) ([]*ent.OrganizationClosure, error) {
	return entdb.Client(q).OrganizationClosure.Query().
		Where(organizationclosure.SentAtIsNil()).
		Order(organizationclosure.ByCreatedAt()).
		Limit(100).
		All(ctx)
}

// usagePending reports whether the outbox still holds usage (not rejected) for the organization.
// event->>'organizationId' with rejected_at IS NULL is usage_outbox_pending_org_idx.
func usagePending(ctx context.Context, q db.Driverer, organizationID string) (bool, error) {
	return entdb.Client(q).UsageOutbox.Query().
		Where(
			usageoutbox.RejectedAtIsNil(),
			func(s *entsql.Selector) {
				s.Where(sqljson.ValueEQ(usageoutbox.FieldEvent, organizationID, sqljson.Path("organizationId")))
			},
		).
		Exist(ctx)
}

// closureSent marks the organization's closure sent.
func closureSent(ctx context.Context, q db.Driverer, organizationID string) error {
	_, err := entdb.Client(q).OrganizationClosure.Update().
		Where(organizationclosure.ID(organizationID)).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(organizationclosure.FieldSentAt, entsql.Expr("now()")) }).
		Save(ctx)
	return err
}
