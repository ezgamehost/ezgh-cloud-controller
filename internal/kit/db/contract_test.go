package db_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/organizationclosure"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/predicate"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/servicecredential"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/usagewatermark"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/testent"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// The contract between the kit's data layer (database/sql over pgx, ent on top) and what the
// services rely on. Each test names the behaviour it pins.

var ctx = context.Background()

func closure(t *testing.T, q db.Driverer, org string, at time.Time) error {
	t.Helper()
	return entdb.Client(q).OrganizationClosure.Create().SetID(org).SetCompleteThrough(at).Exec(ctx)
}

func closures(t *testing.T, d *testdb.DB) []string {
	t.Helper()
	got, err := entdb.Client(d.DB).OrganizationClosure.Query().Order(organizationclosure.ByID()).IDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A uuid reads back as its canonical lowercase string, through ent and raw SQL alike.
func TestUUIDReadsBackCanonical(t *testing.T) {
	d := testdb.New(t)
	upper := "0199A3C2-5B1E-7D40-9F3A-2C8E6B1D4F70"
	if err := closure(t, d.DB, upper, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := entdb.Client(d.DB).OrganizationClosure.Query().Only(ctx)
	if err != nil || got.ID != strings.ToLower(upper) {
		t.Fatalf("ent read %v, %v", got, err)
	}
	var raw string
	if err := d.DB.QueryRowContext(ctx, `SELECT organization_id FROM organization_closures`).Scan(&raw); err != nil || raw != strings.ToLower(upper) {
		t.Fatalf("raw read %q, %v", raw, err)
	}
}

// timestamptz reads back in UTC, to the microsecond (so milliseconds are exact), whatever zone
// it was written in; DB defaults (now()) too.
func TestTimestamptzReadsBackUTC(t *testing.T) {
	d := testdb.New(t)
	written := time.Date(2026, 9, 28, 10, 30, 15, 123_456_000, time.FixedZone("UTC+5", 5*3600))
	org := ids.NewV7()
	if err := closure(t, d.DB, org, written); err != nil {
		t.Fatal(err)
	}
	got, err := entdb.Client(d.DB).OrganizationClosure.Get(ctx, org)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]time.Time{"complete_through": got.CompleteThrough, "created_at (now())": got.CreatedAt} {
		if v.Location() != time.UTC {
			t.Errorf("%s in %v, want UTC", name, v.Location())
		}
	}
	if !got.CompleteThrough.Equal(written) || got.CompleteThrough.Nanosecond() != 123_456_000 {
		t.Errorf("complete_through %v, want %v", got.CompleteThrough, written)
	}
	var raw time.Time
	if err := d.DB.QueryRowContext(ctx, `SELECT complete_through FROM organization_closures`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Location() != time.UTC || !raw.Equal(written) || raw.Format(time.RFC3339Nano) != "2026-09-28T05:30:15.123456Z" {
		t.Errorf("raw read %v", raw.Format(time.RFC3339Nano))
	}
}

// A json.RawMessage is stored as the JSON object it is (never a JSON string inside jsonb) and
// reads back as jsonb's text; a nil one is SQL NULL; an unset column gets its DB default.
func TestJSONBRawMessage(t *testing.T) {
	d := testdb.New(t)
	c := entdb.Client(d.DB)
	create := func() *ent.OperationCreate {
		return c.Operation.Create().SetID(ids.NewV7()).SetOrganizationID(ids.NewV7()).SetType("CreateWidget").
			SetAction("widgets.widgets.create").SetStatus("queued").SetTargetID(ids.NewV7()).SetTargetResourceName("x").
			SetTargetType("Widget").SetCreatedBy(json.RawMessage(`{"type":"user","id":"u1"}`))
	}
	unset, explicitNil := create(), create().SetError(nil)
	for _, op := range []*ent.OperationCreate{unset, explicitNil} {
		if err := op.Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	type stored struct {
		kind, createdBy, input string
		errorIsNull            bool
	}
	rows, err := d.DB.QueryContext(ctx, `SELECT jsonb_typeof(created_by), created_by::text, input::text, error IS NULL FROM operations`)
	got, err := db.Collect(rows, err, func(r *sql.Rows) (stored, error) {
		var s stored
		return s, r.Scan(&s.kind, &s.createdBy, &s.input, &s.errorIsNull)
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v, %v", got, err)
	}
	for _, r := range got {
		if r.kind != "object" || r.input != "{}" || !r.errorIsNull {
			t.Errorf("stored %+v", r)
		}
	}
	ops, err := c.Operation.Query().All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if string(op.CreatedBy) != got[0].createdBy || op.Error != nil || string(op.Input) != "{}" {
			t.Errorf("ent read created_by %s, input %s, error %v", op.CreatedBy, op.Input, op.Error)
		}
	}
}

// text[] and infinite timestamps, which database/sql has no type for, round-trip.
func TestTextArrayAndInfinity(t *testing.T) {
	d := testdb.New(t)
	in := db.TextArray{"a", `quo"te`, `back\slash`, "comma,{brace}", "", "NULL", " spaced ", "ünï"}
	var out db.TextArray
	if err := d.DB.QueryRowContext(ctx, `SELECT $1::text[]`, in).Scan(&out); err != nil || !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip %q, %v", out, err)
	}
	var literal db.TextArray
	if err := d.DB.QueryRowContext(ctx, `SELECT ARRAY['x', 'y"z']::text[]`).Scan(&literal); err != nil || !reflect.DeepEqual(literal, db.TextArray{"x", `y"z`}) {
		t.Fatalf("scan %q, %v", literal, err)
	}
	var null, empty bool
	if err := d.DB.QueryRowContext(ctx, `SELECT $1::text[] IS NULL, $2::text[] = '{}'`, db.TextArray(nil), db.TextArray{}).Scan(&null, &empty); err != nil || !null || !empty {
		t.Fatalf("nil is NULL %v, empty is '{}' %v (%v)", null, empty, err)
	}
	var fromNull, fromEmpty db.TextArray
	if err := d.DB.QueryRowContext(ctx, `SELECT NULL::text[], '{}'::text[]`).Scan(&fromNull, &fromEmpty); err != nil || fromNull != nil || fromEmpty == nil || len(fromEmpty) != 0 {
		t.Fatalf("NULL -> %#v, '{}' -> %#v (%v)", fromNull, fromEmpty, err)
	}

	for _, m := range []pgtype.InfinityModifier{pgtype.Infinity, pgtype.NegativeInfinity} {
		var back pgtype.Timestamptz
		if err := d.DB.QueryRowContext(ctx, `SELECT $1::timestamptz`, pgtype.Timestamptz{InfinityModifier: m, Valid: true}).Scan(&back); err != nil {
			t.Fatal(err)
		}
		if !back.Valid || back.InfinityModifier != m {
			t.Errorf("%v round-tripped as %+v", m, back)
		}
	}
	var finite pgtype.Timestamptz
	if err := d.DB.QueryRowContext(ctx, `SELECT '2026-09-28T10:00:00.5Z'::timestamptz`).Scan(&finite); err != nil || finite.InfinityModifier != pgtype.Finite ||
		!finite.Time.Equal(time.Date(2026, 9, 28, 10, 0, 0, 500_000_000, time.UTC)) {
		t.Fatalf("finite %+v, %v", finite, err)
	}
}

// OnConflict(...).DoNothing().ID(): a skipped row is sql.ErrNoRows, which db.Inserted reads as
// false; with ConflictWhere it arbitrates on a partial unique index. Exec on a skipped row is
// an error too, which is why DoNothing always goes through ID and Inserted.
func TestDoNothingID(t *testing.T) {
	d := testdb.New(t)
	c := entdb.Client(d.DB)
	inserted := func(_ string, err error) (bool, error) { return db.Inserted(err) }
	org := ids.NewV7()
	upsert := func() *ent.OrganizationClosureUpsertOne {
		return c.OrganizationClosure.Create().SetID(org).SetCompleteThrough(time.Now()).
			OnConflictColumns(organizationclosure.FieldID).DoNothing()
	}
	if ok, err := inserted(upsert().ID(ctx)); !ok || err != nil {
		t.Fatalf("first: %v %v", ok, err)
	}
	_, raw := upsert().ID(ctx)
	if ok, err := db.Inserted(raw); ok || err != nil || !errors.Is(raw, sql.ErrNoRows) {
		t.Fatalf("second: %v %v (%v)", ok, err, raw)
	}
	if err := upsert().Exec(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Exec on a skipped row: %v", err)
	}

	target := ids.NewV7()
	operation := func() *ent.OperationUpsertOne {
		return c.Operation.Create().SetID(ids.NewV7()).SetOrganizationID(org).SetType("CreateWidget").
			SetAction("widgets.widgets.create").SetStatus("queued").SetTargetID(target).SetTargetResourceName("x").
			SetTargetType("Widget").SetCreatedBy(json.RawMessage(`{}`)).
			OnConflict(entsql.ConflictColumns("target_id"), entsql.ConflictWhere(entsql.ExprP("status IN ('queued', 'running')"))).
			DoNothing()
	}
	if ok, err := inserted(operation().ID(ctx)); !ok || err != nil {
		t.Fatalf("first operation: %v %v", ok, err)
	}
	if ok, err := inserted(operation().ID(ctx)); ok || err != nil {
		t.Fatalf("second active operation: %v %v", ok, err)
	}
	if _, err := d.DB.ExecContext(ctx, `UPDATE operations SET status = 'succeeded', finished_at = now()`); err != nil {
		t.Fatal(err)
	}
	if ok, err := inserted(operation().ID(ctx)); !ok || err != nil {
		t.Fatalf("after the first finished: %v %v", ok, err)
	}
}

// The database's code and constraint name reach the caller through ent's ConstraintError.
func TestPgErrorThroughEnt(t *testing.T) {
	d := testdb.New(t)
	create := func() error {
		return entdb.Client(d.DB).ServiceCredential.Create().SetID("wk_3k9d0a1m2x7q4b8c").SetCaller("billing").
			SetSecretHash("h").SetExpiresAt(time.Now().Add(time.Hour)).SetCreatedBy("ops").Exec(ctx)
	}
	if err := create(); err != nil {
		t.Fatal(err)
	}
	err := create()
	if !ent.IsConstraintError(err) {
		t.Fatalf("not a ConstraintError: %v", err)
	}
	pg, ok := db.PgError(err)
	if !ok || pg.Code != "23505" || pg.ConstraintName != "service_credentials_pkey" {
		t.Fatalf("PgError %+v %v", pg, ok)
	}
	if !db.IsUnique(err, "service_credentials_pkey") || !db.IsUnique(err, "") || db.IsUnique(err, "other") {
		t.Error("IsUnique")
	}
}

// A database that can't be reached, or a closed pool, is Unavailable (503), not a bug.
func TestUnavailable(t *testing.T) {
	bad, err := db.Open(ctx, "postgres://postgres:postgres@127.0.0.1:1/none?connect_timeout=2", db.PoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Close()
	if err := bad.Ping(ctx); !db.Unavailable(err) {
		t.Errorf("ping: %v", err)
	}
	if err := bad.RunTx(ctx, nil, func(*db.Tx) error { return nil }); !db.Unavailable(err) {
		t.Errorf("RunTx: %v", err)
	}
	if _, err := bad.Pin(ctx); !db.Unavailable(err) {
		t.Errorf("Pin: %v", err)
	}

	d := testdb.New(t)
	closed, err := db.Open(ctx, d.URL, db.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	// DB.Close, the real API: database/sql is closed first, so every call fails with its "database
	// is closed".
	closed.Close()
	if err := closed.Ping(ctx); !db.Unavailable(err) {
		t.Errorf("Ping after Close: %v", err)
	}
	if err := closed.RunTx(ctx, nil, func(*db.Tx) error { return nil }); !db.Unavailable(err) {
		t.Errorf("RunTx after Close: %v", err)
	}
	if _, err := closed.Pin(ctx); !db.Unavailable(err) {
		t.Errorf("Pin after Close: %v", err)
	}
	if _, err := closed.ExecContext(ctx, `SELECT 1`); !db.Unavailable(err) {
		t.Errorf("Exec after Close: %v", err)
	}
	// Only the pool closed (database/sql still open): pgxpool's ErrClosedPool.
	pool, err := db.Open(ctx, d.URL, db.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	pool.Pool.Close()
	if err := pool.Ping(ctx); !db.Unavailable(err) {
		t.Errorf("closed pool: %v", err)
	}
	pool.Close()

	timedOut, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if err := d.DB.QueryRowContext(timedOut, `SELECT 1`).Scan(new(int)); !db.Unavailable(err) {
		t.Errorf("deadline: %v", err)
	}
	if db.Unavailable(errors.New("syntax error")) || db.Unavailable(sql.ErrNoRows) {
		t.Error("ordinary errors aren't Unavailable")
	}
}

// Savepoint undoes only its own work (a failed statement included) and a joined ent rollback
// inside it; the transaction carries on and commits the rest.
func TestSavepoint(t *testing.T) {
	d := testdb.New(t)
	a, b, c := "0199a3c2-0000-7000-8000-00000000000a", "0199a3c2-0000-7000-8000-00000000000b", "0199a3c2-0000-7000-8000-00000000000c"
	err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		if err := closure(t, tx, a, time.Now()); err != nil {
			return err
		}
		inner := errors.New("inner failed")
		err := tx.Savepoint(ctx, "inner_work", func() error {
			if err := closure(t, tx, b, time.Now()); err != nil {
				return err
			}
			// A joined ent rollback: UpdateOne on a missing row.
			if _, err := entdb.Client(tx).UsageWatermark.UpdateOneID("nowhere").SetCompleteThrough(time.Now()).Save(ctx); !ent.IsNotFound(err) {
				t.Errorf("UpdateOne on a missing row: %v", err)
			}
			return inner
		})
		if !errors.Is(err, inner) {
			t.Errorf("savepoint returned %v", err)
		}
		// A failed statement (a duplicate) inside a savepoint leaves the transaction usable.
		err = tx.Savepoint(ctx, "duplicate", func() error { return closure(t, tx, a, time.Now()) })
		if !db.IsUnique(err, "organization_closures_pkey") {
			t.Errorf("duplicate in savepoint: %v", err)
		}
		if err := tx.Savepoint(ctx, "kept", func() error { return closure(t, tx, c, time.Now()) }); err != nil {
			return err
		}
		if err := tx.Savepoint(ctx, "Bad-Name", func() error { return nil }); err == nil {
			t.Error("a savepoint name outside [a-z_] was accepted")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunTx: %v", err)
	}
	if got := closures(t, d); !reflect.DeepEqual(got, []string{a, c}) {
		t.Fatalf("committed %v, want %v", got, []string{a, c})
	}
}

// ent operations that open a "transaction" of their own join db.Tx: UpdateOne commits with the
// outer transaction; a failed edge write or an UpdateOne NotFound poisons it, so RunTx refuses
// to commit even when fn swallows the error; and beginning a transaction inside one is refused.
func TestTxDriver(t *testing.T) {
	d := testdb.New(t)
	region := "us-west-1"
	before := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	after := before.Add(time.Hour)
	if err := entdb.Client(d.DB).UsageWatermark.Create().SetID(region).SetCompleteThrough(before).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	read := func() time.Time {
		w, err := entdb.Client(d.DB).UsageWatermark.Get(ctx, region)
		if err != nil {
			t.Fatal(err)
		}
		return w.CompleteThrough
	}

	// UpdateOneID joins: rolled back with the outer transaction, committed with it.
	rollback := errors.New("roll back")
	err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		w, err := entdb.Client(tx).UsageWatermark.UpdateOneID(region).SetCompleteThrough(after).Save(ctx)
		if err != nil || !w.CompleteThrough.Equal(after) {
			t.Fatalf("UpdateOneID: %v %v", w, err)
		}
		var seen time.Time
		if err := tx.QueryRowContext(ctx, `SELECT complete_through FROM usage_watermarks WHERE region = $1`, region).Scan(&seen); err != nil || !seen.Equal(after) {
			t.Fatalf("raw SQL in the same transaction saw %v (%v)", seen, err)
		}
		return rollback
	})
	if !errors.Is(err, rollback) || !read().Equal(before) {
		t.Fatalf("rolled back: %v, %v", err, read())
	}
	if err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		_, err := entdb.Client(tx).UsageWatermark.UpdateOneID(region).SetCompleteThrough(after).Save(ctx)
		return err
	}); err != nil || !read().Equal(after) {
		t.Fatalf("committed: %v, %v", err, read())
	}

	// UpdateOne NotFound poisons the transaction; Update().Where().Save counts 0 and doesn't.
	err = d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		if _, err := entdb.Client(tx).UsageWatermark.UpdateOneID("nowhere").SetCompleteThrough(after).Save(ctx); !ent.IsNotFound(err) {
			t.Errorf("UpdateOne NotFound: %v", err)
		}
		return nil
	})
	if !errors.Is(err, db.ErrJoinedRollback) {
		t.Fatalf("UpdateOne NotFound: RunTx returned %v", err)
	}
	err = d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		n, err := entdb.Client(tx).UsageWatermark.Update().Where(usagewatermark.ID("nowhere")).SetCompleteThrough(after).Save(ctx)
		if n != 0 {
			t.Errorf("updated %d", n)
		}
		return err
	})
	if err != nil {
		t.Fatalf("Update().Where().Save: %v", err)
	}

	// A failed edge write (the join table doesn't exist) rolls back what ent wrote and poisons
	// the transaction.
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE contract_things (id uuid PRIMARY KEY, name text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	err = d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		err := testent.NewClient(testent.Driver(tx.Driver())).ContractThing.Create().SetID(ids.NewV7()).SetName("a").AddRelatedIDs(ids.NewV7()).Exec(ctx)
		if err == nil {
			t.Error("the edge write succeeded")
		}
		return nil // swallowed: RunTx must still refuse to commit
	})
	if !errors.Is(err, db.ErrJoinedRollback) {
		t.Fatalf("edge write failure: RunTx returned %v", err)
	}
	var things int
	if err := d.DB.QueryRowContext(ctx, `SELECT count(*) FROM contract_things`).Scan(&things); err != nil || things != 0 {
		t.Fatalf("contract_things: %d (%v)", things, err)
	}

	if err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		if _, err := entdb.Client(tx).BeginTx(ctx, nil); err == nil {
			t.Error("BeginTx inside db.Tx was allowed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// RunTx returns a commit-time error as the database raised it, so a deferred check's
// constraint name is still matched.
func TestRunTxCommitError(t *testing.T) {
	d := testdb.New(t)
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE contract_things (id uuid PRIMARY KEY, name text NOT NULL);
		CREATE FUNCTION contract_reject() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'rejected at commit' USING ERRCODE = '23514', CONSTRAINT = 'contract_things_checked'; END $$;
		CREATE CONSTRAINT TRIGGER contract_reject AFTER INSERT ON contract_things DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION contract_reject()`); err != nil {
		t.Fatal(err)
	}
	reached := false
	err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		if err := testent.NewClient(testent.Driver(tx.Driver())).ContractThing.Create().SetID(ids.NewV7()).SetName("a").Exec(ctx); err != nil {
			return err
		}
		reached = true
		return nil
	})
	pg, ok := db.PgError(err)
	if !reached || !ok || pg.Code != "23514" || pg.ConstraintName != "contract_things_checked" {
		t.Fatalf("commit error %v (%+v)", err, pg)
	}
}

// A session advisory lock dies with a discarded connection; a returned one would keep it.
func TestDiscardReleasesAdvisoryLocks(t *testing.T) {
	d := testdb.New(t)
	const key = 0x636f6e7472616374
	try := func(q db.Querier) bool {
		t.Helper()
		var locked bool
		if err := q.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		return locked
	}
	holder, err := d.DB.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	other, err := d.DB.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if !try(holder) {
		t.Fatal("the lock was taken")
	}
	if try(other) {
		t.Fatal("another connection got the lock")
	}
	// A pinned connection is a Querier too: ent and raw SQL on the same session.
	if n, err := entdb.Client(holder).UsageWatermark.Query().Count(ctx); err != nil || n != 0 {
		t.Fatalf("ent on a pinned connection: %d %v", n, err)
	}
	var pid int
	if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	holder.Discard(ctx)
	// Discard closes the socket; the server ends the backend, and with it the session's locks,
	// asynchronously a moment later (0-2 ms through apple/container's port forward), so trying
	// the lock straight away races the backend's exit. Wait, bounded, for that backend to be gone
	// from pg_stat_activity and pg_locks. A connection kept in the pool keeps its backend alive:
	// that leak fails here, as the timeout.
	for deadline := time.Now().Add(10 * time.Second); ; {
		var alive bool
		if err := other.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_stat_activity WHERE pid = $1)
			OR EXISTS (SELECT FROM pg_locks WHERE pid = $1)`, pid).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d (the discarded connection's) is still alive after 10s", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !try(other) {
		t.Fatal("the lock outlived its discarded connection")
	}
}

// Collect and Strings return [] for no rows, never nil (an empty list is [] in JSON).
func TestCollectEmpty(t *testing.T) {
	d := testdb.New(t)
	got, err := db.Strings(d.DB.QueryContext(ctx, `SELECT key_id FROM service_credentials`))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("Strings: %#v %v", got, err)
	}
	if b, _ := json.Marshal(got); string(b) != "[]" {
		t.Errorf("encoded %s", b)
	}
	if n, err := db.RowsAffected(d.DB.ExecContext(ctx, `DELETE FROM service_credentials`)); n != 0 || err != nil {
		t.Errorf("RowsAffected %d %v", n, err)
	}
}

