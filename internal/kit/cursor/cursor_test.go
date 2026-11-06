package cursor_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/cursor"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// contracts/vectors/cursors.json: audit-api's format, byte for byte.
func TestCursorVectors(t *testing.T) {
	var file struct {
		Secret string `json:"secret"`
		Encode []struct {
			OrganizationID string          `json:"organizationId"`
			Scope          string          `json:"scope"`
			Key            json.RawMessage `json:"key"`
			Cursor         string          `json:"cursor"`
		} `json:"encode"`
		Decode []struct {
			Name           string `json:"name"`
			OrganizationID string `json:"organizationId"`
			Scope          string `json:"scope"`
			Cursor         string `json:"cursor"`
			Expected       struct {
				Key   []string `json:"key"`
				Error string   `json:"error"`
			} `json:"expected"`
		} `json:"decode"`
	}
	vectors.Load(t, "cursors.json", &file)
	s := cursor.NewSigner(file.Secret)
	for _, c := range file.Encode {
		var key any
		_ = json.Unmarshal(c.Key, &key)
		got, err := s.Encode(c.OrganizationID, c.Scope, key)
		if err != nil || got != c.Cursor {
			t.Errorf("encode: %s, %v\nwant %s", got, err, c.Cursor)
		}
	}
	reasons := map[string]error{
		"malformed": cursor.ErrMalformed, "tampered": cursor.ErrTampered, "version": cursor.ErrVersion,
		"wrong_organization": cursor.ErrWrongOrganization, "wrong_scope": cursor.ErrWrongScope,
	}
	for _, c := range file.Decode {
		var key []string
		err := s.Decode(c.OrganizationID, c.Scope, c.Cursor, &key)
		if c.Expected.Error != "" {
			if want := reasons[c.Expected.Error]; want == nil || !errors.Is(err, want) {
				t.Errorf("%s: got %v, want %s", c.Name, err, c.Expected.Error)
			}
			continue
		}
		if err != nil || !slices.Equal(key, c.Expected.Key) {
			t.Errorf("%s: got %v, %v", c.Name, key, err)
		}
	}
}
