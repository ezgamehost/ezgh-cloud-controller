// Package cursor signs and checks pagination cursors (contracts/api-conventions.md,
// "Pagination"), in audit-api's src/cursor.rs format:
//
//	base64url(payload) "." base64url(HMAC-SHA256(secret, payload))   (both unpadded)
//	payload = {"v":1,"org":"<organization id>","scope":"<scope>","key":<keyset position>}
//
// org binds a cursor to the organization in the path and scope to the list and its filters, so a
// cursor can never page past the organization check or into another list. Cursors are signed, not
// encrypted: never put anything in key the caller couldn't see.
package cursor

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

const (
	version   = 1
	maxLength = 4096
)

// Why a cursor was refused. Callers answer every one as 400 invalid_cursor; the reasons are for
// logs and tests.
var (
	ErrMalformed         = errors.New("malformed")
	ErrTampered          = errors.New("tampered")
	ErrVersion           = errors.New("version")
	ErrWrongOrganization = errors.New("wrong_organization")
	ErrWrongScope        = errors.New("wrong_scope")
)

var part = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Signer signs and checks cursors with one service's CURSOR_SECRET.
type Signer struct{ secret []byte }

// NewSigner keys cursors with the UTF-8 bytes of secret.
func NewSigner(secret string) *Signer { return &Signer{secret: []byte(secret)} }

type payload struct {
	V     int             `json:"v"`
	Org   string          `json:"org"`
	Scope string          `json:"scope"`
	Key   json.RawMessage `json:"key"`
}

func (s *Signer) mac(b []byte) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write(b)
	return m.Sum(nil)
}

// Encode signs key (any JSON value, e.g. [createdAt, id]) for organizationID and scope.
func (s *Signer) Encode(organizationID, scope string, key any) (string, error) {
	rawKey, err := compactJSON(key)
	if err != nil {
		return "", err
	}
	body, err := compactJSON(payload{V: version, Org: organizationID, Scope: scope, Key: rawKey})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString(body) + "." + enc.EncodeToString(s.mac(body)), nil
}

// Decode checks cursor against organizationID and scope and unmarshals its key into key.
func (s *Signer) Decode(organizationID, scope, cursor string, key any) error {
	if len(cursor) > maxLength {
		return ErrMalformed
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 || !part.MatchString(parts[0]) || !part.MatchString(parts[1]) {
		return ErrMalformed
	}
	enc := base64.RawURLEncoding
	body, err := enc.DecodeString(parts[0])
	if err != nil {
		return ErrMalformed
	}
	given, err := enc.DecodeString(parts[1])
	if err != nil {
		return ErrMalformed
	}
	if !hmac.Equal(given, s.mac(body)) {
		return ErrTampered
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return ErrMalformed
	}
	switch {
	case p.V != version:
		return ErrVersion
	case p.Org != organizationID:
		return ErrWrongOrganization
	case p.Scope != scope:
		return ErrWrongScope
	}
	if key != nil {
		if err := json.Unmarshal(p.Key, key); err != nil {
			return ErrMalformed
		}
	}
	return nil
}

// compactJSON is JSON.stringify's output: no whitespace and no HTML escaping, with struct fields
// in declaration order (the payload's key order is part of the format).
func compactJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
