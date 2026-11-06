// Package credential is the platform's per-service bearer credentials (contracts/service-auth.md):
//
//	Authorization: Bearer <key_id>.<secret>
//	key_id  <issuer prefix>_<16 [a-z0-9]>     pk_3k9d0a1m2x7q4b8c
//	secret  43 base64url characters: 32 random bytes, unpadded
//
// A service sends credentials other services issued (pk_ to metering-ingest, rk_ to org-api's
// registration API) and checks the ones it issued itself (widgets: wk_) against its own database
// on every request, with no cache.
package credential

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/predicate"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/servicecredential"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
)

var header = regexp.MustCompile(`^Bearer (([a-z]{2,4})_[a-z0-9]{16})\.([A-Za-z0-9_-]{43})$`)

// Parsed is a credential's parts.
type Parsed struct {
	Prefix string
	KeyID  string
	Secret string
}

// Parse reads an Authorization header value. Anything that isn't exactly a service credential
// (another scheme, a customer's ezgh_ API key, padding, a short secret) reports false; the secret
// is hashed as sent and never decoded.
func Parse(authorization string) (Parsed, bool) {
	m := header.FindStringSubmatch(authorization)
	if m == nil {
		return Parsed{}, false
	}
	return Parsed{Prefix: m[2], KeyID: m[1], Secret: m[3]}, true
}

// Valid reports whether token (without "Bearer ") is <key_id>.<secret> with the given prefix;
// callers check their own credentials' shape at startup, so a typo fails loudly at once.
func Valid(token, prefix string) bool {
	p, ok := Parse("Bearer " + token)
	return ok && p.Prefix == prefix
}

// SecretHash is what a receiver stores: the hex SHA-256 of the secret's 43 ASCII characters.
func SecretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Matches compares a secret with a stored hash in constant time.
func Matches(secret, storedHash string) bool {
	given := sha256.Sum256([]byte(secret))
	stored, err := hex.DecodeString(storedHash)
	if err != nil || len(stored) != len(given) {
		// Still do the comparison, so a malformed row costs what a good one does.
		subtle.ConstantTimeCompare(given[:], given[:])
		return false
	}
	return subtle.ConstantTimeCompare(given[:], stored) == 1
}

// dummyHash is compared against when no key matches, so timing doesn't reveal which key IDs
// exist.
var dummyHash = SecretHash(strings.Repeat("A", 43))

// Store checks and issues the credentials this service accepts on /internal, in its
// service_credentials table (created by the kit's migrations).
type Store struct {
	DB *db.DB
	// Prefix is the service's issuer prefix, from its manifest (widgets: wk).
	Prefix string
}

// Verify returns the caller a header's credential belongs to, or an error for anything that
// doesn't check out; every failure is the same 401 to the caller. It asks the database every
// time: a revoked key stops working on the next request.
func (s *Store) Verify(ctx context.Context, authorization string) (string, error) {
	c, err := s.VerifyCaller(ctx, authorization)
	return c.Name, err
}

// Caller is who a verified credential belongs to.
type Caller struct {
	// Name is the free-text caller it was issued for (compute-api).
	Name string
	// Namespace is the calling product's namespace (compute) for a credential issued with one, or
	// "" (contracts/service-auth.md, "Delegated calls"): only a credential with a namespace may
	// call auth "delegated" endpoints.
	Namespace string
}

// VerifyCaller is Verify, naming the credential's namespace too.
func (s *Store) VerifyCaller(ctx context.Context, authorization string) (Caller, error) {
	p, ok := Parse(authorization)
	if !ok || p.Prefix != s.Prefix {
		Matches("", dummyHash)
		return Caller{}, ErrInvalid
	}
	// One statement: the expiry and revocation checks and the staleness of last_used_at all use
	// the database's clock.
	var found []struct {
		Caller     string  `sql:"caller"`
		Namespace  *string `sql:"namespace"`
		SecretHash string  `sql:"secret_hash"`
		Stale      bool    `sql:"stale"`
	}
	err := entdb.Client(s.DB).ServiceCredential.Query().
		Where(servicecredential.ID(p.KeyID), servicecredential.RevokedAtIsNil(), unexpired()).
		Modify(func(sel *entsql.Selector) {
			sel.Select(sel.C(servicecredential.FieldCaller), sel.C(servicecredential.FieldNamespace), sel.C(servicecredential.FieldSecretHash))
			sel.AppendSelectExprAs(entsql.Raw(staleSQL), "stale")
		}).
		Scan(ctx, &found)
	if err != nil {
		return Caller{}, err
	}
	if len(found) == 0 {
		Matches(p.Secret, dummyHash)
		return Caller{}, ErrInvalid
	}
	if !Matches(p.Secret, found[0].SecretHash) {
		return Caller{}, ErrInvalid
	}
	caller := Caller{Name: found[0].Caller}
	if found[0].Namespace != nil {
		caller.Namespace = *found[0].Namespace
	}
	if !found[0].Stale {
		// One round trip on the hot path: last_used_at is fresh.
		return caller, nil
	}
	// At most once a minute, in SQL, so no process keeps state about it.
	if _, err := entdb.Client(s.DB).ServiceCredential.Update().
		Where(servicecredential.ID(p.KeyID), stale()).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(servicecredential.FieldLastUsedAt, entsql.Expr("now()")) }).
		Save(ctx); err != nil {
		return Caller{}, err
	}
	return caller, nil
}

