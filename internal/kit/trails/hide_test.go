package trails_test

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

// A sensitive field is hidden whatever the case of its key: JSON decoders fill fields
// case-insensitively, so "Email" must be as hidden as "email".
func TestHideSecretsIgnoresCase(t *testing.T) {
	got := trails.HideSecrets(map[string]any{"Registrant": map[string]any{"Email": "a@b.c"}, "EMAIL": "x", "name": "n"}, []string{"email"}).(map[string]any)
	if got["EMAIL"] != trails.Hidden || got["Registrant"].(map[string]any)["Email"] != trails.Hidden || got["name"] != "n" {
		t.Errorf("%v", got)
	}
}
