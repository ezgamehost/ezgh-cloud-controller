// Package delegation is the kit's half of delegated calls (contracts/service-auth.md "Delegated
// calls", contracts/identity-and-authz.md "Delegated checks"): the X-Ezgh-Caller reference an
// edge builds from its live, verified batch answer and sends to a receiver beside the
// receiver-issued service credential, the client that sends it, and the mapping of a receiver's
// answer to the edge's customer. The receiver's half (auth "delegated" endpoints) is in package
// service. A reference is built only from a live request and never stored: asynchronous work acts
// with the service's own identity.
package delegation

import (
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
)

// Header is the caller reference's header.
const Header = "X-Ezgh-Caller"

// Limits of the grammar.
const (
	MaxBytes = 512
	MaxChain = 2
)

// The principal types and credential kinds a reference names.
var (
	PrincipalTypes  = []string{"user", "bot", "ssoUser"}
	CredentialKinds = []string{"session", "federation", "oauth", "apiKey", "ssoOauth"}
)

// Reference is a caller reference: the customer's organization, principal and credential (by ID,
// never its secret), when the edge accepted their request, and the namespaces that relayed it, the
// edge first.
type Reference struct {
	Org            string
	PrincipalType  string
	PrincipalID    string
	CredentialKind string
	CredentialID   string
	At             time.Time
	Chain          []string
}

// The parser's errors, in the order it checks; their text is contracts/vectors/
// caller-reference.json's message, the issue a receiver answers at X-Ezgh-Caller.
var (
	ErrLength     = errors.New("must be at most 512 bytes")
	ErrASCII      = errors.New("must be printable ASCII with no whitespace")
	ErrKeys       = errors.New("must be v, org, principal, credential, at and chain, each once, in that order")
	ErrVersion    = errors.New("v must be 1")
	ErrOrg        = errors.New("org must be an ID: 1-64 letters, digits, _ or -")
	ErrPrincipal  = errors.New("principal must be user, bot or ssoUser, a colon and an ID")
	ErrCredential = errors.New("credential must be session, federation, oauth, apiKey or ssoOauth, a colon and an ID")
	ErrAt         = errors.New("at must be Unix seconds")
	ErrChain      = errors.New("chain must be one or two distinct namespaces")
	ErrSecret     = errors.New("IDs must not look like credentials")
	ErrMissing    = errors.New("is required")
	ErrRepeated   = errors.New("must be exactly one header")
)

// ErrNotDelegable is FromAnswer's error for an answer that names no credential: an edge then
// makes no delegated call and answers 503.
var ErrNotDelegable = errors.New("delegation: org-api's answer names no delegable credential")

var (
	keys          = []string{"v", "org", "principal", "credential", "at", "chain"}
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	nsPattern     = regexp.MustCompile(`^[a-z]{2,32}$`)
	atPattern     = regexp.MustCompile(`^[1-9][0-9]{0,11}$`)
	serviceKeyID  = regexp.MustCompile(`^[a-z]{2,4}_[a-z0-9]{16}$`)
	printableNoWS = regexp.MustCompile(`^[\x21-\x7e]+$`)
)

// secretShaped: every customer credential starts with ezgh_, and a service credential's key_id is
// <prefix>_<16>; neither is ever an ID.
func secretShaped(id string) bool {
	return strings.HasPrefix(id, "ezgh_") || serviceKeyID.MatchString(id)
}

func typed(value string, types []string) (string, string, bool) {
	t, id, found := strings.Cut(value, ":")
	if !found || !slices.Contains(types, t) || !idPattern.MatchString(id) {
		return "", "", false
	}
	return t, id, true
}

