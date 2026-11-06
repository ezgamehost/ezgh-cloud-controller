package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// contracts/vectors/logging.json.
type loggingVectors struct {
	Redacted  string `json:"redacted"`
	Sensitive struct {
		OneWord    []string `json:"oneWord"`
		TwoWords   []string `json:"twoWords"`
		Containers []string `json:"containers"`
	} `json:"sensitive"`
	Words []struct {
		Key   string   `json:"key"`
		Words []string `json:"words"`
	} `json:"words"`
	Keys []struct {
		Key       string `json:"key"`
		Sensitive bool   `json:"sensitive"`
	} `json:"keys"`
	Redact []struct {
		Name   string `json:"name"`
		Input  any    `json:"input"`
		Output any    `json:"output"`
	} `json:"redact"`
	Routes []struct {
		Input string `json:"input"`
		Route string `json:"route"`
	} `json:"routes"`
	Lines []struct {
		Name    string              `json:"name"`
		Level   string              `json:"level"`
		Message string              `json:"message"`
		Scope   *Context            `json:"scope"`
		Attrs   [][]json.RawMessage `json:"attrs"`
		Line    map[string]any      `json:"line"`
	} `json:"lines"`
	Access []struct {
		Name  string `json:"name"`
		Input struct {
			Method        string  `json:"method"`
			Route         string  `json:"route"`
			Status        int     `json:"status"`
			DurationMs    float64 `json:"durationMs"`
			RequestBytes  *int64  `json:"requestBytes"`
			ResponseBytes *int64  `json:"responseBytes"`
			ClientIP      string  `json:"clientIp"`
			UserAgent     string  `json:"userAgent"`
			Caller        *struct {
				Type     string `json:"type"`
				ID       string `json:"id"`
				APIKeyID string `json:"apiKeyId"`
			} `json:"caller"`
			ErrorCode *string `json:"errorCode"`
		} `json:"input"`
		Level string         `json:"level"`
		Attrs map[string]any `json:"attrs"`
	} `json:"access"`
}

func load(t *testing.T) loggingVectors {
	t.Helper()
	var v loggingVectors
	vectors.Load(t, "logging.json", &v)
	return v
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sorted(s []string) []string { s = slices.Clone(s); slices.Sort(s); return s }

func TestVectorLists(t *testing.T) {
	v := load(t)
	if v.Redacted != Redacted {
		t.Errorf("Redacted = %q, vectors say %q", Redacted, v.Redacted)
	}
	for _, c := range []struct {
		name string
		got  map[string]bool
		want []string
	}{{"oneWord", sensitiveOne, v.Sensitive.OneWord}, {"twoWords", sensitiveTwo, v.Sensitive.TwoWords}, {"containers", containers, v.Sensitive.Containers}} {
		if !slices.Equal(keys(c.got), sorted(c.want)) {
			t.Errorf("%s = %v, vectors say %v", c.name, keys(c.got), sorted(c.want))
		}
	}
}

func TestWordsAndKeys(t *testing.T) {
	v := load(t)
	for _, c := range v.Words {
		if got := words(c.Key); !slices.Equal(got, c.Words) {
			t.Errorf("words(%q) = %q, want %q", c.Key, got, c.Words)
		}
	}
	for _, c := range v.Keys {
		if got := Sensitive(c.Key); got != c.Sensitive {
			t.Errorf("Sensitive(%q) = %v, want %v", c.Key, got, c.Sensitive)
		}
	}
}

func TestRedactVectors(t *testing.T) {
	for _, c := range load(t).Redact {
		if got := Redact(c.Input); !reflect.DeepEqual(got, c.Output) {
			t.Errorf("%s: Redact = %#v, want %#v", c.Name, got, c.Output)
		}
	}
}

func TestRouteVectors(t *testing.T) {
	for _, c := range load(t).Routes {
		if got := RouteOf(c.Input); got != c.Route {
			t.Errorf("RouteOf(%q) = %q, want %q", c.Input, got, c.Route)
		}
	}
}

// attrsJSON is a []slog.Attr as the JSON object a line would carry.
func attrsJSON(t *testing.T, attrs []slog.Attr) map[string]any {
	t.Helper()
	var fields []field
	for _, a := range attrs {
		fields = putAttr(fields, nil, a)
	}
	var buf bytes.Buffer
	encodeLine(&buf, fields)
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAccessVectors(t *testing.T) {
	for _, c := range load(t).Access {
		in := c.Input
		a := Access{Method: in.Method, Route: in.Route, Status: in.Status, DurationMs: in.DurationMs,
			RequestBytes: -1, ResponseBytes: -1, ClientIP: in.ClientIP, UserAgent: in.UserAgent}
		if in.RequestBytes != nil {
			a.RequestBytes = *in.RequestBytes
		}
		if in.ResponseBytes != nil {
			a.ResponseBytes = *in.ResponseBytes
		}
		if in.Caller != nil {
			a.Caller = Caller{Type: in.Caller.Type, ID: in.Caller.ID, APIKeyID: in.Caller.APIKeyID}
		}
		if in.ErrorCode != nil {
			a.ErrorCode = *in.ErrorCode
		}
		level, attrs := AccessAttrs(a)
		if got := levelName(level); got != c.Level {
			t.Errorf("%s: level %q, want %q", c.Name, got, c.Level)
		}
		if got := attrsJSON(t, attrs); !reflect.DeepEqual(got, c.Attrs) {
			t.Errorf("%s: attrs %v, want %v", c.Name, got, c.Attrs)
		}
	}
}

var fixedTime = time.Date(2026, 10, 14, 18, 3, 22, 123_000_000, time.UTC)

func TestLineVectors(t *testing.T) {
	for _, c := range load(t).Lines {
		var attrs []field
		for _, kv := range c.Attrs {
			var k string
			var val any
			if err := json.Unmarshal(kv[0], &k); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(kv[1], &val); err != nil {
				t.Fatal(err)
			}
			attrs = append(attrs, field{k, val})
		}
		var ctx Context
		if c.Scope != nil {
			ctx = *c.Scope
		}
		line := buildLine(fixedTime, c.Level, c.Message, "widgets-api", ctx, attrs)
		var buf bytes.Buffer
		encodeLine(&buf, line)
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v in %s", c.Name, err, buf.String())
		}
		if !reflect.DeepEqual(got, c.Line) {
			t.Errorf("%s:\n got %s\nwant %v", c.Name, buf.String(), c.Line)
		}
		if order := []string{line[0].key, line[1].key, line[2].key, line[3].key}; !slices.Equal(order, []string{"time", "level", "message", "service"}) {
			t.Errorf("%s: first keys %v", c.Name, order)
		}
	}
}
