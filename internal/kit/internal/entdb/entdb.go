// Package entdb builds the kit's ent client on a db handle, and lists what of the kit's schema
// ent deliberately doesn't model (the drift test's allowances).
package entdb

import (
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/migrate"
)

// Client is the kit's ent client on q: the pool (*db.DB), a held connection or a transaction
// (*db.Tx, shared with raw SQL and the product's own client). It's a small allocation with no
// I/O: build one per call, and never close or keep it.
func Client(q db.Driverer) *ent.Client { return ent.NewClient(ent.Driver(q.Driver())) }

// RawTables are the kit's tables ent doesn't model: composite primary keys, reached through raw
// SQL in their owning packages. (schema_migrations is the runner's, and testdb knows it.)
var RawTables = []string{
	"idempotency_keys",      // (organization_id, key); package service
	"idempotency_claims",    // (organization_id, key); package service
	"quota_usage",           // (quota_id, scope_key, region); package quota
	"usage_control_totals",  // (usage_hour, meter), numeric(38,12); package metering
	"usage_watermark_hours", // (region, complete_through); package metering
}

// AllowedDrift names schema objects (index or constraint names, or table.column) the drift
// test ignores, each because ent can't express it.
var AllowedDrift = []string{
	// An expression index, (event->>'organizationId') WHERE rejected_at IS NULL: ent models
	// column indexes only.
	"usage_outbox_pending_org_idx",
}

// Tables are the tables the kit's ent schema models.
func Tables() []string {
	out := make([]string, 0, len(migrate.Tables))
	for _, t := range migrate.Tables {
		out = append(out, t.Name)
	}
	return out
}
