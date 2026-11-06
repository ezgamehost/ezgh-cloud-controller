// Package enroll is how a node gets its first certificate: POST /v1/enroll on a product's enroll
// host, off the API's router and undeclared (contracts/registration.md, "Node-facing routes").
// Lifted from databases-stack (9ff61c1; lb-stack's at b63684f). The product's controller holds
// issuance, so this route records the request (Store), asks for it (a KindEnroll signal) and waits a
// moment. Retrying with the same public key is idempotent, so a lost reply strands nothing.
package enroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/signals"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

// maxBody is far above any CSR.
const maxBody = 16 << 10

// MaxKeys bounds the keys one node may enroll: a Store answers ErrTooManyKeys past it.
const MaxKeys = 8

var (
	// ErrUnknown is an instance with no live node of the product's: 403.
	ErrUnknown = errors.New("enroll: no live node is this instance")
	// ErrTooManyKeys is a node past MaxKeys: 403.
	ErrTooManyKeys = errors.New("enroll: too many keys")
)

// Row is one enrollment: a node's key, and its certificate once issued.
type Row struct {
	ID             string
	CertificatePEM *string
	NotAfter       *time.Time
}

// Store is the product's enrollment rows (its ent-typed table).
type Store interface {
	// Row is the row for the live node on instance and this key (the SHA-256 of its
	// SubjectPublicKeyInfo, hex), made with csr if it's new: ErrUnknown, ErrTooManyKeys.
	Row(ctx context.Context, instance, publicKeySHA256, csr string) (Row, error)
	// Get is a row by ID.
	Get(ctx context.Context, id string) (Row, error)
}

// IssuedChannel is what the controller publishes on once a row is issued.
func IssuedChannel(prefix, id string) string { return prefix + ":issued:" + id }

// Handler serves the route.
type Handler struct {
	OrgAPI  *authz.Client
	Redis   *redis.Client
	Signals *signals.Publisher
	Store   Store
	// Prefix is the product's, for IssuedChannel.
	Prefix string
	// Action is what only the product's node Bots may do (databases.enrollments.create). It's in
	// no managed policy.
	Action string
	// PlatformOrganization is the one organization a node's key is in.
	PlatformOrganization string
	// CN is every node certificate's common name: Private CA takes it from the CSR, and a first CSR
	// can't know its scope.
	CN string
	// Respond is the product's answer body for an issued row.
	Respond func(certificatePEM string, notAfter time.Time) any
	Logger  *slog.Logger
	// Wait is how long a request waits for the controller; zero is 10 s. Then 503, and the node
	// retries with the same key.
	Wait time.Duration
}

// Routes is the route, apart from the API. service.ServeWith writes its access line.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enroll", h.enroll)
	return mux
}

func answer(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func refuse(w http.ResponseWriter, status int, code string) {
	answer(w, status, map[string]any{"error": map[string]string{"code": code, "message": http.StatusText(status)}})
}

// identity is the part of org-api's answer enrollment may read, and nothing else may.
type identity struct {
	SessionContext struct {
		SourceIdentity *struct {
			Type        string `json:"type"`
			PrincipalID string `json:"principalId"`
		} `json:"sourceIdentity"`
	} `json:"sessionContext"`
}

// InstanceOf is the instance a node Bot's key belongs to: asked of org-api as the caller, in the
// platform organization, for the one action, and only a ComputeInstance source counts
// (identity-and-authz.md, "sourceIdentity": a product enrolling its own nodes). Everything else
// about the key is refused alike.
func (h *Handler) InstanceOf(r *http.Request) (string, *httpapi.Error) {
	cred := authz.Credential{Header: "Authorization", Value: r.Header.Get("Authorization"), ForwardedFor: r.Header.Get("X-Forwarded-For")}
	perm, err := h.OrgAPI.Check(r.Context(), cred, h.PlatformOrganization, []string{h.Action}, "")
	if err != nil {
		return "", err
	}
	if !perm.Allows(h.Action) {
		return "", httpapi.AccessDenied(h.Action, true)
	}
	var who identity
	if json.Unmarshal(perm.UserIdentity, &who) != nil || who.SessionContext.SourceIdentity == nil ||
		who.SessionContext.SourceIdentity.Type != "ComputeInstance" || who.SessionContext.SourceIdentity.PrincipalID == "" {
		return "", httpapi.AccessDenied(h.Action, true)
	}
	return who.SessionContext.SourceIdentity.PrincipalID, nil
}

// ReadCSR checks a CSR's signature, which proves the key, and names the key. Nothing else in it is
// used but its common name, which Private CA keeps, and which must be cn unless cn is empty: a
// certificate's SANs come from the product's rows. Only what Private CA will sign is accepted.
func ReadCSR(raw, cn string) (string, bool) {
	block, _ := pem.Decode([]byte(raw))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return "", false
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		return "", false
	}
	if _, ecdsaKey := csr.PublicKey.(*ecdsa.PublicKey); !ecdsaKey {
		return "", false
	}
	if cn != "" && csr.Subject.CommonName != cn {
		return "", false
	}
	sum := sha256.Sum256(csr.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:]), true
}

