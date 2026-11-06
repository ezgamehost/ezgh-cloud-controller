package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// PoolOptions tune the pool Open builds. Zero values keep pgxpool's defaults.
type PoolOptions struct{ MinConns, MaxConns int32 }

// DB is a service's database: database/sql over a pgx pool. ent clients and raw SQL run on SQL
// (through Driver, the Exec/Query methods or RunTx); Pool is only for the few pgx-native
// exceptions (migrations, test fixtures, pgx.Batch).
type DB struct {
	SQL  *sql.DB
	Pool *pgxpool.Pool
	drv  dialect.Driver
}

// CancelDeadlineDelay is how long a statement whose context ended may take to answer the
// server-side cancel before the connection is given up (CancelHandler).
const CancelDeadlineDelay = 5 * time.Second

// CancelHandler answers a context ending mid-statement by asking the server to cancel the
// statement, and closes the connection only if that doesn't answer within CancelDeadlineDelay.
// pgx's default expires the socket's deadline at once, which closes the connection; over TLS its
// Terminate then doesn't reach the server and pgx drains the socket for up to 15 s, so the backend
// stays idle in its transaction, holding its locks and a pool slot, until the drain gives up.
func CancelHandler(c *pgconn.PgConn) ctxwatch.Handler {
	return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: CancelDeadlineDelay}
}

// ConfigurePool makes every connection of a pool scan timestamptz in UTC, through pgx and
// database/sql alike, and answer a context ending mid-statement with a server-side cancel
// (CancelHandler). Open calls it; so must anything that builds its own pool config (testdb).
func ConfigurePool(cfg *pgxpool.Config) {
	cfg.ConnConfig.BuildContextWatcherHandler = CancelHandler
	next := cfg.AfterConnect
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		c.TypeMap().RegisterType(&pgtype.Type{Name: "timestamptz", OID: pgtype.TimestamptzOID,
			Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC}})
		if next != nil {
			return next(ctx, c)
		}
		return nil
	}
}

// Open connects a pool to url and wraps it.
func Open(ctx context.Context, url string, o PoolOptions) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if o.MinConns > 0 {
		cfg.MinConns = o.MinConns
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return FromPool(pool), nil
}

// FromPool wraps an existing pool. OpenDBFromPool keeps no idle connections of its own, so the
// pool owns them; database/sql never opens more than the pool's MaxConns.
func FromPool(pool *pgxpool.Pool) *DB {
	s := stdlib.OpenDBFromPool(pool)
	s.SetMaxOpenConns(int(pool.Config().MaxConns))
	return &DB{SQL: s, Pool: pool, drv: noClose{entsql.OpenDB(dialect.Postgres, s)}}
}

// Driver is the ent driver over the pool, for entdb.Client. Closing a client doesn't close the
// pool.
func (d *DB) Driver() dialect.Driver { return d.drv }

// Ping checks the database answers.
func (d *DB) Ping(ctx context.Context) error { return d.SQL.PingContext(ctx) }

// Close closes database/sql and then the pool.
func (d *DB) Close() {
	_ = d.SQL.Close()
	d.Pool.Close()
}

// ExecContext runs a statement on the pool.
func (d *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return d.SQL.ExecContext(ctx, query, args...)
}

// QueryContext runs a query on the pool.
func (d *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.SQL.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a single-row query on the pool.
func (d *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return d.SQL.QueryRowContext(ctx, query, args...)
}

// RunTx runs fn in a transaction on the pool (see runTx).
func (d *DB) RunTx(ctx context.Context, opts *sql.TxOptions, fn func(*Tx) error) error {
	return runTx(ctx, d.SQL.BeginTx, opts, fn)
}

// noClose keeps an ent client's Close from closing the shared *sql.DB.
type noClose struct{ *entsql.Driver }

func (noClose) Close() error { return nil }

// Conn is one connection held from the pool, for session state such as an advisory lock.
type Conn struct{ c *sql.Conn }

// Pin holds one connection until Close (or Discard).
func (d *DB) Pin(ctx context.Context) (*Conn, error) {
	c, err := d.SQL.Conn(ctx)
	if err != nil {
		return nil, err
	}
	return &Conn{c: c}, nil
}

// ExecContext runs a statement on the held connection.
func (c *Conn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return c.c.ExecContext(ctx, query, args...)
}

// QueryContext runs a query on the held connection.
func (c *Conn) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return c.c.QueryContext(ctx, query, args...)
}

// QueryRowContext runs a single-row query on the held connection.
func (c *Conn) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return c.c.QueryRowContext(ctx, query, args...)
}

// Driver is an ent driver on the held connection.
func (c *Conn) Driver() dialect.Driver {
	return &connDriver{Driver: entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: c.c}), c: c.c}
}

// RunTx runs fn in a transaction on the held connection (see runTx).
func (c *Conn) RunTx(ctx context.Context, opts *sql.TxOptions, fn func(*Tx) error) error {
	return runTx(ctx, c.c.BeginTx, opts, fn)
}

// Discard destroys the physical connection, and with it any session state (advisory locks). A
// plain Close only returns a pool-backed connection to the pool, which keeps it open.
func (c *Conn) Discard(ctx context.Context) {
	_ = c.c.Raw(func(dc any) error {
		if sc, ok := dc.(*stdlib.Conn); ok {
			_ = sc.Conn().Close(ctx)
		}
		return driver.ErrBadConn
	})
	_ = c.c.Close()
}

// Close returns the connection to the pool.
func (c *Conn) Close() error { return c.c.Close() }

// connDriver is an ent driver on a held connection: its transactions begin on that connection.
type connDriver struct {
	*entsql.Driver
	c *sql.Conn
}

func (d *connDriver) Tx(ctx context.Context) (dialect.Tx, error) { return d.BeginTx(ctx, nil) }

func (d *connDriver) BeginTx(ctx context.Context, opts *entsql.TxOptions) (dialect.Tx, error) {
	tx, err := d.c.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &entsql.Tx{Conn: entsql.Conn{ExecQuerier: tx}, Tx: tx}, nil
}

func (d *connDriver) Close() error { return nil }
