// Package ids makes and checks the identifiers every resource carries (contracts/resources.md):
// UUIDv7 IDs for URLs and slugs for resource names.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"sync"
	"time"
)

// IsUUID reports whether s is a UUID in lowercase 8-4-4-4-12 form, the only form the platform
// accepts in paths and bodies, so an ID has exactly one spelling in keys and filters.
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
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

var (
	mu     sync.Mutex
	lastMs int64
	seq    uint16
)

// NewV7 returns a UUIDv7 (RFC 9562): a millisecond timestamp, then a counter for IDs made in the
// same millisecond by this process (so they sort in creation order), then random bits.
func NewV7() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
	mu.Lock()
	ms := time.Now().UnixMilli()
	if ms <= lastMs {
		ms = lastMs
		seq++
		if seq > 0x0fff {
			ms++
			seq = 0
		}
	} else {
		seq = 0
	}
	lastMs = ms
	counter := seq
	mu.Unlock()

	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(b[0:6], ts[2:8])
	b[6] = 0x70 | byte(counter>>8)
	b[7] = byte(counter)
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

// slugAlphabet is what a slug's 12 characters are drawn from.
const slugAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var (
	prefixPattern = regexp.MustCompile(`^[a-z]{3}$`)
	slugPattern   = regexp.MustCompile(`^[a-z]{3}_[a-z0-9]{12}$`)
)

// IsSlug reports whether s is a slug: a 3-letter type prefix, "_", and 12 [a-z0-9].
func IsSlug(s string) bool { return slugPattern.MatchString(s) }

// NewSlug returns prefix + "_" + 12 characters from [a-z0-9], drawn without bias: bytes at or
// above the largest multiple of 36 are thrown away (rejection sampling, as org-api's newSlug
// does), so every character is equally likely. Slugs are unique by a unique index; callers retry
// with a new one on the (1 in 36¹²) collision.
func NewSlug(prefix string) string {
	if !prefixPattern.MatchString(prefix) {
		panic(fmt.Sprintf("ids: %q isn't a 3-letter slug prefix", prefix))
	}
	const limit = 256 - 256%len(slugAlphabet) // 252
	out := make([]byte, 0, 16)
	out = append(out, prefix...)
	out = append(out, '_')
	var buf [32]byte
	for len(out) < 16 {
		if _, err := rand.Read(buf[:]); err != nil {
			panic(err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, slugAlphabet[int(b)%len(slugAlphabet)])
			if len(out) == 16 {
				break
			}
		}
	}
	return string(out)
}