// staleSQL is true when a credential's last use was never recorded or is over a minute old, on
// the database's clock; Verify records a use at most that often.
const staleSQL = "(last_used_at IS NULL OR last_used_at < now() - interval '1 minute')"

func stale() predicate.ServiceCredential {
	return func(s *entsql.Selector) { s.Where(entsql.ExprP(staleSQL)) }
}

// unexpired is expires_at > now(), on the database's clock.
func unexpired() predicate.ServiceCredential {
	return func(s *entsql.Selector) { s.Where(entsql.ExprP("expires_at > now()")) }
}

// ErrInvalid is any credential that doesn't check out.
var ErrInvalid = errors.New("credential: invalid, revoked or expired")

// Issued is a new credential. Token is shown once; only its hash is kept.
type Issued struct {
	KeyID  string `json:"keyId"`
	Caller string `json:"caller"`
	// Namespace is the calling product's namespace, or "" (omitted) when it has none.
	Namespace string    `json:"namespace,omitempty"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// IssueRequest is what IssueFor issues.
type IssueRequest struct {
	// Caller is who the credential is for (compute-api).
	Caller string
	// Namespace, when set, binds the credential to the calling product's namespace (compute), so
	// it may call auth "delegated" endpoints. It matches ^[a-z]{2,32}$.
	Namespace string
	CreatedBy string
	// Days is the lifetime, 1-365.
	Days int
}

var namespacePattern = regexp.MustCompile(`^[a-z]{2,32}$`)

// Issue creates a credential for caller, valid for days, bound to no namespace.
func (s *Store) Issue(ctx context.Context, caller, createdBy string, days int) (Issued, error) {
	return s.IssueFor(ctx, IssueRequest{Caller: caller, CreatedBy: createdBy, Days: days})
}

// IssueFor creates a credential as r says. A bad lifetime or namespace is an error before
// anything is written.
func (s *Store) IssueFor(ctx context.Context, r IssueRequest) (Issued, error) {
	if r.Days < 1 || r.Days > 365 {
		return Issued{}, fmt.Errorf("credential: lifetime must be 1-365 days, got %d", r.Days)
	}
	var namespace *string
	if r.Namespace != "" {
		if !namespacePattern.MatchString(r.Namespace) {
			return Issued{}, fmt.Errorf("credential: namespace must match ^[a-z]{2,32}$, got %q", r.Namespace)
		}
		namespace = &r.Namespace
	}
	keyID := s.Prefix + "_" + randomKeyID()
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return Issued{}, err
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	var expires time.Time
	err := s.DB.QueryRowContext(ctx, `
		INSERT INTO service_credentials (key_id, caller, secret_hash, expires_at, created_by, namespace)
		VALUES ($1, $2, $3, now() + make_interval(days => $4), $5, $6)
		RETURNING expires_at`, keyID, r.Caller, SecretHash(secret), r.Days, r.CreatedBy, namespace).Scan(&expires)
	if err != nil {
		return Issued{}, err
	}
	return Issued{KeyID: keyID, Caller: r.Caller, Namespace: r.Namespace, Token: keyID + "." + secret, ExpiresAt: expires.UTC()}, nil
}

// Revoke revokes a live key; false if there's none with that ID.
func (s *Store) Revoke(ctx context.Context, keyID string) (bool, error) {
	n, err := entdb.Client(s.DB).ServiceCredential.Update().
		Where(servicecredential.ID(keyID), servicecredential.RevokedAtIsNil()).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(servicecredential.FieldRevokedAt, entsql.Expr("now()")) }).
		Save(ctx)
	return n > 0, err
}

// Credential is a credential as listed: everything but its secret's hash.
type Credential struct {
	KeyID      string
	Caller     string
	Namespace  string // "" when it has none
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time // nil while live
	LastUsedAt *time.Time // nil until first used
}

// List lists every credential this service has issued, live or not, ordered by caller and then
// creation time. The result is never nil.
func (s *Store) List(ctx context.Context) ([]Credential, error) {
	rows, err := entdb.Client(s.DB).ServiceCredential.Query().
		Select(
			servicecredential.FieldID, servicecredential.FieldCaller, servicecredential.FieldNamespace, servicecredential.FieldCreatedBy,
			servicecredential.FieldCreatedAt, servicecredential.FieldExpiresAt,
			servicecredential.FieldRevokedAt, servicecredential.FieldLastUsedAt,
		).
		Order(servicecredential.ByCaller(), servicecredential.ByCreatedAt()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Credential, 0, len(rows))
	for _, r := range rows {
		var namespace string
		if r.Namespace != nil {
			namespace = *r.Namespace
		}
		out = append(out, Credential{
			KeyID: r.ID, Caller: r.Caller, Namespace: namespace, CreatedBy: r.CreatedBy,
			CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
			RevokedAt: r.RevokedAt, LastUsedAt: r.LastUsedAt,
		})
	}
	return out, nil
}

func randomKeyID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	const limit = 256 - 256%len(alphabet)
	out := make([]byte, 0, 16)
	var buf [32]byte
	for len(out) < 16 {
		_, _ = rand.Read(buf[:])
		for _, b := range buf {
			if int(b) < limit && len(out) < 16 {
				out = append(out, alphabet[int(b)%len(alphabet)])
			}
		}
	}
	return string(out)
}
