// Package ids makes the identifiers the CLI sends: UUIDv7 request IDs and idempotency keys, and
// the random strings OAuth needs (PKCE verifiers, state).
package ids

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// NewV7 returns a UUIDv7 (RFC 9562): a millisecond timestamp, then random bits. The platform
// uses UUIDv7 for request IDs, so the CLI's sort next to everyone else's in logs and Trails.
func NewV7() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(time.Now().UnixMilli()))
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | (b[6] & 0x0f)
	b[8] = 0x80 | (b[8] & 0x3f)
	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return string(out[:])
}

// IsUUID reports whether s is a UUID in 8-4-4-4-12 form, in either case. The CLI uses it to tell
// an ID from a slug or a name, so an ID never costs a lookup.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
				return false
			}
		}
	}
	return true
}

// Random returns n random bytes as unpadded base64url: 32 bytes make a 43-character PKCE
// verifier, the length RFC 7636 recommends.
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
