package manifest_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// contracts/vectors/manifest-s3.json: Parse accepts each case exactly when it's valid, and an invalid
// case's issue is among Check's (scripts/checks/s3-host.mjs runs the same cases).
func TestS3Vectors(t *testing.T) {
	var v registryVectors
	vectors.Load(t, "manifest-s3.json", &v)
	if len(v.Cases) < 10 {
		t.Fatalf("only %d cases", len(v.Cases))
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			data := c.applyTo(t, v.Manifest)
			if _, err := manifest.Parse(data); (err == nil) != c.Valid {
				t.Fatalf("valid=%t: %v", c.Valid, err)
			}
			if c.Valid {
				return
			}
			var m manifest.Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, i := range m.Check() {
				paths = append(paths, i.Path)
			}
			if !slices.Contains(paths, c.Issue) {
				t.Fatalf("issue %s not among %v", c.Issue, paths)
			}
		})
	}
}
