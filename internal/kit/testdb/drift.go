package testdb

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	atlas "ariga.io/atlas/sql/schema"
	entschema "entgo.io/ent/dialect/sql/schema"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
)

// The drift test, one per ent generation root (internal/entdb/drift_test.go):
//
//	d := testdb.New(t, product.Migrations())
//	testdb.AssertEntSchema(t, entdb.Client(d.DB).Schema.WriteTo, entdb.AllowedDrift...)
//	testdb.AssertTablesCovered(t, d, entdb.Tables(), entdb.RawTables)
//	testdb.AssertAllowlistLive(t, d, entdb.AllowedDrift...)
//
// ent never changes the database: WriteTo only renders what it would do to make the database
// match the ent schema, and the test fails unless that's nothing.

// KitTables are every table the kit's migrations create: what its ent schema models, its raw
// tables and the migration runner's own. A product's coverage test allows them.
func KitTables() []string {
	return slices.Concat(entdb.Tables(), entdb.RawTables, []string{"schema_migrations"})
}

// AssertEntSchema fails t when the ent schema and the migrated database differ: any table,
// column, type, nullability, default, primary key or index (dropped ones included), except
// foreign keys and CHECKs, which aren't modelled, and the objects allow names (an index or
// constraint name, or table.column).
func AssertEntSchema(t testing.TB, writeTo func(context.Context, io.Writer, ...entschema.MigrateOption) error, allow ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var seen []string
	var buf bytes.Buffer
	err := writeTo(ctx, &buf,
		entschema.WithDropIndex(true),
		entschema.WithDropColumn(true),
		entschema.WithForeignKeys(false),
		entschema.WithDiffHook(describe(&seen), dropCheckChanges, dropNamed(allow)),
	)
	if err != nil {
		t.Fatalf("diffing the ent schema against the database: %v", err)
	}
	if buf.Len() > 0 {
		t.Errorf("the ent schema differs from the migrations (fix the ent schema, never the database):\n%s\nchanges: %s",
			buf.String(), strings.Join(seen, "; "))
	}
}

// dropCheckChanges drops CHECK changes: CHECKs stay in the SQL only.
func dropCheckChanges(next entschema.Differ) entschema.Differ {
	return filterTableChanges(next, func(_ *atlas.Table, c atlas.Change) bool {
		switch c.(type) {
		case *atlas.AddCheck, *atlas.DropCheck, *atlas.ModifyCheck:
			return false
		}
		return true
	})
}

// dropNamed drops changes to the allowed objects.
func dropNamed(allow []string) entschema.DiffHook {
	allowed := func(name string) bool { return name != "" && slices.Contains(allow, name) }
	column := func(t *atlas.Table, c *atlas.Column) bool { return c != nil && allowed(t.Name+"."+c.Name) }
	index := func(i *atlas.Index) bool { return i != nil && allowed(i.Name) }
	return func(next entschema.Differ) entschema.Differ {
		return filterTableChanges(next, func(t *atlas.Table, c atlas.Change) bool {
			switch c := c.(type) {
			case *atlas.AddIndex:
				return !index(c.I)
			case *atlas.DropIndex:
				return !index(c.I)
			case *atlas.ModifyIndex:
				return !index(c.From) && !index(c.To)
			case *atlas.AddColumn:
				return !column(t, c.C)
			case *atlas.DropColumn:
				return !column(t, c.C)
			case *atlas.ModifyColumn:
				return !column(t, c.From) && !column(t, c.To)
			}
			return true
		})
	}
}

// describe records what's left, for the failure message.
func describe(seen *[]string) entschema.DiffHook {
	return func(next entschema.Differ) entschema.Differ {
		return entschema.DiffFunc(func(current, desired *atlas.Schema) ([]atlas.Change, error) {
			changes, err := next.Diff(current, desired)
			for _, c := range changes {
				if m, ok := c.(*atlas.ModifyTable); ok {
					for _, sub := range m.Changes {
						*seen = append(*seen, fmt.Sprintf("%s: %T%s", m.T.Name, sub, detail(sub)))
					}
					continue
				}
				*seen = append(*seen, fmt.Sprintf("%T", c))
			}
			return changes, err
		})
	}
}

func detail(c atlas.Change) string {
	switch c := c.(type) {
	case *atlas.ModifyColumn:
		return fmt.Sprintf(" %s (kind %d)", c.From.Name, c.Change)
	case *atlas.ModifyIndex:
		return fmt.Sprintf(" %s (kind %d)", c.From.Name, c.Change)
	case *atlas.AddIndex:
		return " " + c.I.Name
	case *atlas.DropIndex:
		return " " + c.I.Name
	case *atlas.AddColumn:
		return " " + c.C.Name
	case *atlas.DropColumn:
		return " " + c.C.Name
	}
	return ""
}

// filterTableChanges keeps the table changes keep accepts, and drops table modifications left
// empty.
func filterTableChanges(next entschema.Differ, keep func(*atlas.Table, atlas.Change) bool) entschema.Differ {
	return entschema.DiffFunc(func(current, desired *atlas.Schema) ([]atlas.Change, error) {
		changes, err := next.Diff(current, desired)
		if err != nil {
			return nil, err
		}
		out := make([]atlas.Change, 0, len(changes))
		for _, c := range changes {
			m, ok := c.(*atlas.ModifyTable)
			if !ok {
				out = append(out, c)
				continue
			}
			kept := make([]atlas.Change, 0, len(m.Changes))
			for _, sub := range m.Changes {
				if keep(m.T, sub) {
					kept = append(kept, sub)
				}
			}
			if len(kept) > 0 {
				m.Changes = kept
				out = append(out, m)
			}
		}
		return out, nil
	})
}

// AssertTablesCovered fails t when the database has a table that's neither modelled (ent) nor
// listed as raw, nor one of the kit's (KitTables).
func AssertTablesCovered(t testing.TB, d *DB, modelled, raw []string) {
	t.Helper()
	known := slices.Concat(modelled, raw, KitTables())
	rows, err := d.Pool.Query(context.Background(),
		`SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' ORDER BY table_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(known, name) {
			t.Errorf("table %s is neither in the ent schema nor listed in RawTables", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// AssertAllowlistLive fails t when an allowed name doesn't exist in the database (an index or
// constraint, or a table.column), so the allowlist can't outlive what it excuses.
func AssertAllowlistLive(t testing.TB, d *DB, allow ...string) {
	t.Helper()
	for _, name := range allow {
		var exists bool
		var err error
		if table, column, ok := strings.Cut(name, "."); ok {
			err = d.Pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`, table, column).Scan(&exists)
		} else {
			err = d.Pool.QueryRow(context.Background(), `SELECT
				EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = current_schema() AND indexname = $1) OR
				EXISTS (SELECT 1 FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
					WHERE n.nspname = current_schema() AND c.conname = $1)`, name).Scan(&exists)
		}
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("AllowedDrift names %s, which the database doesn't have: remove it", name)
		}
	}
}
