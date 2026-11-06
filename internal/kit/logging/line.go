package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.opentelemetry.io/otel/trace"
)

const timeLayout = "2006-01-02T15:04:05.000Z"

// Context is what a line knows about the request it's in: the scope's IDs and the active span's.
type Context struct {
	RequestID      string `json:"requestId"`
	OrganizationID string `json:"organizationId"`
	TraceID        string `json:"traceId"`
	SpanID         string `json:"spanId"`
}

func contextOf(ctx context.Context) Context {
	var c Context
	if ctx == nil {
		return c
	}
	if s := ScopeFrom(ctx); s != nil {
		c.RequestID, c.OrganizationID = s.ids()
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		c.TraceID, c.SpanID = sc.TraceID().String(), sc.SpanID().String()
	}
	return c
}

type field struct {
	key   string
	value any
}

var (
	baseKeys    = map[string]bool{"time": true, "level": true, "message": true, "service": true, "msg": true}
	contextKeys = [...]string{"requestId", "organizationId", "traceId", "spanId"}
)

// buildLine lays out one line (contracts/vectors/logging.json, lines): base fields, context
// fields, then the attributes (JSON values). An attribute named like a base field is renamed
// attr.<key>; one named like a context field is that field when the context doesn't know it or
// agrees, and is renamed when it disagrees. A repeated key keeps its last value.
func buildLine(t time.Time, level, message, service string, c Context, attrs []field) []field {
	ctx := [...]string{c.RequestID, c.OrganizationID, c.TraceID, c.SpanID}
	var rest []field
	for _, a := range attrs {
		if i := slices.Index(contextKeys[:], a.key); i >= 0 {
			if s, ok := a.value.(string); ok && (ctx[i] == "" || ctx[i] == s) {
				ctx[i] = s
				continue
			}
			a.key = "attr." + a.key
		} else if baseKeys[a.key] {
			a.key = "attr." + a.key
		}
		rest = append(rest, a)
	}
	line := []field{{"time", t.UTC().Format(timeLayout)}, {"level", level}, {"message", message}, {"service", service}}
	for i, k := range contextKeys {
		if ctx[i] != "" {
			line = append(line, field{k, ctx[i]})
		}
	}
	at := map[string]int{}
	for _, a := range rest {
		v := Redact(a.value)
		if Sensitive(a.key) {
			v = Redacted
		}
		if i, ok := at[a.key]; ok {
			line[i].value = v
			continue
		}
		at[a.key] = len(line)
		line = append(line, field{a.key, v})
	}
	return line
}

func encodeLine(buf *bytes.Buffer, line []field) {
	buf.WriteByte('{')
	for i, f := range line {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeJSON(buf, f.key)
		buf.WriteByte(':')
		writeJSON(buf, f.value)
	}
	buf.WriteString("}\n")
}

// writeJSON writes v as JSON, or its fmt form as a string when encoding/json can't (NaN).
func writeJSON(buf *bytes.Buffer, v any) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		tmp.Reset()
		_ = enc.Encode(fmt.Sprint(v))
	}
	buf.Write(bytes.TrimSuffix(tmp.Bytes(), []byte("\n")))
}
