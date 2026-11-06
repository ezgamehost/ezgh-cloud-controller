package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
)

// IsNoRows reports a single-row query that found nothing (database/sql's or pgx's).
func IsNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, pgx.ErrNoRows)
}

// PgError is the database's error, through database/sql, ent's ConstraintError and wrapping.
func PgError(err error) (*pgconn.PgError, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr, true
	}
	return nil, false
}

// IsUnique reports a unique violation (23505), of constraint when it isn't "".
func IsUnique(err error, constraint string) bool {
	pgErr, ok := PgError(err)
	return ok && pgErr.Code == "23505" && (constraint == "" || pgErr.ConstraintName == constraint)
}

// IsContention reports a transaction the database aborted for contention, not for anything wrong
// with it: a deadlock (40P01) or a serialization failure (40001). Retrying the whole request later
// may succeed.
func IsContention(err error) bool {
	pgErr, ok := PgError(err)
	return ok && (pgErr.Code == "40P01" || pgErr.Code == "40001")
}

// Unavailable reports an error that means the database can't be reached right now (503), not a
// bug (500): it can't be connected to, the call ran out of time, the DB (DB.Close: database/sql
// first) or its pool is closed, or a failover got in the way: the connection dropped mid-query, the
// server is shutting down or starting, or what answered is a replica that hasn't been promoted.
func Unavailable(err error) bool {
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, errDBClosed) || errors.Is(err, puddle.ErrClosedPool) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, driver.ErrBadConn) {
		return true
	}
	pgErr, ok := PgError(err)
	if !ok {
		return false
	}
	switch pgErr.Code {
	case "57P01", "57P02", "57P03", // admin_shutdown, crash_shutdown, cannot_connect_now
		"25006": // read_only_sql_transaction: a primary that's been demoted, or a standby
		return true
	}
	return strings.HasPrefix(pgErr.Code, "08") // connection_exception
}

// errDBClosed is database/sql's (unexported) "sql: database is closed": what every call on a
// closed *sql.DB returns, taken from one.
var errDBClosed = func() error {
	d := sql.OpenDB(neverConnector{})
	_ = d.Close()
	return d.PingContext(context.Background())
}()

// neverConnector is a connector for a *sql.DB that is closed before it's used.
type neverConnector struct{}

func (neverConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("db: never connects")
}
func (neverConnector) Driver() driver.Driver { return nil }

// Inserted reads an upsert's DoNothing().ID() result: nil is a row inserted, sql.ErrNoRows a
// row skipped (the conflict), anything else the error it is.
func Inserted(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case IsNoRows(err):
		return false, nil
	default:
		return false, err
	}
}
