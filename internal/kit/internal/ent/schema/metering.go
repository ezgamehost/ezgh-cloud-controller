package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
)

// UsageWatermark is usage_watermarks: the last watermark sent per region.
type UsageWatermark struct{ ent.Schema }

func (UsageWatermark) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "usage_watermarks"}}
}

func (UsageWatermark) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("region").SchemaType(db.PGText).Immutable(),
		field.Time("complete_through"),
		field.Time("sent_at").Annotations(entsql.DefaultExpr("now()")),
	}
}

// OrganizationClosure is organization_closures: a deleted organization the producer is done
// with, pending until sent.
type OrganizationClosure struct{ ent.Schema }

func (OrganizationClosure) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "organization_closures"}}
}

func (OrganizationClosure) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("organization_id").SchemaType(db.PGUUID).Immutable(),
		field.Time("complete_through"),
		field.Time("created_at").Immutable().Annotations(entsql.DefaultExpr("now()")),
		field.Time("sent_at").Optional().Nillable(),
	}
}
