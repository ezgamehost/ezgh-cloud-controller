// Package db applies a service's schema: the kit's own tables (the outboxes, operations,
// idempotency keys, quota counters, service credentials) and then the product's, from embedded
// SQL files. It's the ledger's migration runner, generalized to several sets.
//
// Each file is <version>_<name>.sql, applied once, in order, in its own transaction, and recorded
// in schema_migrations under its set's name. An advisory lock serializes concurrent migrate runs
// (two replicas of a Job, say), so migrations are safe to run on every deploy.
package db

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed migrations/*.sql
var kitFiles embed.FS

// Set is one named sequence of migrations.
type Set struct {
	// Name keys the set's rows in schema_migrations ("kit", "widgets"). Never rename it.
	Name string
	// FS holds the set's *.sql files at its root.
	FS fs.FS
}

// Kit is the kit's own tables. Apply it before the product's set.
func Kit() Set {
	sub, err := fs.Sub(kitFiles, "migrations")
	if err != nil {
		panic(err)
	}
	return Set{Name: "kit", FS: sub}
}

// Serializes concurrent migrate runs against one database.
const lockKey = 0x6b69746d6967 // "kitmig"

// MigrationLockTimeout bounds how long one migration waits for a table lock (SET LOCAL
// lock_timeout). A migration that queues behind a long transaction would otherwise block every
// query after it on that table, an outage, until it got the lock; this way it fails, and can be
// retried, or rewritten as online DDL (CREATE INDEX CONCURRENTLY, ADD CONSTRAINT … NOT VALID then
// VALIDATE). Waiting for another migrate run's advisory lock isn't bounded.
var MigrationLockTimeout = 5 * time.Second

// ErrMigrationLockTimeout: a migration couldn't take a lock it needs within MigrationLockTimeout.
var ErrMigrationLockTimeout = errors.New("db: the migration waited too long for a table lock (lock_timeout)")

type migration struct {
	version int
	name    string
	sql     string
}

func (s Set) migrations() ([]migration, error) {
	entries, err := fs.ReadDir(s.FS, ".")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			return nil, fmt.Errorf("%s: migration %q isn't named <version>_<name>.sql", s.Name, name)
		}
		body, err := fs.ReadFile(s.FS, name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: version, name: strings.TrimSuffix(name, ".sql"), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i := 1; i < len(out); i++ {
		if out[i].version == out[i-1].version {
			return nil, fmt.Errorf("%s: two migrations have version %d", s.Name, out[i].version)
		}
	}
	return out, nil
}

// Migrate applies every pending migration of each set, in the order given.
func Migrate(ctx context.Context, conn *pgx.Conn, logger *slog.Logger, sets ...Set) error {
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		set_name   text NOT NULL,
		version    integer NOT NULL,
		name       text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now(),
		PRIMARY KEY (set_name, version)
	)`); err != nil && !isDuplicate(err) {
		// Two first runs racing on CREATE TABLE IF NOT EXISTS: the loser's error is harmless.
		return err
	}
	for _, set := range sets {
		all, err := set.migrations()
		if err != nil {
			return err
		}
		for _, m := range all {
			applied, err := apply(ctx, conn, set.Name, m)
			if err != nil {
				return fmt.Errorf("migration %s/%s: %w", set.Name, m.name, err)
			}
			if applied {
				logger.Info("migration applied", "set", set.Name, "version", m.version, "name", m.name)
			}
		}
		var newest int
		if err := conn.QueryRow(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations WHERE set_name = $1", set.Name).Scan(&newest); err != nil {
			return err
		}
		if len(all) > 0 && newest > all[len(all)-1].version {
			return fmt.Errorf("the database is at %s migration %d, newer than this build (%d)", set.Name, newest, all[len(all)-1].version)
		}
	}
	return nil
}

func apply(ctx context.Context, conn *pgx.Conn, set string, m migration) (bool, error) {
	applied := false
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE set_name = $1 AND version = $2)", set, m.version).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		// After the advisory lock: only the migration's own locks are bounded.
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", MigrationLockTimeout.Milliseconds())); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" { // lock_not_available
				return fmt.Errorf("%w (%s): another transaction holds a lock it needs; retry when it ends, or use online DDL: %v",
					ErrMigrationLockTimeout, MigrationLockTimeout, err)
			}
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (set_name, version, name) VALUES ($1, $2, $3)", set, m.version, m.name); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func isDuplicate(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "42P07")
}
