// Package canonical is canonical JSON and the hashes built on it (contracts/metering.md,
// "Deterministic IDs and hashing"): the bytes usage event IDs, idempotency fingerprints and
// registration digests are computed from, byte-identical to login-stack's service-kit and to the
// reference in scripts/lib/canonical.mjs.
//
// The rules: no whitespace; object keys sorted by Unicode code point, which for Go strings (UTF-8)
// is plain byte order; strings escaped exactly as JSON.stringify does, which is "\"", "\\" and
// U+0000–U+001F only, everything else (<, >, &, U+2028, U+2029, non-ASCII) written raw; numbers
// as JSON.stringify writes them. encoding/json can't be used as is: it escapes <, >, & unless told
// not to, and escapes U+2028 and U+2029 always, so strings are written here by hand.
package canonical

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Marshal returns the canonical JSON of v. v may be anything encoding/json can marshal; values
// other than the JSON primitives, map[string]any, []any and their string-typed cousins are
// marshaled first and read back, so struct tags are honoured.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := write(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// MustMarshal is Marshal for values known to be encodable (maps of strings, say).
func MustMarshal(v any) []byte {
	b, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// SHA256Hex is the hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Hash16 is the first 16 hex characters of the SHA-256 of v's canonical JSON: hash16({}) is
// 44136fa355b3678a. Usage event IDs carry it for dimensions and tags.
func Hash16(v any) (string, error) {
	b, err := Marshal(v)
	if err != nil {
		return "", err
	}
	return SHA256Hex(b)[:16], nil
}

// Hash16Strings is Hash16 for a string map, which always encodes; nil hashes as {}.
func Hash16Strings(m map[string]string) string {
	if m == nil {
		m = map[string]string{}
	}
	h, err := Hash16(m)
	if err != nil {
		panic(err) // a map of strings is always valid UTF-8 JSON unless a string isn't UTF-8
	}
	return h
}

func write(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		return writeString(buf, x)
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return fmt.Errorf("canonical: %q isn't a number", string(x))
		}
		return writeNumber(buf, f)
	case float64:
		return writeNumber(buf, x)
	case float32:
		return writeNumber(buf, float64(x))
	case int:
		return writeNumber(buf, float64(x))
	case int64:
		return writeNumber(buf, float64(x))
	case int32:
		return writeNumber(buf, float64(x))
	case uint64:
		return writeNumber(buf, float64(x))
	case map[string]any:
		return writeObject(buf, x)
	case map[string]string:
		m := make(map[string]any, len(x))
		for k, s := range x {
			m[k] = s
		}
		return writeObject(buf, m)
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := write(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []string:
		items := make([]any, len(x))
		for i, s := range x {
			items[i] = s
		}
		return write(buf, items)
	case json.RawMessage:
		decoded, err := Decode(x)
		if err != nil {
			return err
		}
		return write(buf, decoded)
	default:
		// Pointers to nothing are null, as encoding/json writes them.
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
			buf.WriteString("null")
			return nil
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		decoded, err := Decode(raw)
		if err != nil {
			return err
		}
		return write(buf, decoded)
	}
	return nil
}

// Decode reads JSON into the generic values Marshal works on, keeping numbers exact
// (json.Number).
func Decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("canonical: trailing data after the JSON value")
	}
	return v, nil
}

func writeObject(buf *bytes.Buffer, m map[string]any) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Go compares strings bytewise, and UTF-8 byte order is code point order.
	slices.Sort(keys)
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeString(buf, k); err != nil {
			return err
		}
		buf.WriteByte(':')
		if err := write(buf, m[k]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

const hexDigits = "0123456789abcdef"

// writeString escapes exactly what JSON.stringify escapes. Invalid UTF-8 is refused, as the
// reference refuses lone surrogates: such a string has no canonical form.
func writeString(buf *bytes.Buffer, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("canonical: %q isn't valid UTF-8", s)
	}
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			buf.WriteString(`\"`)
		case c == '\\':
			buf.WriteString(`\\`)
		case c == '\b':
			buf.WriteString(`\b`)
		case c == '\f':
			buf.WriteString(`\f`)
		case c == '\n':
			buf.WriteString(`\n`)
		case c == '\r':
			buf.WriteString(`\r`)
		case c == '\t':
			buf.WriteString(`\t`)
		case c < 0x20:
			buf.WriteString(`\u00`)
			buf.WriteByte(hexDigits[c>>4])
			buf.WriteByte(hexDigits[c&0xf])
		default:
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
	return nil
}

func writeNumber(buf *bytes.Buffer, f float64) error {
	s, err := JSNumber(f)
	if err != nil {
		return err
	}
	buf.WriteString(s)
	return nil
}

// JSNumber formats f as ECMAScript's Number.prototype.toString (and so JSON.stringify) does:
// the shortest digits that round-trip, in plain notation for magnitudes in [1e-6, 1e21) and
// exponent notation otherwise. -0 is "0".
func JSNumber(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errors.New("canonical: JSON has no NaN or Infinity")
	}
	if f == 0 {
		return "0", nil
	}
	if f < 0 {
		s, err := JSNumber(-f)
		return "-" + s, err
	}
	// Shortest round-trip digits and the decimal exponent: d.ddde±x.
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exp, _ := strings.Cut(e, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	x, _ := strconv.Atoi(exp)
	k := len(digits)
	n := x + 1 // the value is 0.digits × 10^n
	switch {
	case k <= n && n <= 21:
		return digits + strings.Repeat("0", n-k), nil
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:], nil
	case -6 < n && n <= 0:
		return "0." + strings.Repeat("0", -n) + digits, nil
	}
	sign := "+"
	if n-1 < 0 {
		sign = "-"
	}
	abs := n - 1
	if abs < 0 {
		abs = -abs
	}
	if k == 1 {
		return digits + "e" + sign + strconv.Itoa(abs), nil
	}
	return digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(abs), nil
}
