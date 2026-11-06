package decimal_test

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/decimal"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// contracts/vectors/decimals.json.
func TestDecimalVectors(t *testing.T) {
	var file struct {
		Canonical []struct {
			Input, Canonical string
		} `json:"canonical"`
		Invalid       []string `json:"invalid"`
		SecondsFromMs []struct {
			Ms      int64  `json:"ms"`
			Seconds string `json:"seconds"`
		} `json:"secondsFromMs"`
	}
	vectors.Load(t, "decimals.json", &file)
	for _, c := range file.Canonical {
		if got, ok := decimal.Canonical(c.Input); !ok || got != c.Canonical {
			t.Errorf("Canonical(%q) = %q, %v; want %q", c.Input, got, ok, c.Canonical)
		}
	}
	for _, s := range file.Invalid {
		if got, ok := decimal.Canonical(s); ok {
			t.Errorf("Canonical(%q) accepted it as %q", s, got)
		}
	}
	for _, c := range file.SecondsFromMs {
		if got := decimal.SecondsFromMillis(c.Ms).String(); got != c.Seconds {
			t.Errorf("SecondsFromMillis(%d) = %q, want %q", c.Ms, got, c.Seconds)
		}
	}
	if len(file.Canonical) == 0 || len(file.Invalid) == 0 || len(file.SecondsFromMs) == 0 {
		t.Fatal("vectors missing")
	}
}

func TestArithmetic(t *testing.T) {
	sum := decimal.MustParse("1062.75").Add(decimal.MustParse("3600")).Add(decimal.MustParse("-0.75"))
	if sum.String() != "4662" {
		t.Errorf("sum = %s", sum)
	}
	if decimal.MustParse("-5").Abs().Cmp(decimal.FromInt(5)) != 0 {
		t.Error("abs")
	}
}

// Capacity ratios (placement): products of a ratio and a count, floored once, never rounded up.
func TestMulIntFloorQuo(t *testing.T) {
	for _, c := range []struct {
		ratio string
		n     int64
		want  string
		floor int64
	}{
		{"4.0", 14, "56", 56},      // 4.0 × 2 threads × 7 cores
		{"0.333", 6, "1.998", 1},   // floors, never rounds up
		{"0.9", 256, "230.4", 230}, // 1 − 0.10 headroom × maxPrefixes
		{"0.5", 25000, "12500", 12500},
		{"-1.5", 1, "-1.5", -2}, // toward −∞
		{"0", 99, "0", 0},
	} {
		got := decimal.MustParse(c.ratio).MulInt(c.n)
		if got.String() != c.want {
			t.Errorf("%s × %d = %s, want %s", c.ratio, c.n, got, c.want)
		}
		if f, ok := got.Floor().Int64(); !ok || f != c.floor {
			t.Errorf("floor(%s × %d) = %d, %v; want %d", c.ratio, c.n, f, ok, c.floor)
		}
	}
	if _, ok := decimal.MustParse("1.5").Int64(); ok {
		t.Error("Int64 of a fraction")
	}
	if _, ok := decimal.MustParse("99999999999999999999").Int64(); ok {
		t.Error("Int64 out of range")
	}
	for _, c := range []struct {
		num, den int64
		want     string
	}{
		{1, 3, "0.333333333333"}, {2, 3, "0.666666666666"}, {6, 56, "0.107142857142"}, {5, 5, "1"}, {0, 7, "0"},
		{-1, 3, "-0.333333333333"},
	} {
		if got := decimal.Quo(c.num, c.den).String(); got != c.want {
			t.Errorf("Quo(%d, %d) = %s, want %s", c.num, c.den, got, c.want)
		}
	}
}
