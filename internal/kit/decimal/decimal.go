// Package decimal is the platform's exact decimal quantities (contracts/api-conventions.md):
// strings matching ^-?\d{1,26}(\.\d{1,12})?$, never floats, so a usage quantity or a control total
// is the same number in every service and language.
//
// A Decimal is an integer count of 10⁻¹² units. The canonical string has no leading zeros, no
// trailing fractional zeros and no "-0", so a regenerated event is byte-identical to the first.
package decimal

import (
	"math/big"
	"regexp"
	"strings"
)

// Scale is the number of fractional digits a decimal can carry.
const Scale = 12

var (
	pattern = regexp.MustCompile(`^-?\d{1,26}(\.\d{1,12})?$`)
	unit    = new(big.Int).Exp(big.NewInt(10), big.NewInt(Scale), nil)
)

// Decimal is an exact decimal. The zero value is 0.
type Decimal struct{ scaled big.Int }

// Valid reports whether s is a decimal string in the platform's form.
func Valid(s string) bool { return pattern.MatchString(s) }

// Parse reads a decimal string, or reports false for anything else (exponents, "+1", ".5", more
// than 26 integer or 12 fractional digits).
func Parse(s string) (Decimal, bool) {
	if !pattern.MatchString(s) {
		return Decimal{}, false
	}
	negative := strings.HasPrefix(s, "-")
	whole, fraction, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	var d Decimal
	d.scaled.SetString(whole+fraction+strings.Repeat("0", Scale-len(fraction)), 10)
	if negative {
		d.scaled.Neg(&d.scaled)
	}
	return d, true
}

// MustParse is Parse for constants.
func MustParse(s string) Decimal {
	d, ok := Parse(s)
	if !ok {
		panic("decimal: " + s + " isn't a decimal")
	}
	return d
}

// Canonical rewrites a decimal string canonically ("000.500" → "0.5", "-0.000" → "0").
func Canonical(s string) (string, bool) {
	d, ok := Parse(s)
	if !ok {
		return "", false
	}
	return d.String(), true
}

// FromInt is n as a decimal.
func FromInt(n int64) Decimal {
	var d Decimal
	d.scaled.Mul(big.NewInt(n), unit)
	return d
}

// SecondsFromMillis is ms milliseconds as seconds: 2537250 → 2537.25.
func SecondsFromMillis(ms int64) Decimal {
	var d Decimal
	d.scaled.Mul(big.NewInt(ms), new(big.Int).Exp(big.NewInt(10), big.NewInt(Scale-3), nil))
	return d
}

// Add returns d + o.
func (d Decimal) Add(o Decimal) Decimal {
	var out Decimal
	out.scaled.Add(&d.scaled, &o.scaled)
	return out
}

// Neg returns -d.
func (d Decimal) Neg() Decimal {
	var out Decimal
	out.scaled.Neg(&d.scaled)
	return out
}

// Abs returns |d|.
func (d Decimal) Abs() Decimal {
	var out Decimal
	out.scaled.Abs(&d.scaled)
	return out
}

// Cmp compares d and o: -1, 0 or 1.
func (d Decimal) Cmp(o Decimal) int { return d.scaled.Cmp(&o.scaled) }

// Sign is -1, 0 or 1.
func (d Decimal) Sign() int { return d.scaled.Sign() }

// String is the canonical form.
func (d Decimal) String() string {
	abs := new(big.Int).Abs(&d.scaled)
	whole, fraction := new(big.Int).QuoRem(abs, unit, new(big.Int))
	frac := fraction.String()
	frac = strings.Repeat("0", Scale-len(frac)) + frac
	frac = strings.TrimRight(frac, "0")
	out := whole.String()
	if frac != "" {
		out += "." + frac
	}
	if d.scaled.Sign() < 0 {
		out = "-" + out
	}
	return out
}

// MarshalText writes the canonical form, so a Decimal is a JSON string.
func (d Decimal) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// MulInt returns d × n, exactly.
func (d Decimal) MulInt(n int64) Decimal {
	var out Decimal
	out.scaled.Mul(&d.scaled, big.NewInt(n))
	return out
}

// Floor returns the largest integer at or below d (toward −∞: floor(-1.5) is -2).
func (d Decimal) Floor() Decimal {
	var whole big.Int
	whole.Div(&d.scaled, unit) // Euclidean: rounds toward −∞ for a positive divisor
	var out Decimal
	out.scaled.Mul(&whole, unit)
	return out
}

// Int64 is d as an int64, or false when d has a fraction or doesn't fit.
func (d Decimal) Int64() (int64, bool) {
	whole, fraction := new(big.Int).QuoRem(&d.scaled, unit, new(big.Int))
	if fraction.Sign() != 0 || !whole.IsInt64() {
		return 0, false
	}
	return whole.Int64(), true
}

// Quo is num / den to Scale fractional digits, truncated toward zero (2/3 → 0.666666666666).
// It panics when den is 0, as integer division does.
func Quo(num, den int64) Decimal {
	var out Decimal
	out.scaled.Quo(new(big.Int).Mul(big.NewInt(num), unit), big.NewInt(den))
	return out
}
