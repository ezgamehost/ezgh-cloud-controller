package db

import (
	"database/sql/driver"
	"fmt"
	"strings"

	"entgo.io/ent/dialect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// QuoteIdent quotes a table or column name for SQL text, exactly as pgx.Identifier does. Only
// for names the code chose (a constant table), never for input.
func QuoteIdent(name string) string { return pgx.Identifier{name}.Sanitize() }

// Column types for ent schemas (field SchemaType), where ent's Postgres default differs from the
// migrations' DDL. The SQL migrations stay the schema; the drift test holds the two together.
var (
	PGText        = map[string]string{dialect.Postgres: "text"}
	PGUUID        = map[string]string{dialect.Postgres: "uuid"}
	PGJSONB       = map[string]string{dialect.Postgres: "jsonb"}
	PGTextArray   = map[string]string{dialect.Postgres: "text[]"}
	PGInteger     = map[string]string{dialect.Postgres: "integer"}
	PGTimestamptz = map[string]string{dialect.Postgres: "timestamp with time zone"}
)

// TextArray is a text[] column. database/sql has no array type, so it scans the array's text
// form with pgx's own parser, and writes an array literal. nil is SQL NULL, never '{}': set an
// empty TextArray where an empty array is meant.
type TextArray []string

// Scan implements sql.Scanner.
func (a *TextArray) Scan(src any) error {
	if src == nil {
		*a = nil
		return nil
	}
	var out []string
	if err := pgtype.NewMap().SQLScanner(&out).Scan(src); err != nil {
		return fmt.Errorf("db: scanning text[]: %w", err)
	}
	if out == nil {
		out = []string{}
	}
	*a = out
	return nil
}

// Value implements driver.Valuer: a text-array literal, every element quoted.
func (a TextArray) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range s {
			if r == '"' || r == '\\' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}
