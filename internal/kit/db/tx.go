package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync/atomic"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
)

// Driverer builds ent clients: *DB, *Tx and *Conn.
type Driverer interface {
	Driver() dialect.Driver
}

// Querier is what a pool, a held connection and a transaction all offer: raw SQL through
// database/sql, and an ent driver on the same handle.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	Driverer
}

var (
	_ Querier = (*DB)(nil)
	_ Querier = (*Conn)(nil)
	_ Querier = (*Tx)(nil)
)

// Tx is one transaction, shared by raw SQL and every ent client built on it (the kit's and the
// product's). It's opened and finished only by RunTx (and service.Transact): there's no Commit
// or Rollback to call.
type Tx struct {
	tx *sql.Tx
	// failed is set when an ent operation that joined this transaction rolled back: the outer
	// transaction must not commit what's left.
	failed atomic.Bool
}

// ExecContext runs a statement in the transaction.
func (t *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// QueryContext runs a query in the transaction.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a single-row query in the transaction.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// Driver is an ent driver inside the transaction. ent opens a "transaction" of its own for some
// operations (UpdateOne, edge writes); those join this one instead: their commit is a no-op, and
// their rollback marks the transaction failed so it can't commit.
func (t *Tx) Driver() dialect.Driver {
	return &txDriver{Driver: entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: t.tx}), t: t}
}

type txDriver struct {
	*entsql.Driver
	t *Tx
}

func (d *txDriver) Tx(context.Context) (dialect.Tx, error) { return joinedTx{d}, nil }

func (d *txDriver) BeginTx(context.Context, *entsql.TxOptions) (dialect.Tx, error) {
	return nil, errors.New("db: cannot begin a transaction inside db.Tx; use (*db.Tx).Savepoint")
}

func (d *txDriver) Close() error { return nil }

type joinedTx struct{ *txDriver }

// Commit is a no-op: RunTx (or service.Transact) commits the outer transaction.
func (joinedTx) Commit() error { return nil }

// Rollback poisons the outer transaction: it must never commit half of an ent operation.
func (j joinedTx) Rollback() error {
	j.t.failed.Store(true)
	return nil
}

// ErrJoinedRollback is RunTx's error when an ent operation rolled back inside the transaction
// and fn returned nil anyway.
var ErrJoinedRollback = errors.New("db: an ent operation rolled back inside this transaction")

// runTx begins a transaction and runs fn in it. An error from fn rolls back and is returned as
// is; so is a commit error (unwrapped, so callers can match the database's constraint name, e.g.
// a deferred check's), except that a commit the context's end pre-empted also matches ctx.Err()
// (context.Canceled or context.DeadlineExceeded). A joined ent rollback rolls back and returns
// ErrJoinedRollback. A panic rolls back and re-panics.
func runTx(ctx context.Context, begin func(context.Context, *sql.TxOptions) (*sql.Tx, error), opts *sql.TxOptions, fn func(*Tx) error) error {
	stx, err := begin(ctx, opts)
	if err != nil {
		return err
	}
	t := &Tx{tx: stx}
	done := false
	defer func() {
		if !done {
			_ = stx.Rollback()
		}
	}()
	if err := fn(t); err != nil {
		done = true
		_ = stx.Rollback()
		return err
	}
	done = true
	if t.failed.Load() {
		_ = stx.Rollback()
		return ErrJoinedRollback
	}
	if err := stx.Commit(); err != nil {
		// The context ended after fn's last statement: database/sql's watcher has already rolled
		// back, and Commit says only sql.ErrTxDone. Return the context's error with it, so callers
		// map it as they map any other cancellation or deadline (as pgx's Commit reported it).
		// Other commit errors (the database's, e.g. a deferred check's) stay unwrapped.
		if ctxErr := ctx.Err(); ctxErr != nil && errors.Is(err, sql.ErrTxDone) {
			return errors.Join(ctxErr, err)
		}
		return err
	}
	return nil
}

var savepointName = regexp.MustCompile(`^[a-z_]+$`)

// Savepoint runs fn under a savepoint: an error from fn (a unique violation the caller turns
// into a 409, say) undoes only fn's work, and the transaction carries on. A joined ent rollback
// inside fn is undone with it.
func (t *Tx) Savepoint(ctx context.Context, name string, fn func() error) error {
	if !savepointName.MatchString(name) {
		return fmt.Errorf("db: savepoint name %q must match %s", name, savepointName)
	}
	before := t.failed.Load()
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return err
	}
	if err := fn(); err != nil {
		if _, rerr := t.tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name); rerr != nil {
			return errors.Join(err, rerr)
		}
		if _, rerr := t.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name); rerr != nil {
			return errors.Join(err, rerr)
		}
		t.failed.Store(before)
		return err
	}
	_, err := t.tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name)
	return err
}
