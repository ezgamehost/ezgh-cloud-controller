package quota

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// contracts/vectors/quotas.json units: quota_exceeded messages name every unit the platform's
// quota definitions use in its proper plural ("addresses", not "addresss"; N8), and leave units
// that aren't words as they are ("iops", not "iopses"; "reqps", not "reqpses").
func TestUnitsVectors(t *testing.T) {
	var file struct {
		Units []struct {
			Unit string `json:"unit"`
			N    int64  `json:"n"`
			Text string `json:"text"`
		} `json:"units"`
	}
	vectors.Load(t, "quotas.json", &file)
	if len(file.Units) == 0 {
		t.Fatal("quotas.json has no units cases")
	}
	for _, c := range file.Units {
		if got := units(c.Unit, c.N); got != c.Text {
			t.Errorf("units(%q, %d) = %q, want %q", c.Unit, c.N, got, c.Text)
		}
	}
}
