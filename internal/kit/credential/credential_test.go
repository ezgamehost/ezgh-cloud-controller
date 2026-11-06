package credential_test

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/credential"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// contracts/vectors/service-credentials.json.
func TestServiceCredentialVectors(t *testing.T) {
	var file struct {
		Valid []struct {
			Header         string `json:"header"`
			SecretBytesHex string `json:"secretBytesHex"`
			Expected       struct {
				Prefix, KeyID, Secret, SecretHash string
			} `json:"expected"`
		} `json:"valid"`
		Invalid []struct{ Name, Header string } `json:"invalid"`
	}
	vectors.Load(t, "service-credentials.json", &file)
	for _, c := range file.Valid {
		p, ok := credential.Parse(c.Header)
		e := c.Expected
		if !ok || p.Prefix != e.Prefix || p.KeyID != e.KeyID || p.Secret != e.Secret {
			t.Errorf("parsed %s as %+v, %v", c.Header, p, ok)
		}
		if got := credential.SecretHash(p.Secret); got != e.SecretHash {
			t.Errorf("hash %s, want %s", got, e.SecretHash)
		}
		if !credential.Matches(p.Secret, e.SecretHash) || credential.Matches(p.Secret+"x", e.SecretHash) {
			t.Error("Matches")
		}
		raw, _ := base64.RawURLEncoding.DecodeString(e.Secret)
		if hex.EncodeToString(raw) != c.SecretBytesHex {
			t.Error("secret bytes")
		}
	}
	for _, c := range file.Invalid {
		if _, ok := credential.Parse(c.Header); ok {
			t.Errorf("%s: parsed", c.Name)
		}
	}
	if !credential.Valid("pk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", "pk") || credential.Valid("pk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", "rk") {
		t.Error("Valid")
	}
}
