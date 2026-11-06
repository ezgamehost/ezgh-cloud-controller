// Package vectors finds the platform contracts' test vectors for the kit's tests: the files both
// kits must reproduce. In this repository they're contracts/vectors at its root; in a service that
// vendors the kit, sync-kit copies them to the kit's testdata/contracts/vectors.
package vectors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Load reads contracts/vectors/<name> into v.
func Load(t testing.TB, name string, v any) {
	t.Helper()
	data := File(t, filepath.Join("vectors", name))
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("vectors/%s: %v", name, err)
	}
}

// File reads contracts/<path>, walking up from the test's directory. A missing file fails the
// test: a vector that silently doesn't run is a vector that doesn't protect anything.
func File(t testing.TB, path string) []byte {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		for _, candidate := range []string{
			filepath.Join(dir, "contracts", path),
			filepath.Join(dir, "testdata", "contracts", path),
		} {
			if data, err := os.ReadFile(candidate); err == nil {
				return data
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("contracts/%s not found above the test's directory", path)
		}
		dir = parent
	}
}

// Raw is Load for vectors whose values must stay generic JSON (numbers kept exact).
func Raw(t testing.TB, name string) []byte {
	t.Helper()
	var raw json.RawMessage
	Load(t, name, &raw)
	return raw
}
