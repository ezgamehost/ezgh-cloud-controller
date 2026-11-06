// Package schema is the kit's ent schema: a typed mirror of the kit's SQL migrations
// (kit/go/db/migrations), which stay the only source of the schema. Nothing here creates or
// changes a table; the drift test (internal/entdb) fails when the two differ. Regenerate with
// scripts/generate.sh.
package schema

import (
	"encoding/json"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
)

// outboxFields are audit_outbox's and usage_outbox's columns after the id.
func outboxFields() []ent.Field {
	return []ent.Field{
		// The event, a JSON object; written and relayed as its exact bytes.
		field.Bytes("event").GoType(json.RawMessage{}).SchemaType(db.PGJSONB),
		field.Time("created_at").Immutable().Annotations(entsql.DefaultExpr("now()")),
		field.Time("available_at").Annotations(entsql.DefaultExpr("now()")),
		field.Int32("attempts").Annotations(entsql.DefaultExpr("0")),
		field.String("last_error").SchemaType(db.PGText).Optional().Nillable(),
		field.Time("rejected_at").Optional().Nillable(),
	}
}

// AuditOutbox is audit_outbox: Trails events waiting for the relay.
type AuditOutbox struct{ ent.Schema }

func (AuditOutbox) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "audit_outbox"}}
}

func (AuditOutbox) Fields() []ent.Field {
	return append([]ent.Field{
		// The event's eventId.
		field.String("id").SchemaType(db.PGUUID).Immutable(),
	}, outboxFields()...)
}

func (AuditOutbox) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("available_at").StorageKey("audit_outbox_available_at_idx"),
	}
}

// UsageOutbox is usage_outbox: usage events waiting for the relay. The id is the event's
// deterministic ID.
type UsageOutbox struct{ ent.Schema }

func (UsageOutbox) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "usage_outbox"}}
}

func (UsageOutbox) Fields() []ent.Field {
	return append([]ent.Field{
		field.String("id").SchemaType(db.PGText).Immutable(),
	}, outboxFields()...)
}

// The expression index usage_outbox_pending_org_idx isn't modelled (entdb.AllowedDrift).
func (UsageOutbox) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("available_at").StorageKey("usage_outbox_available_at_idx"),
	}
}
