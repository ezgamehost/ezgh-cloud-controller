package authz_test

import (
	"encoding/json"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// org-api's answers name a bot by its slug too; a user has none. Principals compare by type and ID.
func TestPrincipalSlug(t *testing.T) {
	var bot, user authz.BatchAnswer
	if err := json.Unmarshal(vectors.File(t, "examples/deployer.permissions-batch-response.json"), &bot); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(vectors.File(t, "examples/ada-session.permissions-batch-response.json"), &user); err != nil {
		t.Fatal(err)
	}
	if bot.Principal.Slug != "bot_d3p1oy3r0x7q" || user.Principal.Slug != "" {
		t.Fatalf("%+v %+v", bot.Principal, user.Principal)
	}
	asked := authz.Principal{Type: bot.Principal.Type, ID: bot.Principal.ID}
	if !bot.Principal.Same(asked) || bot.Principal.Same(user.Principal) {
		t.Fatal("Same")
	}
	// A request never sends a slug it wasn't given.
	if b, _ := json.Marshal(asked); string(b) != `{"type":"bot","id":"`+asked.ID+`"}` {
		t.Fatalf("%s", b)
	}
}
