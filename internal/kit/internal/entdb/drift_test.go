package entdb_test

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// The kit's ent schema matches its migrations exactly, every kit table is modelled or listed as
// raw, and every allowance still names something real.
func TestEntSchemaMatchesMigrations(t *testing.T) {
	d := testdb.New(t)
	testdb.AssertEntSchema(t, entdb.Client(d.DB).Schema.WriteTo, entdb.AllowedDrift...)
	testdb.AssertTablesCovered(t, d, entdb.Tables(), entdb.RawTables)
	testdb.AssertAllowlistLive(t, d, entdb.AllowedDrift...)
}
