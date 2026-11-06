package db_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// waitRolledBack waits, bounded, for database/sql's watcher to roll back tx after its context
// ended, so that Commit finds it done (sql.ErrTxDone) rather than racing the rollback.
func waitRolledBack(t *testing.T, tx *db.Tx) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		_, err := tx.ExecContext(context.Background(), `SELECT 1`)
		if errors.Is(err, sql.ErrTxDone) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the transaction wasn't rolled back after its context ended: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// A context that ends after fn's last statement pre-empts the commit: database/sql rolls back
// and Commit says only sql.ErrTxDone. RunTx (on the pool and on a held connection) returns that
// as the context's error too, so a cancelled commit is context.Canceled and one past its
// deadline is context.DeadlineExceeded (db.Unavailable: 503), as a statement's would be; and
// nothing is committed. (database/sql's rollback of a transaction whose context ended discards
// the connection, so a held connection is used once.)
func TestRunTxContextEndedBeforeCommit(t *testing.T) {
	d := testdb.New(t)
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE contract_things (id uuid PRIMARY KEY, name text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	type runTx func(context.Context, *sql.TxOptions, func(*db.Tx) error) error
	pinned := func(t *testing.T) runTx {
		held, err := d.DB.Pin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = held.Close() })
		return held.RunTx
	}
	for _, on := range []struct {
		name  string
		runTx func(*testing.T) runTx
	}{{"pool", func(*testing.T) runTx { return d.DB.RunTx }}, {"held connection", pinned}} {
		for _, end := range []struct {
			name        string
			want        error
			unavailable bool
		}{{"cancelled", context.Canceled, false}, {"deadline", context.DeadlineExceeded, true}} {
			t.Run(on.name+"/"+end.name, func(t *testing.T) {
				tctx, cancel := context.WithCancel(ctx)
				if end.want == context.DeadlineExceeded {
					tctx, cancel = context.WithTimeout(ctx, 500*time.Millisecond)
				}
				defer cancel()
				var stmtErr error
				err := on.runTx(t)(tctx, nil, func(tx *db.Tx) error {
					if _, err := tx.ExecContext(tctx, `INSERT INTO contract_things VALUES (gen_random_uuid(), 'a')`); err != nil {
						return err
					}
					if end.want == context.Canceled {
						cancel()
					}
					<-tctx.Done()
					waitRolledBack(t, tx)
					// What a statement says once the context has ended, to compare with.
					_, stmtErr = tx.ExecContext(tctx, `SELECT 1`)
					return nil
				})
				if !errors.Is(stmtErr, end.want) {
					t.Fatalf("a statement after the context ended: %v, want %v", stmtErr, end.want)
				}
				if !errors.Is(err, end.want) || !errors.Is(err, sql.ErrTxDone) {
					t.Fatalf("RunTx = %v, want %v and sql.ErrTxDone", err, end.want)
				}
				if db.Unavailable(err) != end.unavailable || db.Unavailable(stmtErr) != end.unavailable {
					t.Errorf("Unavailable: commit %v, statement %v, want %v", db.Unavailable(err), db.Unavailable(stmtErr), end.unavailable)
				}
				var n int
				if err := d.DB.QueryRowContext(ctx, `SELECT count(*) FROM contract_things`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("contract_things: %d rows (%v), want none committed", n, err)
				}
			})
		}
	}
}

// A commit the database refuses, with the context still live, comes back from RunTx as the
// database's error itself, not wrapped or joined (amendment G30): callers match its SQLSTATE and
// constraint name.
func TestRunTxCommitErrorIsUnwrapped(t *testing.T) {
	d := testdb.New(t)
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE contract_things (id uuid PRIMARY KEY, name text NOT NULL);
		CREATE FUNCTION contract_reject() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'rejected at commit' USING ERRCODE = '23514', CONSTRAINT = 'contract_things_checked'; END $$;
		CREATE CONSTRAINT TRIGGER contract_reject AFTER INSERT ON contract_things DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW EXECUTE FUNCTION contract_reject()`); err != nil {
		t.Fatal(err)
	}
	held, err := d.DB.Pin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	for name, runTx := range map[string]func(context.Context, *sql.TxOptions, func(*db.Tx) error) error{
		"pool": d.DB.RunTx, "held connection": held.RunTx,
	} {
		tctx, cancel := context.WithCancel(ctx)
		err := runTx(tctx, nil, func(tx *db.Tx) error {
			_, err := tx.ExecContext(tctx, `INSERT INTO contract_things VALUES (gen_random_uuid(), 'a')`)
			return err
		})
		cancel()
		pg, ok := err.(*pgconn.PgError)
		if !ok || pg.Code != "23514" || pg.ConstraintName != "contract_things_checked" {
			t.Errorf("%s: commit error %T %v, want the *pgconn.PgError itself", name, err, err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, sql.ErrTxDone) {
			t.Errorf("%s: commit error %v matches a cancellation", name, err)
		}
	}
}

// A statement whose context ends is cancelled on the server and the connection survives
// (CancelHandler): pgx's default closed it, which over TLS left the backend idle in its
// transaction, holding its locks, for up to 15 s (workflows-stack, 2026-10-20).
func TestCancelledStatementKeepsTheConnection(t *testing.T) {
	d := testdb.New(t)
	c, err := d.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Release()
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Exec(short, `SELECT pg_sleep(10)`); err == nil {
		t.Fatal("pg_sleep(10) finished inside a 100 ms context")
	}
	if took := time.Since(start); took > db.CancelDeadlineDelay {
		t.Fatalf("the cancelled statement took %s", took)
	}
	if c.Conn().IsClosed() {
		t.Fatal("the connection was closed: the context's end wasn't answered with a server-side cancel")
	}
	var one int
	if err := c.QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("the connection after the cancel: %d %v", one, err)
	}
}
