package db

import entsql "entgo.io/ent/dialect/sql"

// Predicates ent doesn't generate in the form our SQL needs. Each qualifies col with the query's
// table and binds v as a numbered argument (sql.Expr would pass a literal '?' to Postgres).
// Use them as predicate.X(db.LowerEQ(x.FieldName, v)).

// LowerEQ is lower(col) = lower(v): the case-insensitive match the unique indexes use
// (generated EqualFold renders ILIKE, which is a different operator).
func LowerEQ(col string, v any) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("lower(").Ident(s.C(col)).WriteString(") = lower(").Arg(v).WriteByte(')')
		}))
	}
}

// DistinctFrom is col IS DISTINCT FROM v (NULL-safe <>).
func DistinctFrom(col string, v any) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(col)).WriteString(" IS DISTINCT FROM ").Arg(v)
		}))
	}
}

// NotDistinctFrom is col IS NOT DISTINCT FROM v (NULL-safe =).
func NotDistinctFrom(col string, v any) func(*entsql.Selector) {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(col)).WriteString(" IS NOT DISTINCT FROM ").Arg(v)
		}))
	}
}
