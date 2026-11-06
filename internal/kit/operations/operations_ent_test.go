package operations

// Tests for the operations package's ent statements: the SQL they send, and that what they read
// and write is what the raw SQL they replaced read and wrote.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/instant"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

const (
	entOrg   = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	otherOrg = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f71"
)

// recorder is an ent driver that keeps every statement it's given.
type recorder struct {
	dialect.Driver
	mu    sync.Mutex
	stmts []string
}

func (r *recorder) add(query string, args any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, fmt.Sprintf("%s %v", query, args))
}

func (r *recorder) Exec(ctx context.Context, query string, args, v any) error {
	r.add(query, args)
	return r.Driver.Exec(ctx, query, args, v)
}

func (r *recorder) Query(ctx context.Context, query string, args, v any) error {
	r.add(query, args)
	return r.Driver.Query(ctx, query, args, v)
}

func (r *recorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stmts
	r.stmts = nil
	return s
}

// recording is a pool or transaction, with its ent driver recording.
type recording struct {
	db.Querier
	r *recorder
}

func (q recording) Driver() dialect.Driver { return q.r }

func newOp(t *testing.T, d *db.DB, org, target string, input any) *Operation {
	t.Helper()
	var op *Operation
	err := d.RunTx(context.Background(), nil, func(tx *db.Tx) error {
		var err error
		project := "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f7f"
		op, err = Create(context.Background(), tx, New{
			OrganizationID: org, ProjectID: &project, Type: "CreateWidget", Action: "widgets.widgets.create",
			Target:    Target{ID: target, ResourceName: "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:wgt_" + target[len(target)-12:], Type: "Widget"},
			CreatedBy: authz.Principal{Type: "user", ID: "u1"}, Input: input,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// rawRead is the operation as the raw SQL the ent reads replaced read it (::text casts).
func rawRead(t *testing.T, d *db.DB, id string) *Operation {
	t.Helper()
	op, err := scan(d.QueryRowContext(context.Background(), `SELECT `+columns+` FROM operations WHERE id = $1`, id))
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func exec(t *testing.T, d *testdb.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// The statements, exactly: DB-clock expressions, literal status predicates the partial indexes
// match, bound (never '?') arguments and the list's millisecond keyset.
func TestEntStatements(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	rec := &recorder{Driver: d.DB.Driver()}
	q := recording{Querier: d.DB, r: rec}
	target := ids.NewV7()
	op := newOp(t, d.DB, entOrg, target, nil)
	cols := `"operations"."id", "operations"."organization_id", "operations"."project_id", "operations"."type", "operations"."action", "operations"."status", "operations"."target_id", "operations"."target_resource_name", "operations"."target_type", "operations"."created_by", "operations"."input", "operations"."error", "operations"."created_at", "operations"."updated_at", "operations"."started_at", "operations"."finished_at", "operations"."available_at", "operations"."lease_until", "operations"."attempts"`
	at := time.Date(2026, 9, 1, 12, 0, 0, 123_000_000, time.UTC)
	steps := []struct {
		name string
		run  func() error
		want string
	}{
		{"Get", func() error { _, err := Get(ctx, q, entOrg, op.ID); return err },
			`SELECT ` + cols + ` FROM "operations" WHERE ("operations"."organization_id" = $1 AND "operations"."id" = $2) AND ("operations"."finished_at" IS NULL OR "operations"."finished_at" > now() - make_interval(secs => $3)) LIMIT 2 [` + entOrg + ` ` + op.ID + ` 604800]`},
		{"Active", func() error { _, err := Active(ctx, q, target); return err },
			`SELECT ` + cols + ` FROM "operations" WHERE "operations"."target_id" = $1 AND "operations"."status" IN ('queued', 'running') LIMIT 2 [` + target + `]`},
		{"InProgress", func() error { return ignore409(InProgress(ctx, q, target)) },
			`SELECT "operations"."id" FROM "operations" WHERE "operations"."target_id" = $1 AND "operations"."status" IN ('queued', 'running') LIMIT 2 [` + target + `]`},
		{"Purge", func() error { _, err := Purge(ctx, q); return err },
			`DELETE FROM "operations" WHERE "operations"."finished_at" < now() - make_interval(secs => $1) [604800]`},
		{"renew", func() error { return renew(ctx, q, op.ID) },
			`UPDATE "operations" SET "lease_until" = now() + make_interval(secs => $1) WHERE "operations"."id" = $2 AND "operations"."status" = 'running' [30 ` + op.ID + `]`},
		{"backOff", func() error { return backOff(ctx, q, op.ID) },
			`UPDATE "operations" SET "lease_until" = NULL, "status" = $1, "updated_at" = now(), "available_at" = now() + least(interval '5 minutes', interval '5 seconds' * power(2, least(attempts - 1, 10))) WHERE "operations"."id" = $2 AND "operations"."status" = 'running' [queued ` + op.ID + `]`},
		{"list", func() error {
			_, err := listPage(ctx, q, entOrg, Failed, "ezgh:x", &keyset{at: at, id: op.ID}, 3)
			return err
		},
			`SELECT ` + cols + ` FROM "operations" WHERE ((("operations"."organization_id" = $1 AND ("operations"."finished_at" IS NULL OR "operations"."finished_at" > now() - make_interval(secs => $2))) AND "operations"."status" = $3) AND "operations"."target_resource_name" = $4) AND (date_trunc('milliseconds', "operations"."created_at"), "operations"."id") < ($5, $6::uuid) ORDER BY date_trunc('milliseconds', "operations"."created_at") DESC, "operations"."id" DESC LIMIT 3 [` + entOrg + ` 604800 failed ezgh:x ` + at.String() + ` ` + op.ID + `]`},
		{"reap scan", func() error {
			_, err := lapsedIDs(ctx, q)
			return err
		},
			`SELECT "operations"."id" FROM "operations" WHERE "operations"."status" = 'running' AND "operations"."lease_until" < now() LIMIT 100 []`},
	}
	for _, s := range steps {
		rec.take()
		if err := s.run(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		got := rec.take()
		if len(got) != 1 || got[0] != s.want {
			t.Errorf("%s sent\n%q\nwant\n%q", s.name, got, s.want)
		}
	}

	// The statements a transaction sends: the row locks (FOR UPDATE serialises a cancel and
	// Abandon against the worker's completion; the reaper's skips a held row), the requeue and
	// the organization purge.
	err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		rec := &recorder{Driver: tx.Driver()}
		q := recording{Querier: tx, r: rec}
		steps := []struct {
			name string
			run  func() error
			want string
		}{
			{"lockRunning", func() error { _, err := lockRunning(ctx, q, op.ID); return err },
				`SELECT ` + cols + ` FROM "operations" WHERE "operations"."id" = $1 AND "operations"."status" = 'running' LIMIT 2 FOR UPDATE [` + op.ID + `]`},
			{"lockActive (Abandon)", func() error { _, err := lockActive(ctx, q, target); return err },
				`SELECT ` + cols + ` FROM "operations" WHERE "operations"."target_id" = $1 AND "operations"."status" IN ('queued', 'running') LIMIT 2 FOR UPDATE [` + target + `]`},
			{"lockForCancel", func() error { _, err := lockForCancel(ctx, q, op.ID); return err },
				`SELECT ` + cols + ` FROM "operations" WHERE "operations"."id" = $1 LIMIT 2 FOR UPDATE [` + op.ID + `]`},
			{"lockLapsed (Reap)", func() error { _, err := lockLapsed(ctx, q, op.ID); return err },
				`SELECT ` + cols + ` FROM "operations" WHERE ("operations"."id" = $1 AND "operations"."status" = 'running') AND "operations"."lease_until" < now() LIMIT 2 FOR UPDATE SKIP LOCKED [` + op.ID + `]`},
			{"requeue", func() error { return requeue(ctx, q, op.ID) },
				`UPDATE "operations" SET "lease_until" = NULL, "status" = $1, "available_at" = now(), "updated_at" = now() WHERE "operations"."id" = $2 [queued ` + op.ID + `]`},
			{"purgeOrganization", func() error { return purgeOrganization(ctx, q, entOrg) },
				`DELETE FROM "operations" WHERE "operations"."organization_id" = $1 [` + entOrg + `]`},
		}
		for _, s := range steps {
			if err := s.run(); err != nil {
				return fmt.Errorf("%s: %w", s.name, err)
			}
			got := rec.take()
			if len(got) != 1 || got[0] != s.want {
				t.Errorf("%s sent\n%q\nwant\n%q", s.name, got, s.want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The completion lock, Abandon's lock and the cancel lock wait for a transaction that holds the
// operation: they serialise against one another rather than read it and race. (Abandon's lock is
// called directly: its finish UPDATE would wait for the row even without it.)
func TestEntLocksWaitForAHeldOperation(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	op := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	exec(t, d, `UPDATE operations SET status = 'running', attempts = 1, started_at = now(), lease_until = now() + interval '30 seconds' WHERE id = $1`, op.ID)
	locked, release, held := make(chan error, 1), make(chan struct{}), make(chan error, 1)
	go func() {
		held <- d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
			_, err := tx.ExecContext(ctx, `SELECT 1 FROM operations WHERE id = $1 FOR UPDATE`, op.ID)
			locked <- err
			<-release
			return err
		})
	}()
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		if err := <-held; err != nil {
			t.Error(err)
		}
	}()
	const wait = 300 * time.Millisecond
	for name, lock := range map[string]func(context.Context, *db.Tx) error{
		"lockRunning":          func(ctx context.Context, tx *db.Tx) error { _, err := lockRunning(ctx, tx, op.ID); return err },
		"lockActive (Abandon)": func(ctx context.Context, tx *db.Tx) error { _, err := lockActive(ctx, tx, op.TargetID); return err },
		"lockForCancel":        func(ctx context.Context, tx *db.Tx) error { _, err := lockForCancel(ctx, tx, op.ID); return err },
	} {
		start := time.Now() // before the deadline is set, so the deadline can't fire "early" against it
		short, cancel := context.WithTimeout(ctx, wait)
		err := d.DB.RunTx(short, nil, func(tx *db.Tx) error { return lock(short, tx) })
		cancel()
		if err == nil || time.Since(start) < wait {
			t.Errorf("%s didn't wait for the held operation: %v after %v", name, err, time.Since(start))
		}
	}
	if got := rawRead(t, d.DB, op.ID); got.Status != Running {
		t.Errorf("the held operation changed: %s", got.Status)
	}
}

func ignore409(err error) error {
	var e *httpapi.Error
	if errors.As(err, &e) && e.Status == 409 {
		return nil
	}
	return err
}

// Every ent read is the operation the raw ::text SQL read: the same jsonb bytes, the same UTC
// instants, and the same view JSON.
func TestEntReadsMatchRawSQL(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	inputs := []any{
		nil,
		map[string]any{"size": "small", "z": []any{3, 1.50, "é <&>"}, "a": map[string]any{"nested": true, "n": nil}},
		json.RawMessage(`{"b":  1e3, "a": "x",   "a": "dup"}`),
	}
	var all []string
	for _, in := range inputs {
		op := newOp(t, d.DB, entOrg, ids.NewV7(), in)
		all = append(all, op.ID)
	}
	// One failed with an error body, one running.
	failed := newOp(t, d.DB, entOrg, ids.NewV7(), map[string]int{"n": 1})
	cause := &httpapi.Error{Status: 503, Code: "capacity_unavailable", Message: "No capacity — try later", Extra: map[string]any{"region": "us-west-1"}}
	if err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error { return Abandon(ctx, tx, failed.TargetID, cause) }); err != nil {
		t.Fatal(err)
	}
	running := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	exec(t, d, `UPDATE operations SET status = 'running', attempts = 1, started_at = now(), lease_until = now() + interval '30 seconds' WHERE id = $1`, running.ID)
	all = append(all, failed.ID, running.ID)

	for _, id := range all {
		want := rawRead(t, d.DB, id)
		got, err := Get(ctx, d.DB, entOrg, id)
		if err != nil || got == nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ent read\n%+v\nraw read\n%+v", got, want)
		}
		if got.CreatedAt.Location() != time.UTC || got.UpdatedAt.Location() != time.UTC {
			t.Errorf("%s: not UTC", id)
		}
		gotJSON, _ := json.Marshal(got.View())
		wantJSON, _ := json.Marshal(want.View())
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("view\n%s\nwant\n%s", gotJSON, wantJSON)
		}
		var text string
		if err := d.DB.QueryRowContext(ctx, `SELECT input::text FROM operations WHERE id = $1`, id).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if string(got.Input) != text {
			t.Errorf("input %s, ::text %s", got.Input, text)
		}
	}
	if got, _ := Get(ctx, d.DB, entOrg, failed.ID); got.Error["code"] != "capacity_unavailable" || got.Error["region"] != "us-west-1" || got.FinishedAt == nil {
		t.Errorf("failed: %+v", got)
	}
	if got, _ := Get(ctx, d.DB, otherOrg, running.ID); got != nil {
		t.Error("another organization's operation was read")
	}

	// Active and the locks read the same operation.
	active, err := Active(ctx, d.DB, running.TargetID)
	if err != nil || !reflect.DeepEqual(active, rawRead(t, d.DB, running.ID)) {
		t.Errorf("active %+v %v", active, err)
	}
	if none, err := Active(ctx, d.DB, failed.TargetID); none != nil || err != nil {
		t.Errorf("a finished operation is active: %+v %v", none, err)
	}
	err = d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		locked, err := lockRunning(ctx, tx, running.ID)
		if err != nil || !reflect.DeepEqual(locked, rawRead(t, d.DB, running.ID)) {
			t.Errorf("lockRunning %+v %v", locked, err)
		}
		if queued, err := lockRunning(ctx, tx, all[0]); queued != nil || err != nil {
			t.Errorf("lockRunning a queued operation: %+v %v", queued, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// 409 operation_in_progress names the active operation, as before; Abandon fails only an active
// one and is nothing without one.
func TestEntInProgressAndAbandon(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	target := ids.NewV7()
	first := newOp(t, d.DB, entOrg, target, nil)
	var e *httpapi.Error
	if err := InProgress(ctx, d.DB, target); !errors.As(err, &e) || e.Code != "operation_in_progress" ||
		e.Extra["operationId"] != first.ID || e.Message != "Operation "+first.ID+" on this resource hasn't finished yet" {
		t.Fatalf("got %v", err)
	}
	if err := InProgress(ctx, d.DB, ids.NewV7()); !errors.As(err, &e) || e.Extra != nil ||
		e.Message != "Another operation on this resource hasn't finished yet" {
		t.Fatalf("no active operation: %v", err)
	}
	cause := &httpapi.Error{Status: 404, Code: "organization_deleted", Message: "gone"}
	abandon := func(target string) error {
		return d.DB.RunTx(ctx, nil, func(tx *db.Tx) error { return Abandon(ctx, tx, target, cause) })
	}
	if err := abandon(target); err != nil {
		t.Fatal(err)
	}
	got := rawRead(t, d.DB, first.ID)
	if got.Status != Failed || got.Error["code"] != "organization_deleted" || got.FinishedAt == nil {
		t.Errorf("abandoned: %+v", got)
	}
	if err := abandon(target); err != nil {
		t.Errorf("abandoning with nothing active: %v", err)
	}
	if after := rawRead(t, d.DB, first.ID); !reflect.DeepEqual(after, got) {
		t.Errorf("a finished operation changed: %+v", after)
	}
	// The target is free again.
	newOp(t, d.DB, entOrg, target, nil)
}

// Operations stay readable for Retention after they finish, then Purge deletes them; the edge is
// on the database's clock.
func TestEntRetention(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	fresh := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	stale := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	open := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	exec(t, d, `UPDATE operations SET status = 'succeeded', finished_at = now() - make_interval(secs => $2) WHERE id = $1`, fresh.ID, Retention.Seconds()-60)
	exec(t, d, `UPDATE operations SET status = 'succeeded', finished_at = now() - make_interval(secs => $2) WHERE id = $1`, stale.ID, Retention.Seconds()+60)
	if op, err := Get(ctx, d.DB, entOrg, fresh.ID); op == nil || err != nil {
		t.Errorf("fresh: %v %v", op, err)
	}
	if op, err := Get(ctx, d.DB, entOrg, stale.ID); op != nil || err != nil {
		t.Errorf("stale is readable: %v %v", op, err)
	}
	page, err := listPage(ctx, d.DB, entOrg, "", "", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := idsOf(page); !slices.Equal(got, []string{open.ID, fresh.ID}) {
		t.Errorf("list %v", got)
	}
	n, err := Purge(ctx, d.DB)
	if err != nil || n != 1 {
		t.Fatalf("purged %d %v", n, err)
	}
	var left []string
	rows, err := d.DB.QueryContext(ctx, `SELECT id::text FROM operations ORDER BY id`)
	if left, err = db.Strings(rows, err); err != nil {
		t.Fatal(err)
	}
	want := []string{fresh.ID, open.ID}
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Errorf("left %v, want %v", left, want)
	}
}

// PurgeOrganization deletes every operation of that organization, active or finished, and no
// other's, in the caller's transaction.
func TestEntPurgeOrganization(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	done := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	exec(t, d, `UPDATE operations SET status = 'cancelled', finished_at = now() WHERE id = $1`, done.ID)
	other := newOp(t, d.DB, otherOrg, ids.NewV7(), nil)
	count := func(org string) (n int) {
		if err := d.DB.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE organization_id = $1`, org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Rolled back with its transaction.
	boom := errors.New("boom")
	if err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		if err := PurgeOrganization(ctx, tx, entOrg); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) || count(entOrg) != 2 {
		t.Fatalf("rollback: %v, %d left", err, count(entOrg))
	}
	if err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error { return PurgeOrganization(ctx, tx, entOrg) }); err != nil {
		t.Fatal(err)
	}
	if count(entOrg) != 0 || count(otherOrg) != 1 {
		t.Errorf("left %d of the org's, %d of the other's", count(entOrg), count(otherOrg))
	}
	if op, _ := Get(ctx, d.DB, otherOrg, other.ID); op == nil {
		t.Error("the other organization's operation went")
	}
}

func idsOf(ops []*ent.Operation) []string {
	out := make([]string, 0, len(ops))
	for _, o := range ops {
		out = append(out, o.ID)
	}
	return out
}

// The list pages exactly as the raw SQL did: newest first by the millisecond, ties by id, with
// the cursor's millisecond createdAt, through status and target filters.
func TestEntListPagesLikeRawSQL(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	// Several share a millisecond (microseconds apart), so the keyset's truncation matters.
	offsets := []time.Duration{0, 100 * time.Microsecond, 400 * time.Microsecond, 999 * time.Microsecond,
		time.Millisecond, time.Millisecond + 500*time.Microsecond, 2 * time.Millisecond, time.Second}
	statuses := []string{Queued, Succeeded, Queued, Failed, Succeeded, Queued, Failed, Succeeded}
	for i, off := range offsets {
		target := ids.NewV7()
		op := newOp(t, d.DB, entOrg, target, nil)
		var finished, errBody any
		if statuses[i] != Queued {
			finished = time.Now().Add(-time.Minute)
		}
		if statuses[i] == Failed {
			errBody = `{"code":"x","message":"y"}`
		}
		name := "ezgh:t:a"
		if i%3 == 0 {
			name = "ezgh:t:b"
		}
		exec(t, d, `UPDATE operations SET created_at = $2, status = $3, finished_at = $4, error = $5::jsonb, target_resource_name = $6 WHERE id = $1`,
			op.ID, base.Add(off), statuses[i], finished, errBody, name)
	}
	newOp(t, d.DB, otherOrg, ids.NewV7(), nil)

	// raw is the list's statement as it was before ent.
	raw := func(status, target string, after *keyset, n int) []string {
		args := []any{entOrg, Retention.Seconds(), n}
		where := `organization_id = $1 AND (finished_at IS NULL OR finished_at > now() - make_interval(secs => $2))`
		if status != "" {
			args = append(args, status)
			where += fmt.Sprintf(" AND status = $%d", len(args))
		}
		if target != "" {
			args = append(args, target)
			where += fmt.Sprintf(" AND target_resource_name = $%d", len(args))
		}
		if after != nil {
			args = append(args, after.at, after.id)
			where += fmt.Sprintf(" AND (date_trunc('milliseconds', created_at), id) < ($%d, $%d::uuid)", len(args)-1, len(args))
		}
		out, err := db.Strings(d.DB.QueryContext(ctx, `SELECT id::text FROM operations WHERE `+where+
			` ORDER BY date_trunc('milliseconds', created_at) DESC, id DESC LIMIT $3`, args...))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, f := range []struct{ status, target string }{{"", ""}, {Queued, ""}, {"", "ezgh:t:a"}, {Succeeded, "ezgh:t:a"}, {Cancelled, ""}} {
		for _, size := range []int{1, 2, 3, 20} {
			var after *keyset
			var seen []string
			for pages := 0; ; pages++ {
				if pages > len(offsets)+1 {
					t.Fatalf("%+v/%d: the list doesn't end", f, size)
				}
				page, err := listPage(ctx, d.DB, entOrg, f.status, f.target, after, size+1)
				if err != nil {
					t.Fatal(err)
				}
				got := idsOf(page)
				if want := raw(f.status, f.target, after, size+1); !slices.Equal(got, want) {
					t.Fatalf("%+v/%d page %d: ent %v, raw %v", f, size, pages, got, want)
				}
				if len(page) <= size {
					seen = append(seen, got...)
					break
				}
				page = page[:size]
				seen = append(seen, idsOf(page)...)
				last, err := fromEnt(page[len(page)-1])
				if err != nil {
					t.Fatal(err)
				}
				// The cursor carries createdAt as the API shows it.
				at, ok := instant.ParseResponse(instant.Format(last.CreatedAt))
				if !ok {
					t.Fatal("cursor instant")
				}
				after = &keyset{at: at, id: last.ID}
			}
			if all := raw(f.status, f.target, nil, 100); !slices.Equal(seen, all) {
				t.Errorf("%+v/%d: paged %v, all %v", f, size, seen, all)
			}
		}
	}
	if page, err := listPage(ctx, d.DB, "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f72", "", "", nil, 5); err != nil || len(page) != 0 {
		t.Errorf("empty org: %v %v", page, err)
	}
}

// A parked operation (queued, started) lists under running, as its view shows it, not queued.
func TestEntListParkedAsRunning(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	parked := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	exec(t, d, `UPDATE operations SET started_at = now() WHERE id = $1`, parked.ID)
	waiting := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	for status, want := range map[string]string{Running: parked.ID, Queued: waiting.ID} {
		page, err := listPage(ctx, d.DB, entOrg, status, "", nil, 10)
		if err != nil {
			t.Fatal(err)
		}
		if got := idsOf(page); !slices.Equal(got, []string{want}) {
			t.Errorf("status=%s: %v, want %s", status, got, want)
		}
	}
}

// The worker's ent updates: the heartbeat's lease, the retry's backoff and the reaper's requeue,
// each on the database's clock and only for an operation in the state it expects.
func TestEntWorkerUpdates(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	op := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	type row struct {
		Status                string
		LeaseNull             bool
		LeaseAhead, DueAfter  float64 // lease_until - now(), available_at - updated_at, in seconds
		UpdatedNow, Available bool
	}
	read := func() (r row) {
		err := d.DB.QueryRowContext(ctx, `SELECT status, lease_until IS NULL,
			coalesce(extract(epoch FROM lease_until - now())::float8, 0), extract(epoch FROM available_at - updated_at)::float8,
			updated_at = now(), available_at <= now() FROM operations WHERE id = $1`, op.ID).
			Scan(&r.Status, &r.LeaseNull, &r.LeaseAhead, &r.DueAfter, &r.UpdatedNow, &r.Available)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Queued: the heartbeat and the backoff leave it be.
	if err := renew(ctx, d.DB, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := backOff(ctx, d.DB, op.ID); err != nil {
		t.Fatal(err)
	}
	if r := read(); r.Status != Queued || !r.LeaseNull {
		t.Fatalf("a queued operation changed: %+v", r)
	}
	run := func(attempts int) {
		exec(t, d, `UPDATE operations SET status = 'running', attempts = $2, started_at = now(), lease_until = now() - interval '1 second' WHERE id = $1`, op.ID, attempts)
	}
	run(1)
	if err := renew(ctx, d.DB, op.ID); err != nil {
		t.Fatal(err)
	}
	if r := read(); r.Status != Running || r.LeaseNull || r.LeaseAhead < Lease.Seconds()-5 || r.LeaseAhead > Lease.Seconds()+1 {
		t.Errorf("renewed: %+v", r)
	}
	for attempts, due := range map[int]float64{1: 5, 2: 10, 3: 20, 7: 300, 50: 300} {
		run(attempts)
		if err := backOff(ctx, d.DB, op.ID); err != nil {
			t.Fatal(err)
		}
		if r := read(); r.Status != Queued || !r.LeaseNull || r.DueAfter != due {
			t.Errorf("attempt %d backed off: %+v, want due after %v s", attempts, r, due)
		}
	}
	run(1)
	exec(t, d, `UPDATE operations SET updated_at = now() - interval '1 hour', available_at = now() + interval '1 hour' WHERE id = $1`, op.ID)
	if err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error { return requeue(ctx, tx, op.ID) }); err != nil {
		t.Fatal(err)
	}
	if r := read(); r.Status != Queued || !r.LeaseNull || r.DueAfter != 0 || !r.Available {
		t.Errorf("requeued: %+v", r)
	}
}

// Reaping skips an operation another transaction holds (SKIP LOCKED) rather than waiting for it.
func TestEntReapSkipsLockedOperations(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	op := newOp(t, d.DB, entOrg, ids.NewV7(), nil)
	exec(t, d, `UPDATE operations SET status = 'running', attempts = 1, started_at = now(), lease_until = now() - interval '1 second' WHERE id = $1`, op.ID)
	// Another transaction holds the row until release.
	locked, release, held := make(chan error, 1), make(chan struct{}), make(chan error, 1)
	go func() {
		held <- d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
			_, err := tx.ExecContext(ctx, `SELECT 1 FROM operations WHERE id = $1 FOR UPDATE`, op.ID)
			locked <- err
			<-release
			return err
		})
	}()
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
	released := false
	unlock := func() {
		if !released {
			released = true
			close(release)
			if err := <-held; err != nil {
				t.Fatal(err)
			}
		}
	}
	defer unlock()
	w := &Worker{DB: d.DB, Logger: testdb.Logger()}
	short, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := w.Reap(short); err != nil {
		t.Fatalf("reap waited or failed: %v", err)
	}
	if got := rawRead(t, d.DB, op.ID); got.Status != Running {
		t.Errorf("a locked operation was reaped: %s", got.Status)
	}
	unlock()
	if err := w.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rawRead(t, d.DB, op.ID); got.Status != Queued {
		t.Errorf("not reaped once free: %s", got.Status)
	}
}
