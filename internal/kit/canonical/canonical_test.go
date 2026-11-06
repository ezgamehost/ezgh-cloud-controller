package canonical_test

import (
	"encoding/json"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// contracts/vectors/usage-id.vectors.json, "canonicalJson": byte-identical to service-kit and to
// the independent Python implementation it was checked against.
func TestCanonicalJSONVectors(t *testing.T) {
	var file struct {
		CanonicalJSON []struct {
			Name      string          `json:"name"`
			Value     json.RawMessage `json:"value"`
			Canonical string          `json:"canonical"`
			SHA256    string          `json:"sha256"`
			Hash      string          `json:"hash"`
		} `json:"canonicalJson"`
	}
	vectors.Load(t, "usage-id.vectors.json", &file)
	if len(file.CanonicalJSON) == 0 {
		t.Fatal("no canonicalJson vectors")
	}
	for _, c := range file.CanonicalJSON {
		t.Run(c.Name, func(t *testing.T) {
			value, err := canonical.Decode(c.Value)
			if err != nil {
				t.Fatal(err)
			}
			got, err := canonical.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.Canonical {
				t.Fatalf("canonical\n got %s\nwant %s", got, c.Canonical)
			}
			if sum := canonical.SHA256Hex(got); sum != c.SHA256 {
				t.Errorf("sha256 %s, want %s", sum, c.SHA256)
			}
			if h, _ := canonical.Hash16(value); h != c.Hash {
				t.Errorf("hash16 %s, want %s", h, c.Hash)
			}
		})
	}
}

func TestHash16OfEmptyAndExampleTags(t *testing.T) {
	if h := canonical.Hash16Strings(nil); h != "44136fa355b3678a" {
		t.Errorf("hash16({}) = %s", h)
	}
	if h := canonical.Hash16Strings(map[string]string{"team": "platform", "env": "prod"}); h != "c784f4f83b7be259" {
		t.Errorf("hash16(tags) = %s", h)
	}
}

// Numbers as JSON.stringify writes them.
func TestJSNumber(t *testing.T) {
	// Variables, so the sum is float64 arithmetic rather than an exact constant.
	tenth, fifth := 0.1, 0.2
	for in, want := range map[float64]string{
		0: "0", 1.5: "1.5", -0.25: "-0.25", 3: "3", 1e21: "1e+21", 1e20: "100000000000000000000",
		1e-7: "1e-7", 1.5e-7: "1.5e-7", 0.000001: "0.000001", 123456789.125: "123456789.125",
		-1e-7: "-1e-7", 2.5e25: "2.5e+25", tenth + fifth: "0.30000000000000004",
	} {
		got, err := canonical.JSNumber(in)
		if err != nil || got != want {
			t.Errorf("JSNumber(%v) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// U+2028 and U+2029 stay raw (encoding/json would escape them), and <, >, & too.
func TestStringsAreNotHTMLEscaped(t *testing.T) {
	got := string(canonical.MustMarshal(map[string]any{"s": "<a>&  \u0001"}))
	if want := "{\"s\":\"<a>&  \\u0001\"}"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if _, err := canonical.Marshal("\xff"); err == nil {
		t.Error("invalid UTF-8 was encoded")
	}
}