func (h *Handler) enroll(w http.ResponseWriter, r *http.Request) {
	instance, denied := h.InstanceOf(r)
	if denied != nil {
		if denied.Status >= http.StatusInternalServerError || denied.RetryAfter > 0 {
			h.Logger.Warn("enroll: org-api didn't answer", "status", denied.Status, "code", denied.Code)
		}
		if denied.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(denied.RetryAfter))
		}
		refuse(w, denied.Status, denied.Code)
		return
	}
	var in struct {
		CSR string `json:"csr"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request")
		return
	}
	key, ok := ReadCSR(in.CSR, h.CN)
	if !ok {
		refuse(w, http.StatusBadRequest, "invalid_request")
		return
	}
	row, err := h.Store.Row(r.Context(), instance, key, in.CSR)
	if errors.Is(err, ErrUnknown) || errors.Is(err, ErrTooManyKeys) {
		refuse(w, http.StatusForbidden, "access_denied")
		return
	}
	if err != nil {
		h.unavailable(w, err)
		return
	}
	if Usable(row) {
		answer(w, http.StatusOK, h.Respond(*row.CertificatePEM, *row.NotAfter))
		return
	}
	// Subscribe before asking, or the answer could go by first.
	wait := h.Wait
	if wait == 0 {
		wait = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	sub, err := h.Redis.Subscribe(ctx, IssuedChannel(h.Prefix, row.ID))
	if err != nil {
		h.unavailable(w, err)
		return
	}
	defer func() { _ = sub.Close() }()
	body, err := json.Marshal(map[string]string{"row": row.ID})
	if err != nil {
		h.unavailable(w, err)
		return
	}
	if err := h.Signals.Publish(ctx, signals.Signal{Kind: signals.KindEnroll, Node: instance, Body: body}); err != nil {
		h.unavailable(w, err)
		return
	}
	if _, err := sub.Receive(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		h.unavailable(w, err)
		return
	}
	if row, err = h.Store.Get(r.Context(), row.ID); err != nil {
		h.unavailable(w, err)
		return
	}
	if !Usable(row) {
		w.Header().Set("Retry-After", strconv.Itoa(2))
		refuse(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	answer(w, http.StatusOK, h.Respond(*row.CertificatePEM, *row.NotAfter))
}

// Usable is whether a row holds a certificate that still works: an expired node enrolls again.
func Usable(row Row) bool {
	return row.CertificatePEM != nil && row.NotAfter != nil && time.Now().Before(*row.NotAfter)
}

func (h *Handler) unavailable(w http.ResponseWriter, err error) {
	h.Logger.Warn("enroll: unavailable", "error", err.Error())
	w.Header().Set("Retry-After", strconv.Itoa(5))
	refuse(w, http.StatusServiceUnavailable, "unavailable")
}
