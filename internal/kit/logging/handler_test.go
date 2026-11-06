package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func logged(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

func testLogger(buf *bytes.Buffer, level slog.Level) *slog.Logger {
	return slog.New(newHandler(buf, "widgets-api", level, func() time.Time { return fixedTime }))
}

func TestHandlerContextGroupsAndSecrets(t *testing.T) {
	var buf bytes.Buffer
	scope := NewScope("req-1")
	ctx := WithScope(context.Background(), scope)
	SetCaller(ctx, Caller{Type: "user", ID: "usr_1", OrganizationID: "org_1"})
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid}))
	log := testLogger(&buf, slog.LevelInfo).With("component", "relay").WithGroup("upstream")
	log.InfoContext(ctx, "called", "status", 502, slog.Group("headers", "Authorization", "Bearer x", "accept", "*/*"),
		"key", Secret("s3cr3t"), "err", errors.New("boom"), "took", 1500*time.Microsecond)
	got := logged(t, &buf)[0]
	want := map[string]any{
		"time": "2026-10-14T18:03:22.123Z", "level": "info", "message": "called", "service": "widgets-api",
		"requestId": "req-1", "organizationId": "org_1",
		"traceId": "4bf92f3577b34da6a3ce929d0e0e4736", "spanId": "00f067aa0ba902b7",
		"component": "relay",
		"upstream": map[string]any{
			"status":  float64(502),
			"headers": map[string]any{"Authorization": "[REDACTED]", "accept": "*/*"},
			"key":     "[REDACTED]", "err": "boom", "took": 1.5,
		},
	}
	if !jsonEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if strings.Contains(buf.String(), "s3cr3t") || strings.Contains(buf.String(), "Bearer x") {
		t.Errorf("secret leaked: %s", buf.String())
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestHandlerLevels(t *testing.T) {
	var buf bytes.Buffer
	log := testLogger(&buf, slog.LevelWarn)
	log.Info("dropped")
	log.Warn("kept")
	log.Log(context.Background(), slog.LevelError+4, "fatal-ish")
	lines := logged(t, &buf)
	if len(lines) != 2 || lines[0]["level"] != "warn" || lines[1]["level"] != "error" {
		t.Errorf("lines %v", lines)
	}
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "": slog.LevelInfo, "WARN": slog.LevelWarn, "error": slog.LevelError, "nonsense": slog.LevelInfo} {
		if got := levelFrom(in); got != want {
			t.Errorf("levelFrom(%q) = %v, want %v", in, got, want)
		}
	}
}

type cyclic struct{ Next *cyclic }

// Review focus 3: values encoding/json can't write still produce a line; and the ones that are
// structs, maps or pointers are walked, so a secret inside one is redacted, never fmt-printed.
func TestHandlerUnencodableValues(t *testing.T) {
	var buf bytes.Buffer
	c := &cyclic{}
	c.Next = c
	testLogger(&buf, slog.LevelInfo).Info("odd", "cycle", c, "ch", make(chan int), "nan", math.NaN())
	got := logged(t, &buf)[0]
	for _, k := range []string{"cycle", "ch", "nan"} {
		if got[k] == nil {
			t.Errorf("%s missing from %v", k, got)
		}
	}
	if _, ok := got["nan"].(string); !ok {
		t.Errorf("nan = %#v, want its fmt form", got["nan"])
	}

	req := httptest.NewRequest("GET", "https://api.example/v1/x?access_token=sekrit-query", nil)
	req.Header.Set("Authorization", "Bearer sekrit-bearer")
	type config struct {
		DBPassword string
		OnReady    func()
	}
	values := []any{
		"req", req,
		"config", config{DBPassword: "sekrit-db", OnReady: func() {}},
		"m", map[string]any{"token": "sekrit-token", "f": func() {}, "kept": "visible"},
	}
	for _, local := range []bool{false, true} {
		buf.Reset()
		var log *slog.Logger
		if local {
			log = New(&buf, "widgets-api", true)
		} else {
			log = testLogger(&buf, slog.LevelInfo)
		}
		log.Info("unencodable", values...)
		out := buf.String()
		if strings.Contains(out, "sekrit") {
			t.Errorf("local=%v: secret leaked: %s", local, out)
		}
		if !strings.Contains(out, "visible") || !strings.Contains(out, "unencodable") {
			t.Errorf("local=%v: line lost its values: %s", local, out)
		}
		if !local {
			got := logged(t, &buf)[0]
			if cfg, _ := got["config"].(map[string]any); cfg["DBPassword"] != Redacted {
				t.Errorf("config = %#v, want DBPassword redacted", got["config"])
			}
		}
	}
}

func TestLocalTextIsRedacted(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "widgets-api", true).Info("hi", "password", "hunter2", "nested", map[string]any{"token": "t"})
	if s := buf.String(); strings.Contains(s, "hunter2") || strings.Contains(s, `"t"`) || !strings.Contains(s, "service=widgets-api") {
		t.Errorf("local line %q", s)
	}
}

func TestSecretEverywhere(t *testing.T) {
	s := Secret("p")
	b, _ := json.Marshal(struct{ S Secret }{s})
	if s.String() != Redacted || string(b) != `{"S":"[REDACTED]"}` || s.Reveal() != "p" {
		t.Errorf("String %q, JSON %s, Reveal %q", s.String(), b, s.Reveal())
	}
}
