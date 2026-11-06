// Package trails records a product's Trails events (contracts/trails.md): the event, what it
// records from a request (secrets hidden), the transactional outbox, and the relay to audit-api.
//
// It mirrors login-stack's service-kit audit.ts, the implementation in production, and
// contracts/vectors/trails-recording.json checks it.
package trails

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/outbox"
)

// Event is a Trails event, eventVersion 1.0 (schemas/trail-event.schema.json). Every field is
// always written, null when empty.
type Event struct {
	EventVersion        string          `json:"eventVersion"`
	EventID             string          `json:"eventId"`
	EventTime           string          `json:"eventTime"`
	EventType           string          `json:"eventType"`
	EventCategory       string          `json:"eventCategory"`
	EventSource         string          `json:"eventSource"`
	EventName           string          `json:"eventName"`
	ReadOnly            bool            `json:"readOnly"`
	OrganizationID      string          `json:"organizationId"`
	ProjectID           *string         `json:"projectId"`
	UserIdentity        json.RawMessage `json:"userIdentity"`
	SourceIPAddress     *string         `json:"sourceIpAddress"`
	UserAgent           *string         `json:"userAgent"`
	RequestParameters   map[string]any  `json:"requestParameters"`
	ResponseElements    map[string]any  `json:"responseElements"`
	AdditionalEventData map[string]any  `json:"additionalEventData"`
	ErrorCode           *string         `json:"errorCode"`
	ErrorMessage        *string         `json:"errorMessage"`
	Resources           []Resource      `json:"resources"`
	RequestID           *string         `json:"requestId"`
}

// Resource is one resource a call touched.
type Resource struct {
	ResourceName string `json:"resourceName"`
	// Type is the resource type's name, capitalized: Organization, Project, Widget.
	Type string `json:"type"`
}

// Event types.
const (
	APICall      = "ApiCall"
	ServiceEvent = "ServiceEvent"
)

// ServiceIdentity is userIdentity for a change the platform made on its own, caused by
// invokedBy (billing.ezghcloud.com for a spend control, organizations.ezghcloud.com for an
// organization's deletion).
func ServiceIdentity(invokedBy string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "EzghService", "principalId": nil, "userName": nil, "isRoot": false,
		"accessKeyId": nil, "sessionContext": nil, "invokedBy": invokedBy,
	})
	return raw
}

// IngestKeyIdentity is userIdentity for something a project ingest key did (contracts/trails.md,
// "Ingest keys"), such as a dataset created automatically for an environment it sent: the key's
// ID is the principal and the access key, and no person, session or service is behind it. The
// event's OrganizationID and ProjectID must be the key's.
func IngestKeyIdentity(keyID string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"type": "IngestKey", "principalId": keyID, "userName": nil, "isRoot": false,
		"accessKeyId": keyID, "sessionContext": nil, "invokedBy": nil,
	})
	return raw
}

// acmeBindingSlug is an ACME binding's slug: eab_ and 12 characters.
var acmeBindingSlug = regexp.MustCompile(`^eab_[0-9a-z]{12}$`)

// s3AccessKeyID is an S3 access key's ID: oak_ and 12 characters.
var s3AccessKeyID = regexp.MustCompile(`^oak_[0-9a-z]{12}$`)

// AcmeBindingIdentity is userIdentity for what an ACME client does through a Private CA binding
// (contracts/trails.md, "ACME bindings"): the binding acts with its owner's access, as an API key
// acts with its owner's, so it's the owner (owner: the userIdentity of org-api's principal-batch
// answer, authz.CheckPrincipalBatch) with bindingSlug as the access key and credential
// acmeBinding. Nothing of a session the owner identity came with is kept. An owner that isn't a
// user, the root user or a bot, or a slug that isn't an eab_ slug, is an error: there's no
// identity to record.
func AcmeBindingIdentity(owner json.RawMessage, bindingSlug string) (json.RawMessage, error) {
	return productKeyIdentity("AcmeBindingIdentity", owner, bindingSlug, acmeBindingSlug, "acmeBinding")
}

// S3AccessKeyIdentity is userIdentity for what an S3 client does through an Object Storage access
// key (contracts/trails.md, "S3 access keys"), as AcmeBindingIdentity: the owner, with keyID, an oak_
// ID, as the access key and credential s3AccessKey.
func S3AccessKeyIdentity(owner json.RawMessage, keyID string) (json.RawMessage, error) {
	return productKeyIdentity("S3AccessKeyIdentity", owner, keyID, s3AccessKeyID, "s3AccessKey")
}