// Parse reads one X-Ezgh-Caller value strictly: canonical or an error, never repaired.
func Parse(header string) (Reference, error) {
	if len(header) > MaxBytes {
		return Reference{}, ErrLength
	}
	if !printableNoWS.MatchString(header) {
		return Reference{}, ErrASCII
	}
	parts := strings.Split(header, ";")
	if len(parts) != len(keys) {
		return Reference{}, ErrKeys
	}
	values := make([]string, len(keys))
	for i, part := range parts {
		v, ok := strings.CutPrefix(part, keys[i]+"=")
		if !ok {
			return Reference{}, ErrKeys
		}
		values[i] = v
	}
	var r Reference
	if values[0] != "1" {
		return Reference{}, ErrVersion
	}
	if r.Org = values[1]; !idPattern.MatchString(r.Org) {
		return Reference{}, ErrOrg
	}
	var ok bool
	if r.PrincipalType, r.PrincipalID, ok = typed(values[2], PrincipalTypes); !ok {
		return Reference{}, ErrPrincipal
	}
	if r.CredentialKind, r.CredentialID, ok = typed(values[3], CredentialKinds); !ok {
		return Reference{}, ErrCredential
	}
	if !atPattern.MatchString(values[4]) {
		return Reference{}, ErrAt
	}
	at, err := strconv.ParseInt(values[4], 10, 64)
	if err != nil {
		return Reference{}, ErrAt
	}
	r.At = time.Unix(at, 0).UTC()
	r.Chain = strings.Split(values[5], ",")
	if len(r.Chain) > MaxChain || slices.ContainsFunc(r.Chain, func(n string) bool { return !nsPattern.MatchString(n) }) || (len(r.Chain) == 2 && r.Chain[0] == r.Chain[1]) {
		return Reference{}, ErrChain
	}
	if secretShaped(r.Org) || secretShaped(r.PrincipalID) || secretShaped(r.CredentialID) {
		return Reference{}, ErrSecret
	}
	return r, nil
}

// FromHeaders reads a request's X-Ezgh-Caller values: exactly one, parsed. A receiver answers any
// error 400 invalid_caller_reference (httpapi.InvalidCallerReference(err.Error())).
func FromHeaders(values []string) (Reference, error) {
	switch len(values) {
	case 0:
		return Reference{}, ErrMissing
	case 1:
		return Parse(values[0])
	default:
		return Reference{}, ErrRepeated
	}
}

// format is the canonical header, unchecked.
func (r Reference) format() string {
	return "v=1;org=" + r.Org + ";principal=" + r.PrincipalType + ":" + r.PrincipalID +
		";credential=" + r.CredentialKind + ":" + r.CredentialID + ";at=" + strconv.FormatInt(r.At.Unix(), 10) +
		";chain=" + strings.Join(r.Chain, ",")
}

// String is the canonical header, byte for byte as contracts/vectors/caller-reference.json pins
// it. A reference that wouldn't parse back (built by hand, badly) formats as "": never send it.
func (r Reference) String() string {
	s := r.format()
	if _, err := Parse(s); err != nil {
		return ""
	}
	return s
}

// Valid is nil when the reference formats to a header that parses back, or why not.
func (r Reference) Valid() error {
	_, err := Parse(r.format())
	return err
}

// Caller is the namespace that called the receiver: the chain's last entry.
func (r Reference) Caller() string { return r.Chain[len(r.Chain)-1] }

// Origin is the edge the customer's request arrived at: the chain's first entry.
func (r Reference) Origin() string { return r.Chain[0] }

// FromAnswer is the edge's reference from its own live, verified batch answer: the organization,
// principal and credential as org-api named them, at when the edge accepted the request, its own
// namespace as the chain. ErrNotDelegable when the answer names no credential.
func FromAnswer(a *authz.BatchAnswer, at time.Time, namespace string) (Reference, error) {
	if a == nil || a.Credential == nil {
		return Reference{}, ErrNotDelegable
	}
	r := Reference{
		Org: a.Organization.ID, PrincipalType: a.Principal.Type, PrincipalID: a.Principal.ID,
		CredentialKind: a.Credential.Kind, CredentialID: a.Credential.ID,
		At: time.Unix(at.Unix(), 0).UTC(), Chain: []string{namespace},
	}
	if err := r.Valid(); err != nil {
		return Reference{}, err
	}
	return r, nil
}

// Relay is the reference a receiver sends onward: its namespace appended. It refuses a third
// entry or a repeat (ErrChain).
func (r Reference) Relay(namespace string) (Reference, error) {
	if len(r.Chain) >= MaxChain || slices.Contains(r.Chain, namespace) {
		return Reference{}, ErrChain
	}
	out := r
	out.Chain = append(slices.Clone(r.Chain), namespace)
	if err := out.Valid(); err != nil {
		return Reference{}, err
	}
	return out, nil
}
