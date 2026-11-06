// Package testdb gives each test its own migrated Postgres database, on the server named by
// DATABASE_TEST_URL (scripts/test.sh starts a throwaway one on apple/container; CI runs Postgres as
// a service container). Tests are skipped without it, except in CI, where it's an error: a test
// that silently doesn't run protects nothing.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
)

// DB is one test's database.
type DB struct {
	Name string
	URL  string
	// Pool is for raw fixtures and assertions; DB, on the same pool, is what kit and product
	// code take.
	Pool *pgxpool.Pool
	DB   *db.DB
}

// New creates a database for t, applies the kit's migrations and then sets, and drops it when
// the test ends.
func New(t testing.TB, sets ...db.Set) *DB {
	t.Helper()
	base := os.Getenv("DATABASE_TEST_URL")
	if base == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("DATABASE_TEST_URL isn't set")
		}
		t.Skip("DATABASE_TEST_URL isn't set; run scripts/test.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "kit_test_" + hex.EncodeToString(suffix)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connecting to DATABASE_TEST_URL: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	_ = admin.Close(ctx)

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("DATABASE_TEST_URL: %v", err)
	}
	u.Path = "/" + name
	d := &DB{Name: name, URL: u.String()}
	t.Cleanup(func() {
		// database/sql first, then the pool, then the database.
		if d.DB != nil {
			d.DB.Close()
		} else if d.Pool != nil {
			d.Pool.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("dropping %s: %v", name, err)
			return
		}
		defer func() { _ = admin.Close(ctx) }()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})

	conn, err := pgx.Connect(ctx, d.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, conn, Logger(), append([]db.Set{db.Kit()}, sets...)...); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	_ = conn.Close(ctx)

	cfg, err := pgxpool.ParseConfig(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	// CI's Postgres keeps its default max_connections (100), shared by parallel packages.
	cfg.MaxConns = 16
	// Timestamps scan in UTC, as in production (db.Open).
	db.ConfigurePool(cfg)
	if d.Pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	d.DB = db.FromPool(d.Pool)
	return d
}

// Logger discards logs, unless KIT_TEST_LOGS is set.
func Logger() *slog.Logger {
	if os.Getenv("KIT_TEST_LOGS") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