// productKeyIdentity is the owner's identity with a product-held key as its access key.
func productKeyIdentity(name string, owner json.RawMessage, key string, pattern *regexp.Regexp, credential string) (json.RawMessage, error) {
	var o struct {
		Type        string          `json:"type"`
		PrincipalID json.RawMessage `json:"principalId"`
		UserName    json.RawMessage `json:"userName"`
		IsRoot      bool            `json:"isRoot"`
	}
	if err := json.Unmarshal(owner, &o); err != nil {
		return nil, fmt.Errorf("trails.%s: the owner isn't a userIdentity: %w", name, err)
	}
	if o.Type != "Root" && o.Type != "User" && o.Type != "Bot" {
		return nil, fmt.Errorf("trails.%s: the owner is a user, the root user or a bot, not %q", name, o.Type)
	}
	if !pattern.MatchString(key) {
		return nil, fmt.Errorf("trails.%s: %q doesn't match %s", name, key, pattern)
	}
	orNull := func(v json.RawMessage) json.RawMessage {
		if len(v) == 0 {
			return json.RawMessage("null")
		}
		return v
	}
	return json.Marshal(map[string]any{
		"type": o.Type, "principalId": orNull(o.PrincipalID), "userName": orNull(o.UserName), "isRoot": o.IsRoot,
		"accessKeyId": key,
		"sessionContext": map[string]any{
			"credential": credential, "sessionIssuedAt": nil, "ssoProviderId": nil, "mfaAuthenticated": false,
		},
		"invokedBy": nil,
	})
}

// DelegatedUserAgent is the userAgent of a receiver's event for a delegated call (contracts/
// trails.md, "Delegated calls"): the calling service, <caller>.ezghcloud.com, as CloudTrail
// records service-invoked calls. Its userIdentity is org-api's delegated answer's, as given: the
// customer's, with invokedBy the same host.
func DelegatedUserAgent(caller string) string { return caller + ".ezghcloud.com" }

// Size limits: an event is at most 64 KiB; build to 60 KiB, dropping the big parts first.
const (
	maxEventBytes   = 60 * 1024
	maxUserAgent    = 512
	maxErrorCode    = 64
	maxErrorMessage = 1024
	maxResources    = MaxResources
)

// MaxResources is how many resources an event names at most (Fit keeps the first ones).
const MaxResources = 50

var truncated = map[string]any{"truncated": true}

// Fit trims the event to the limits: the user agent and error to their lengths, resources to
// 50, and an event over 60 KiB loses its requestParameters and responseElements, then its
// additionalEventData, each replaced by {"truncated": true}. An event is never dropped.
func (e *Event) Fit() {
	if e.UserAgent != nil && len(*e.UserAgent) > maxUserAgent {
		ua := truncateUTF8(*e.UserAgent, maxUserAgent)
		e.UserAgent = &ua
	}
	if e.ErrorCode != nil && len(*e.ErrorCode) > maxErrorCode {
		c := truncateUTF8(*e.ErrorCode, maxErrorCode)
		e.ErrorCode = &c
	}
	if e.ErrorMessage != nil && len(*e.ErrorMessage) > maxErrorMessage {
		m := truncateUTF8(*e.ErrorMessage, maxErrorMessage)
		e.ErrorMessage = &m
	}
	if len(e.Resources) > maxResources {
		e.Resources = e.Resources[:maxResources]
	}
	if e.size() <= maxEventBytes {
		return
	}
	if e.RequestParameters != nil {
		e.RequestParameters = truncated
	}
	if e.ResponseElements != nil {
		e.ResponseElements = truncated
	}
	if e.size() > maxEventBytes && e.AdditionalEventData != nil {
		e.AdditionalEventData = truncated
	}
}

