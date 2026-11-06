package ids

import (
	"strings"
	"testing"
)

func TestNewV7(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := NewV7()
		if !IsUUID(id) || id[14] != '7' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("not a UUIDv7: %s", id)
		}
		if seen[id] {
			t.Fatalf("repeated %s", id)
		}
		seen[id] = true
	}
}

func TestIsUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70": true,
		"0199A3C2-5B1E-7D40-9F3A-2C8E6B1D4F70": true,
		"org_k3f9a0x2m7qp":                     false,
		"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f7":  false,
		"0199a3c2x5b1e-7d40-9f3a-2c8e6b1d4f70": false,
		"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4g70": false,
	} {
		if IsUUID(s) != want {
			t.Errorf("IsUUID(%q) = %v", s, !want)
		}
	}
}

func TestRandom(t *testing.T) {
	if v := Random(32); len(v) != 43 || strings.ContainsAny(v, "+/=") {
		t.Errorf("Random(32) = %q", v)
	}
}
