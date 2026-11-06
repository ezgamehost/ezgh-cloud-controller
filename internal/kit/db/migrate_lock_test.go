package db_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// A migration that can't get a table lock fails after MigrationLockTimeout with a clear error,
// instead of queueing behind a long transaction (and every later query behind it).
func TestMigrationLockTimeout(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, `CREATE TABLE widgets (id int)`); err != nil {
		t.Fatal(err)
	}
	held, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Rollback(ctx) }()
	if _, err := held.Exec(ctx, `LOCK TABLE widgets IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	old := db.MigrationLockTimeout
	db.MigrationLockTimeout = 200 * time.Millisecond
	t.Cleanup(func() { db.MigrationLockTimeout = old })
	conn, err := pgx.Connect(ctx, d.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	set := db.Set{Name: "locktest", FS: fstest.MapFS{"0001_add.sql": {Data: []byte(`ALTER TABLE widgets ADD COLUMN name text`)}}}
	start := time.Now()
	err = db.Migrate(ctx, conn, slog.New(slog.DiscardHandler), set)
	if !errors.Is(err, db.ErrMigrationLockTimeout) || time.Since(start) > 5*time.Second {
		t.Fatalf("migrate under a held lock: %v after %v", err, time.Since(start))
	}
	_ = held.Rollback(ctx)
	if err := db.Migrate(ctx, conn, slog.New(slog.DiscardHandler), set); err != nil {
		t.Fatalf("migrate once the lock is free: %v", err)
	}
}
