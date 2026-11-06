package trails

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// The backstop is contracts/vectors/trails-recording.json's secretKeyPattern, exactly.
func TestSecretKeyIsTheContractPattern(t *testing.T) {
	var file struct {
		SecretKeyPattern string `json:"secretKeyPattern"`
	}
	vectors.Load(t, "trails-recording.json", &file)
	if got := secretKey.String(); got != "(?i)"+file.SecretKeyPattern {
		t.Errorf("secretKey %q, contract %q", got, file.SecretKeyPattern)
	}
}
