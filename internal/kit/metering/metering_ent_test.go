package metering

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/operation"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/predicate"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// The ent statements here are checked as SQL (what Postgres is sent), and against the database.

const (
	entOrg      = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	entOtherOrg = "0199a3c2-7d3a-7f62-9b5c-4e0a8d3f6b92"
)

type statement struct {
	query string
	args  []any
}

// recorder is a db.Driverer whose driver records every statement before running it.
type recorder struct {
	q          db.Driverer
	mu         sync.Mutex
	statements []statement
}

func (r *recorder) Driver() dialect.Driver { return recordingDriver{Driver: r.q.Driver(), r: r} }

func (r *recorder) only(t *testing.T) statement {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.statements) != 1 {
		t.Fatalf("%d statements, want one: %v", len(r.statements), r.statements)
	}
	return r.statements[0]
}

type recordingDriver struct {
	dialect.Driver
	r *recorder
}

func (d recordingDriver) add(query string, args any) {
	a, _ := args.([]any)
	d.r.mu.Lock()
	d.r.statements = append(d.r.statements, statement{query, a})
	d.r.mu.Unlock()
}

func (d recordingDriver) Exec(ctx context.Context, query string, args, v any) error {
	d.add(query, args)
	return d.Driver.Exec(ctx, query, args, v)
}

func (d recordingDriver) Query(ctx context.Context, query string, args, v any) error {
	d.add(query, args)
	return d.Driver.Query(ctx, query, args, v)
}