func (e *Event) size() int {
	b, err := httpapi.Marshal(e)
	if err != nil {
		return 0
	}
	return len(b)
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// Insert writes the event into the outbox in tx: the transaction of the change it describes (or
// a transaction of its own, for a failed call after the rollback). It's stored as a JSON object:
// the encoded bytes go to the jsonb column as JSON, never as a JSON string inside jsonb. The row's
// created_at, available_at and attempts are the database's defaults.
func Insert(ctx context.Context, tx *db.Tx, e *Event) error {
	e.Fit()
	b, err := httpapi.Marshal(e)
	if err != nil {
		return fmt.Errorf("trails: encoding %s: %w", e.EventName, err)
	}
	return entdb.Client(tx).AuditOutbox.Create().SetID(e.EventID).SetEvent(json.RawMessage(b)).Exec(ctx)
}

// InsertAlone writes the event in a transaction of its own.
func InsertAlone(ctx context.Context, d *db.DB, e *Event) error {
	return d.RunTx(ctx, nil, func(tx *db.Tx) error { return Insert(ctx, tx, e) })
}

// NewRelay is the relay to audit-api's POST /internal/v1/events, with the shared
// X-Internal-Token (a per-sender ak_ credential is proposed; see contracts/service-auth.md).
func NewRelay(d *db.DB, auditAPIURL, internalToken string, logger *slog.Logger) *outbox.Relay {
	return &outbox.Relay{
		DB:        d,
		Table:     "audit_outbox",
		Name:      "audit relay",
		Target:    "audit-api",
		URL:       auditAPIURL + "/internal/v1/events",
		Authorize: func(r *http.Request) { r.Header.Set("X-Internal-Token", internalToken) },
		// Oldest eventTime first.
		Order: func(event json.RawMessage) string {
			var e struct {
				EventTime string `json:"eventTime"`
			}
			_ = json.Unmarshal(event, &e)
			return e.EventTime
		},
		BatchSize: outbox.DefaultBatchSize,
		Logger:    logger,
	}
}

// ---- What a call records --------------------------------------------------------------------

// Hidden replaces a secret's value.
const Hidden = "HIDDEN_DUE_TO_SECURITY_REASONS"

// secretKey is the backstop for fields an endpoint forgot to mark sensitive: the logging redaction
// words (contracts/vectors/logging.json) as substrings, the pairs spelled with or without _ or -,
// and PEM-encoded fields (keyPem, key_pem).
var secretKey = regexp.MustCompile(`(?i)token|secret|password|passwd|passphrase|authorization|cookie|credential|signature|api[-_]?key|private[-_]?key|(signing|encryption|access|application)[-_]?key$|card[-_]?number|cvc|cvv|otp$|pem$`)

const (
	maxDepth      = 6
	maxArrayItems = 100
)

// HideSecrets copies a generic JSON value with the endpoint's sensitive fields (matched without
// regard to case) and every
// secret-looking key hidden at any depth, values nested deeper than 6 levels cut to
// "[truncated]", and arrays kept to their first 100 items.
func HideSecrets(value any, sensitive []string) any {
	return hide(value, sensitive, 0)
}

func hide(value any, sensitive []string, depth int) any {
	switch v := value.(type) {
	case map[string]any:
		if depth >= maxDepth {
			return "[truncated]"
		}
		out := make(map[string]any, len(v))
		for key, item := range v {
			// Without regard to case: JSON decoders (Go's, and others) fill a field from a key spelled
			// in any case, so "Email" must be as hidden as "email".
			if slices.ContainsFunc(sensitive, func(s string) bool { return strings.EqualFold(s, key) }) || secretKey.MatchString(key) {
				out[key] = Hidden
			} else {
				out[key] = hide(item, sensitive, depth+1)
			}
		}
		return out
	case []any:
		if depth >= maxDepth {
			return "[truncated]"
		}
		n := min(len(v), maxArrayItems)
		out := make([]any, n)
		for i := range n {
			out[i] = hide(v[i], sensitive, depth+1)
		}
		return out
	default:
		return value
	}
}

// RequestParameters is path parameters, then query parameters, then the parameters an endpoint
// chose to record (later ones win), secrets hidden; nil when there are none. A body that isn't a
// JSON object is ignored. Never pass a request body: that's DefaultRequestParameters.
func RequestParameters(path map[string]string, query map[string]any, body any, sensitive []string) map[string]any {
	merged := map[string]any{}
	for k, v := range path {
		merged[k] = v
	}
	for k, v := range query {
		merged[k] = v
	}
	if obj, ok := body.(map[string]any); ok {
		for k, v := range obj {
			merged[k] = v
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return HideSecrets(merged, sensitive).(map[string]any)
}

// DefaultRequestParameters is what an event records when the endpoint sets no parameters: path
// and query parameters, secrets hidden, then only the JSON body's top-level field names, each
// value Hidden, never replacing a path or query value; nil when there are none. A body's values
// are recorded only as an endpoint chooses them, so a key the backstop misses can't leak.
func DefaultRequestParameters(path map[string]string, query map[string]any, body any, sensitive []string) map[string]any {
	merged := RequestParameters(path, query, nil, sensitive)
	if obj, ok := body.(map[string]any); ok {
		for k := range obj {
			if merged == nil {
				merged = map[string]any{}
			}
			if _, taken := merged[k]; !taken {
				merged[k] = Hidden
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

// identifiers are what responseElements record by default.
var identifiers = []string{"id", "slug", "name", "resourceName", "prefix", "expiresAt"}

func pickIdentifiers(value any) map[string]any {
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	picked := map[string]any{}
	for _, key := range identifiers {
		if v, present := obj[key]; present {
			picked[key] = v
		}
	}
	if len(picked) == 0 {
		return nil
	}
	return picked
}

// ResponseElements is a successful write's responseElements: the identifiers at the top level of
// the response or, when there are none there, one object down; plus the top-level fields the
// endpoint's catalog entry lists (secrets hidden). Never a token or a secret.
func ResponseElements(body any, extra []string) map[string]any {
	base := pickIdentifiers(body)
	if base == nil {
		if obj, ok := body.(map[string]any); ok {
			nested := map[string]any{}
			for key, value := range obj {
				if picked := pickIdentifiers(value); picked != nil {
					nested[key] = picked
				}
			}
			if len(nested) > 0 {
				base = nested
			}
		}
	}
	obj, ok := body.(map[string]any)
	if !ok {
		return base
	}
	listed := map[string]any{}
	for _, key := range extra {
		if v, present := obj[key]; present {
			listed[key] = HideSecrets(v, nil)
		}
	}
	if len(listed) == 0 {
		return base
	}
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range listed {
		out[k] = v
	}
	return out
}
