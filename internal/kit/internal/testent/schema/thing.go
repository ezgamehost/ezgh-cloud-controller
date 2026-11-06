// Package schema is a second, test-only ent generation root: the kit's contract tests use its
// client next to the kit's own, on one transaction, as a product's client is. Its table is
// created by those tests, never by a migration.
package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
)

// ContractThing is contract_things, with a many-to-many edge to itself (contract_thing_related)
// so a test can make an ent edge write fail.
type ContractThing struct{ ent.Schema }

func (ContractThing) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "contract_things"}}
}

func (ContractThing) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").SchemaType(db.PGUUID).Immutable(),
		field.String("name").SchemaType(db.PGText),
	}
}

func (ContractThing) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("related", ContractThing.Type),
	}
}
