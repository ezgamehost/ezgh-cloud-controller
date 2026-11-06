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

// Operation is operations: long-running changes, run by the worker. status is CHECK-enumerated
// in the DDL, and the CHECKs aren't modelled.
type Operation struct{ ent.Schema }

func (Operation) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "operations"}}
}

func (Operation) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").SchemaType(db.PGUUID).Immutable(),
		field.String("organization_id").SchemaType(db.PGUUID).Immutable(),
		field.String("project_id").SchemaType(db.PGUUID).Optional().Nillable().Immutable(),
		field.String("type").SchemaType(db.PGText).Immutable(),
		field.String("action").SchemaType(db.PGText).Immutable(),
		field.String("status").SchemaType(db.PGText),
		field.String("target_id").SchemaType(db.PGUUID).Immutable(),
		field.String("target_resource_name").SchemaType(db.PGText).Immutable(),
		field.String("target_type").SchemaType(db.PGText).Immutable(),
		field.Bytes("created_by").GoType(json.RawMessage{}).SchemaType(db.PGJSONB).Immutable(),
		field.Bytes("input").GoType(json.RawMessage{}).SchemaType(db.PGJSONB).Immutable().
			Annotations(entsql.DefaultExpr("'{}'::jsonb")),
		field.Bytes("error").GoType(json.RawMessage{}).SchemaType(db.PGJSONB).Optional(),
		field.Time("created_at").Immutable().Annotations(entsql.DefaultExpr("now()")),
		field.Time("updated_at").Annotations(entsql.DefaultExpr("now()")),
		field.Time("started_at").Optional().Nillable(),
		field.Time("finished_at").Optional().Nillable(),
		field.Time("available_at").Annotations(entsql.DefaultExpr("now()")),
		field.Time("lease_until").Optional().Nillable(),
		field.Int32("attempts").Annotations(entsql.DefaultExpr("0")),
	}
}

// Partial-index predicates are written as Postgres normalizes the DDL's (pg_get_expr), which is
// what the drift test compares: status IN ('queued', 'running') reads back as = ANY (ARRAY[…]).
func (Operation) Indexes() []ent.Index {
	return []ent.Index{
		// One active operation per resource.
		index.Fields("target_id").Unique().StorageKey("operations_one_active_per_target_idx").
			Annotations(entsql.IndexWhere("status = ANY (ARRAY['queued'::text, 'running'::text])")),
		index.Fields("organization_id", "created_at", "id").StorageKey("operations_list_idx").
			Annotations(entsql.DescColumns("created_at", "id")),
		index.Fields("available_at").StorageKey("operations_queued_idx").
			Annotations(entsql.IndexWhere("status = 'queued'::text")),
		index.Fields("lease_until").StorageKey("operations_running_idx").
			Annotations(entsql.IndexWhere("status = 'running'::text")),
		index.Fields("finished_at").StorageKey("operations_finished_idx").
			Annotations(entsql.IndexWhere("finished_at IS NOT NULL")),
	}
}
