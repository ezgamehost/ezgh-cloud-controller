package db_test

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
)

// A failover is 503, whatever error it surfaces as, wrapped or not; anything else stays a 500.
func TestUnavailableCoversFailover(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	for _, err := range []error{
		pg("57P01"), pg("57P02"), pg("57P03"), pg("25006"), pg("08000"), pg("08006"), pg("08P01"),
		io.ErrUnexpectedEOF, driver.ErrBadConn,
	} {
		if !db.Unavailable(err) || !db.Unavailable(fmt.Errorf("query: %w", err)) {
			t.Errorf("%v isn't unavailable", err)
		}
	}
	for _, err := range []error{pg("23505"), pg("42P01"), pg("57014"), pg("40001"), errors.New("a bug")} {
		if db.Unavailable(err) {
			t.Errorf("%v is unavailable", err)
		}
	}
}
