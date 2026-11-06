package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
)

// ServiceCredential is service_credentials: the credentials this service issues for its
// /internal routes. Only the secret's hash is stored.
type ServiceCredential struct{ ent.Schema }

func (ServiceCredential) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "service_credentials"}}
}

func (ServiceCredential) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("key_id").SchemaType(db.PGText).Immutable(),
		field.String("caller").SchemaType(db.PGText).Immutable(),
		field.String("secret_hash").SchemaType(db.PGText).Immutable().Sensitive(),
		field.Time("created_at").Immutable().Annotations(entsql.DefaultExpr("now()")),
		field.Time("expires_at").Immutable(),
		field.Time("revoked_at").Optional().Nillable(),
		field.Time("last_used_at").Optional().Nillable(),
		field.String("created_by").SchemaType(db.PGText).Immutable(),
		// The calling product's namespace, for delegated calls; NULL when it has none.
		field.String("namespace").SchemaType(db.PGText).Optional().Nillable().Immutable(),
	}
}