func exec(t *testing.T, d *testdb.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func runTx(t *testing.T, d *testdb.DB, fn func(*db.Tx) error) {
	t.Helper()
	if err := d.DB.RunTx(context.Background(), nil, fn); err != nil {
		t.Fatal(err)
	}
}

type watermarkRow struct {
	completeThrough time.Time
	sentAt          time.Time
}

func watermark(t *testing.T, d *testdb.DB, region string) watermarkRow {
	t.Helper()
	var w watermarkRow
	if err := d.Pool.QueryRow(context.Background(), `SELECT complete_through, sent_at FROM usage_watermarks WHERE region = $1`, region).Scan(&w.completeThrough, &w.sentAt); err != nil {
		t.Fatal(err)
	}
	return w
}

// Recording a watermark is the same single upsert as the SQL it replaced: the watermark never
// moves back, and sending stamps sent_at with the database's clock.
func TestRecordWatermark(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	hour := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

	r := &recorder{q: d.DB}
	if err := recordWatermark(ctx, r, "us-west-1", hour); err != nil {
		t.Fatal(err)
	}
	got := r.only(t)
	want := `INSERT INTO "usage_watermarks" ("complete_through", "region") VALUES ($1, $2) ON CONFLICT ("region") DO UPDATE SET "complete_through" = greatest(usage_watermarks.complete_through, excluded.complete_through), "sent_at" = now() RETURNING "region"`
	if got.query != want {
		t.Errorf("upsert\n got %s\nwant %s", got.query, want)
	}
	if len(got.args) != 2 || got.args[1] != "us-west-1" {
		t.Errorf("args %v", got.args)
	}
	first := watermark(t, d, "us-west-1")
	if !first.completeThrough.Equal(hour) || first.completeThrough.Location() != time.UTC {
		t.Errorf("complete_through %v", first.completeThrough)
	}

	// Backdate sent_at, so a later stamp is visible.
	exec(t, d, `UPDATE usage_watermarks SET sent_at = sent_at - interval '1 hour'`)
	backdated := watermark(t, d, "us-west-1")

	// Re-sending an earlier hour (a correction) never moves the watermark back, but is a send.
	if err := recordWatermark(ctx, d.DB, "us-west-1", hour.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	again := watermark(t, d, "us-west-1")
	if !again.completeThrough.Equal(hour) {
		t.Errorf("watermark moved back to %v", again.completeThrough)
	}
	if !again.sentAt.After(backdated.sentAt) {
		t.Errorf("sent_at %v not stamped (was %v)", again.sentAt, backdated.sentAt)
	}
	// A later hour moves it on.
	if err := recordWatermark(ctx, d.DB, "us-west-1", hour.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if w := watermark(t, d, "us-west-1"); !w.completeThrough.Equal(hour.Add(time.Hour)) {
		t.Errorf("watermark %v", w.completeThrough)
	}
	// Regions are independent.
	if err := recordWatermark(ctx, d.DB, "eu-central-1", hour.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if w := watermark(t, d, "eu-central-1"); !w.completeThrough.Equal(hour.Add(-time.Hour)) {
		t.Errorf("eu-central-1 %v", w.completeThrough)
	}
	if w := watermark(t, d, "us-west-1"); !w.completeThrough.Equal(hour.Add(time.Hour)) {
		t.Errorf("us-west-1 %v", w.completeThrough)
	}
}

type recordingIngest struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (i *recordingIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	i.mu.Lock()
	i.bodies = append(i.bodies, body)
	i.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// With no watermark sent yet, the first is the last hour that's complete (after the grace).
func TestWatermarksStartAtTheLastCompleteHour(t *testing.T) {
	d := testdb.New(t)
	ingest := &recordingIngest{}
	server := httptest.NewServer(ingest)
	defer server.Close()
	w := &Watermarks{
		DB: d.DB, Client: &Client{URL: server.URL, Credential: "pk_test"}, Producer: "widgets", Region: "us-west-1",
		Meters: []string{"widgets.widget.uptime"}, Logger: testdb.Logger(),
	}
	now := time.Date(2026, 9, 28, 12, 5, 0, 0, time.FixedZone("PDT", -7*3600))
	if n, err := w.Once(context.Background(), now); err != nil || n != 1 {
		t.Fatalf("sent %d, %v", n, err)
	}
	if got := ingest.bodies[0]["completeThrough"]; got != "2026-09-28T19:00:00Z" {
		t.Errorf("completeThrough %v", got)
	}
	if wm := watermark(t, d, "us-west-1"); !wm.completeThrough.Equal(time.Date(2026, 9, 28, 19, 0, 0, 0, time.UTC)) {
		t.Errorf("recorded %v", wm.completeThrough)
	}
	// It's read back next time: nothing more is due.
	if n, err := w.Once(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("sent %d more, %v", n, err)
	}
}

// Marking a closure twice keeps the first, without failing (or poisoning) the transaction.
func TestMarkClosurePendingKeepsTheFirst(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	first := time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC)
	runTx(t, d, func(tx *db.Tx) error {
		r := &recorder{q: tx}
		if err := markClosurePending(ctx, r, entOrg, first); err != nil {
			return err
		}
		want := `INSERT INTO "organization_closures" ("complete_through", "organization_id") VALUES ($1, $2) ON CONFLICT ("organization_id") DO NOTHING RETURNING "organization_id"`
		if got := r.only(t).query; got != want {
			t.Errorf("insert\n got %s\nwant %s", got, want)
		}
		if err := MarkClosurePending(ctx, tx, entOrg, first.Add(time.Hour)); err != nil {
			return err
		}
		// The transaction goes on: the skipped insert raised nothing.
		_, err := tx.ExecContext(ctx, `SELECT 1`)
		return err
	})
	runTx(t, d, func(tx *db.Tx) error { return MarkClosurePending(ctx, tx, entOrg, first.Add(2*time.Hour)) })

	c, err := ClosureOf(ctx, d.DB, entOrg)
	if err != nil || c == nil {
		t.Fatalf("closure %v, %v", c, err)
	}
	if c.OrganizationID != entOrg || !c.CompleteThrough.Equal(first) || c.CompleteThrough.Location() != time.UTC || c.Sent() {
		t.Errorf("closure %+v", c)
	}
	var n int
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM organization_closures`).Scan(&n)
	if n != 1 {
		t.Errorf("%d closures", n)
	}
}

// ClosureOf is nil until the organization is closed, then pending until the closure is sent.
func TestClosureOf(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if c, err := ClosureOf(ctx, d.DB, entOrg); err != nil || c != nil {
		t.Fatalf("closure of an open organization: %+v, %v", c, err)
	}
	at := time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC)
	runTx(t, d, func(tx *db.Tx) error {
		if err := MarkClosurePending(ctx, tx, entOrg, at); err != nil {
			return err
		}
		// Inside the transaction that marked it, too.
		c, err := ClosureOf(ctx, tx, entOrg)
		if err != nil || c == nil || c.Sent() {
			t.Errorf("in the transaction: %+v, %v", c, err)
		}
		return nil
	})
	pending, err := ClosureOf(ctx, d.DB, entOrg)
	if err != nil || pending == nil || pending.Sent() || pending.CreatedAt.IsZero() || !pending.CompleteThrough.Equal(at) {
		t.Fatalf("pending: %+v, %v", pending, err)
	}

	ingest := &recordingIngest{}
	server := httptest.NewServer(ingest)
	defer server.Close()
	c := &Closures{DB: d.DB, Client: &Client{URL: server.URL, Credential: "pk_test"}, Producer: "widgets", Logger: testdb.Logger()}
	if n, err := c.Once(ctx); err != nil || n != 1 {
		t.Fatalf("sent %d, %v", n, err)
	}
	sent, err := ClosureOf(ctx, d.DB, entOrg)
	if err != nil || sent == nil || !sent.Sent() || sent.SentAt.Location() != time.UTC {
		t.Fatalf("sent: %+v, %v", sent, err)
	}
	if !sent.CreatedAt.Equal(pending.CreatedAt) || !sent.CompleteThrough.Equal(at) {
		t.Errorf("sent %+v, was %+v", sent, pending)
	}
	if other, err := ClosureOf(ctx, d.DB, entOtherOrg); err != nil || other != nil {
		t.Errorf("other organization: %+v, %v", other, err)
	}
}

func insertOperation(t *testing.T, d *testdb.DB, org string) {
	t.Helper()
	exec(t, d, `INSERT INTO operations (id, organization_id, type, action, status, target_id, target_resource_name, target_type, created_by)
		VALUES (gen_random_uuid(), $1, 'CreateWidget', 'widgets:CreateWidget', 'queued', gen_random_uuid(), 'r', 'widget', '{}')`, org)
}

// OrganizationNotClosed composes into another table's query, against that table's column,
// whatever the closure's state.
func TestOrganizationNotClosed(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	insertOperation(t, d, entOrg)
	insertOperation(t, d, entOtherOrg)
	insertOperation(t, d, entOtherOrg)

	open := func() []string {
		t.Helper()
		r := &recorder{q: d.DB}
		var orgs []string
		err := entdb.Client(r).Operation.Query().
			Where(predicate.Operation(OrganizationNotClosed(operation.FieldOrganizationID))).
			Unique(true).
			Select(operation.FieldOrganizationID).
			Scan(ctx, &orgs)
		if err != nil {
			t.Fatal(err)
		}
		q := r.only(t).query
		if !strings.Contains(q, `NOT EXISTS (SELECT 1 FROM organization_closures oc WHERE oc.organization_id = "operations"."organization_id")`) {
			t.Errorf("query %s", q)
		}
		slices.Sort(orgs)
		return orgs
	}
	if got := open(); !slices.Equal(got, []string{entOrg, entOtherOrg}) {
		t.Fatalf("before any closure: %v", got)
	}
	runTx(t, d, func(tx *db.Tx) error {
		return MarkClosurePending(ctx, tx, entOtherOrg, time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC))
	})
	if got := open(); !slices.Equal(got, []string{entOrg}) {
		t.Errorf("with %s's closure pending: %v", entOtherOrg, got)
	}
	exec(t, d, `UPDATE organization_closures SET sent_at = now()`)
	if got := open(); !slices.Equal(got, []string{entOrg}) {
		t.Errorf("with %s's closure sent: %v", entOtherOrg, got)
	}
	// And in a count (another statement shape) and a delete.
	n, err := entdb.Client(d.DB).Operation.Query().Where(predicate.Operation(OrganizationNotClosed(operation.FieldOrganizationID))).Count(ctx)
	if err != nil || n != 1 {
		t.Errorf("count %d, %v", n, err)
	}
	deleted, err := entdb.Client(d.DB).Operation.Delete().Where(predicate.Operation(OrganizationNotClosed(operation.FieldOrganizationID))).Exec(ctx)
	if err != nil || deleted != 1 {
		t.Errorf("deleted %d, %v", deleted, err)
	}
	var left int
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FROM operations WHERE organization_id = $1`, entOtherOrg).Scan(&left)
	if left != 2 {
		t.Errorf("%d of the closed organization's operations left", left)
	}
}

func outboxEvent(t *testing.T, d *testdb.DB, id, org string, rejected bool) {
	t.Helper()
	event, _ := json.Marshal(map[string]any{"id": id, "organizationId": org, "occurredAt": "2026-09-28T10:00:00Z"})
	exec(t, d, `INSERT INTO usage_outbox (id, event, rejected_at) VALUES ($1, $2::text::jsonb, CASE WHEN $3 THEN now() END)`, id, string(event), rejected)
}

// usagePending looks only at the organization's events that aren't rejected, and does so on the
// expression index usage_outbox_pending_org_idx, as the SQL it replaced did.
func TestUsagePending(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	pending := func(org string) bool {
		t.Helper()
		p, err := usagePending(ctx, d.DB, org)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if pending(entOrg) {
		t.Fatal("pending with an empty outbox")
	}
	outboxEvent(t, d, "a", entOtherOrg, false)
	outboxEvent(t, d, "b", entOrg, true)
	if pending(entOrg) {
		t.Error("a rejected event (or another organization's) counted as pending")
	}
	if !pending(entOtherOrg) {
		t.Error("the other organization's event isn't pending")
	}
	outboxEvent(t, d, "c", entOrg, false)
	if !pending(entOrg) {
		t.Error("not pending")
	}

	r := &recorder{q: d.DB}
	if _, err := usagePending(ctx, r, entOrg); err != nil {
		t.Fatal(err)
	}
	got := r.only(t)
	want := `SELECT "usage_outbox"."id" FROM "usage_outbox" WHERE "usage_outbox"."rejected_at" IS NULL AND "event"->>'organizationId' = $1 LIMIT 1`
	if got.query != want {
		t.Errorf("query\n got %s\nwant %s", got.query, want)
	}
	for _, q := range []string{got.query, `SELECT EXISTS (SELECT 1 FROM usage_outbox WHERE rejected_at IS NULL AND event ->> 'organizationId' = $1)`} {
		if plan := explain(t, d, q, entOrg); !strings.Contains(plan, "usage_outbox_pending_org_idx") {
			t.Errorf("%s isn't on usage_outbox_pending_org_idx:\n%s", q, plan)
		}
	}
}

// explain is the plan of query when a sequential scan is the planner's last resort.
func explain(t *testing.T, d *testdb.DB, query string, args ...any) string {
	t.Helper()
	var plan []string
	runTx(t, d, func(tx *db.Tx) error {
		if _, err := tx.ExecContext(context.Background(), `SET LOCAL enable_seqscan = off`); err != nil {
			return err
		}
		var err error
		plan, err = db.Strings(tx.QueryContext(context.Background(), "EXPLAIN "+query, args...))
		return err
	})
	return strings.Join(plan, "\n")
}

// The pending closures are the oldest 100 not sent, and sending one stamps only it.
func TestPendingClosuresAndClosureSent(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	exec(t, d, `INSERT INTO organization_closures (organization_id, complete_through, created_at)
		SELECT gen_random_uuid(), '2026-09-28T10:00:00Z', '2026-09-28T11:00:00Z'::timestamptz + make_interval(secs => i)
		FROM generate_series(1, 101) AS i`)
	exec(t, d, `INSERT INTO organization_closures (organization_id, complete_through, created_at, sent_at) VALUES ($1, now(), '2026-09-28T09:00:00Z', now())`, entOtherOrg)

	r := &recorder{q: d.DB}
	list, err := pendingClosures(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "organization_closures"."organization_id", "organization_closures"."complete_through", "organization_closures"."created_at", "organization_closures"."sent_at" FROM "organization_closures" WHERE "organization_closures"."sent_at" IS NULL ORDER BY "organization_closures"."created_at" LIMIT 100`
	if got := r.only(t).query; got != want {
		t.Errorf("query\n got %s\nwant %s", got, want)
	}
	if len(list) != 100 {
		t.Fatalf("%d pending", len(list))
	}
	var newest string
	_ = d.Pool.QueryRow(ctx, `SELECT organization_id::text FROM organization_closures ORDER BY created_at DESC LIMIT 1`).Scan(&newest)
	for i, c := range list {
		if c.ID == newest || c.ID == entOtherOrg || c.SentAt != nil {
			t.Fatalf("pending[%d] = %+v", i, c)
		}
		if i > 0 && !c.CreatedAt.After(list[i-1].CreatedAt) {
			t.Fatalf("not oldest first at %d", i)
		}
		if c.ID != strings.ToLower(c.ID) || len(c.ID) != 36 {
			t.Fatalf("id %q", c.ID)
		}
	}

	if err := closureSent(ctx, d.DB, list[0].ID); err != nil {
		t.Fatal(err)
	}
	var sent int
	var stamped sql.NullBool
	_ = d.Pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE sent_at IS NOT NULL), bool_and(sent_at IS NOT NULL) FILTER (WHERE organization_id = $1) FROM organization_closures`, list[0].ID).Scan(&sent, &stamped)
	if sent != 2 || !stamped.Bool {
		t.Errorf("%d sent; first stamped %v", sent, stamped)
	}
	if again, err := pendingClosures(ctx, d.DB); err != nil || len(again) != 100 || again[0].ID != list[1].ID {
		t.Errorf("after one sent: %d, %v", len(again), err)
	}
}
