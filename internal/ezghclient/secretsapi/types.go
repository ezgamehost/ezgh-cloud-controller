package secretsapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// MaxValueBytes is the largest field value, decoded (8 KiB), as secrets-api documents it.
const MaxValueBytes = 8 << 10

// Value is a field's value: a JSON string, or `{ "base64": "…" }` for binary, returned in the
// form it was written.
type Value struct {
	// Text is the value when it isn't Binary.
	Text string
	// Base64 is the value, base64 encoded, when it is Binary.
	Base64 string
	Binary bool
}

// NewValue is the form to write b in: a string when b is UTF-8, otherwise base64.
func NewValue(b []byte) Value {
	if utf8.Valid(b) {
		return Value{Text: string(b)}
	}
	return Value{Binary: true, Base64: base64.StdEncoding.EncodeToString(b)}
}

// Bytes is the value's exact bytes (base64 decoded for a binary one).
func (v Value) Bytes() ([]byte, error) {
	if !v.Binary {
		return []byte(v.Text), nil
	}
	b, err := base64.StdEncoding.DecodeString(v.Base64)
	if err != nil {
		return nil, fmt.Errorf("secrets-api answered with a binary value that isn't valid base64: %w", err)
	}
	return b, nil
}

func (v Value) MarshalJSON() ([]byte, error) {
	if v.Binary {
		return json.Marshal(struct {
			Base64 string `json:"base64"`
		}{v.Base64})
	}
	return json.Marshal(v.Text)
}

func (v *Value) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		*v = Value{}
		return json.Unmarshal(data, &v.Text)
	}
	var obj struct {
		Base64 *string `json:"base64"`
	}
	if err := json.Unmarshal(data, &obj); err != nil || obj.Base64 == nil {
		return fmt.Errorf("a field value is a string or { \"base64\": … }")
	}
	*v = Value{Binary: true, Base64: *obj.Base64}
	return nil
}

// Secret is a secret's metadata (never its values).
type Secret struct {
	ID             string            `json:"id"`
	Slug           string            `json:"slug"`
	Name           string            `json:"name"`
	ResourceName   string            `json:"resourceName"`
	OrganizationID string            `json:"organizationId"`
	ProjectID      string            `json:"projectId"`
	Region         string            `json:"region"`
	Description    string            `json:"description"`
	Tags           map[string]string `json:"tags"`
	LatestVersion  *int              `json:"latestVersion"`
	VersionCount   int               `json:"versionCount"`
	FieldCount     int               `json:"fieldCount"`
	GrantCount     int               `json:"grantCount"`
	CreatedAt      string            `json:"createdAt"`
	UpdatedAt      string            `json:"updatedAt"`
}

// Version is one version's metadata: its state and field names.
type Version struct {
	Number      int             `json:"number"`
	State       string          `json:"state"`
	FieldNames  []string        `json:"fieldNames"`
	FieldCount  int             `json:"fieldCount"`
	CreatedBy   json.RawMessage `json:"createdBy"`
	CreatedAt   string          `json:"createdAt"`
	DestroyedAt *string         `json:"destroyedAt"`
}

// Grant lets a product read a secret.
type Grant struct {
	ID               string          `json:"id"`
	Slug             string          `json:"slug"`
	SecretID         string          `json:"secretId"`
	Consumer         string          `json:"consumer"`
	ConsumerResource string          `json:"consumerResource"`
	Version          string          `json:"version"`
	CreatedBy        json.RawMessage `json:"createdBy"`
	CreatedAt        string          `json:"createdAt"`
}

// GrantBody is what creating a grant sends: the product's namespace and its resource's name,
// and the version, "latest" unless pinned.
type GrantBody struct {
	Consumer         string `json:"consumer"`
	ConsumerResource string `json:"consumerResource"`
	Version          string `json:"version,omitempty"`
}

// CreateBody is what creating a secret sends.
type CreateBody struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	Fields      map[string]Value  `json:"fields,omitempty"`
}

// VersionBody is what writing a version sends: either Fields (the whole version), or Base with
// Set and Remove (a change to a version's fields).
type VersionBody struct {
	Fields map[string]Value `json:"fields,omitempty"`
	// Base is a version number (int) or "latest".
	Base   any              `json:"base,omitempty"`
	Set    map[string]Value `json:"set,omitempty"`
	Remove []string         `json:"remove,omitempty"`
}

// ValueAnswer is a version's values: `{ version, fields }`.
type ValueAnswer struct {
	Version int              `json:"version"`
	Fields  map[string]Value `json:"fields"`
}

// FieldAnswer is one field's value: `{ version, name, value }`.
type FieldAnswer struct {
	Version int    `json:"version"`
	Name    string `json:"name"`
	Value   Value  `json:"value"`
}
