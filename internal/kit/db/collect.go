package db

import "database/sql"

// Collect scans every row with scan and closes rows; it replaces pgx.CollectRows. The slice is
// never nil, so an empty list still encodes as [].
func Collect[T any](rows *sql.Rows, err error, scan func(*sql.Rows) (T, error)) ([]T, error) {
	out := []T{}
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return out, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	return out, rows.Close()
}

// Strings collects a single text column; it replaces pgx.RowTo[string].
func Strings(rows *sql.Rows, err error) ([]string, error) {
	return Collect(rows, err, func(r *sql.Rows) (string, error) {
		var s string
		return s, r.Scan(&s)
	})
}

// RowsAffected is a statement's affected-row count; it replaces CommandTag.RowsAffected.
func RowsAffected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