// The kit's predicates bind their values as numbered arguments next to ent's own and qualify
// the column.
func TestPredicates(t *testing.T) {
	d := testdb.New(t)
	c := entdb.Client(d.DB)
	for _, k := range []struct{ id, caller string }{{"wk_aaaaaaaaaaaaaaaa", "Billing"}, {"wk_bbbbbbbbbbbbbbbb", "quotas"}} {
		if err := c.ServiceCredential.Create().SetID(k.id).SetCaller(k.caller).SetSecretHash("h").
			SetExpiresAt(time.Now().Add(time.Hour)).SetCreatedBy("ops").Exec(ctx); err != nil {
			t.Fatal(err)
		}
	}
	revoked := time.Now()
	if _, err := d.DB.ExecContext(ctx, `UPDATE service_credentials SET revoked_at = $1 WHERE key_id = 'wk_bbbbbbbbbbbbbbbb'`, revoked); err != nil {
		t.Fatal(err)
	}
	matched, err := c.ServiceCredential.Query().Where(
		servicecredential.CreatedBy("ops"),
		predicate.ServiceCredential(db.LowerEQ(servicecredential.FieldCaller, "BILLING")),
	).IDs(ctx)
	if err != nil || !reflect.DeepEqual(matched, []string{"wk_aaaaaaaaaaaaaaaa"}) {
		t.Fatalf("LowerEQ: %v %v", matched, err)
	}
	live, err := c.ServiceCredential.Query().Where(predicate.ServiceCredential(db.NotDistinctFrom(servicecredential.FieldRevokedAt, nil))).IDs(ctx)
	if err != nil || !reflect.DeepEqual(live, []string{"wk_aaaaaaaaaaaaaaaa"}) {
		t.Fatalf("NotDistinctFrom NULL: %v %v", live, err)
	}
	others, err := c.ServiceCredential.Query().Where(
		servicecredential.CreatedBy("ops"),
		predicate.ServiceCredential(db.DistinctFrom(servicecredential.FieldCaller, "quotas")),
	).IDs(ctx)
	if err != nil || !reflect.DeepEqual(others, []string{"wk_aaaaaaaaaaaaaaaa"}) {
		t.Fatalf("DistinctFrom: %v %v", others, err)
	}
}
