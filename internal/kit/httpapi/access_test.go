package httpapi_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/logging"
)

func init() {
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
}

func serve(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, []map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	rec := httptest.NewRecorder()
	httpapi.AccessLog(logging.New(&buf, "widgets-api", false), h).ServeHTTP(rec, req)
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		lines = append(lines, m)
	}
	return rec, lines
}

func widgets(t *testing.T) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/organizations/{organizationId}/widgets", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		logging.SetCaller(r.Context(), logging.Caller{Type: "user", ID: "usr_1", APIKeyID: "key_1", OrganizationID: "org_1"})
		logging.New(io.Discard, "x", false).InfoContext(r.Context(), "inside")
		if string(body) == "bad" {
			httpapi.WriteError(w, httpapi.AccessDenied("create widgets", true))
			return
		}
		httpapi.WriteJSON(w, http.StatusCreated, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	return mux
}

func TestAccessLogLine(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/organizations/org_1/widgets?token=sekrit", strings.NewReader("bad"))
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.9")
	req.Header.Set("User-Agent", "ezgh/0.27.0")
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec, lines := serve(t, widgets(t), req)
	if len(lines) != 1 {
		t.Fatalf("lines %v", lines)
	}
	l := lines[0]
	for k, want := range map[string]any{
		"message": "request", "kind": "access", "method": "POST", "route": "/v1/organizations/{organizationId}/widgets",
		"status": float64(403), "requestBytes": float64(3), "clientIp": "203.0.113.9", "userAgent": "ezgh/0.27.0",
		"principalType": "user", "principalId": "usr_1", "apiKeyId": "key_1", "organizationId": "org_1",
		"errorCode": "access_denied", "traceId": "4bf92f3577b34da6a3ce929d0e0e4736", "level": "info",
	} {
		if l[k] != want {
			t.Errorf("%s = %v, want %v", k, l[k], want)
		}
	}
	if l["requestId"] != rec.Header().Get("X-Request-Id") || l["spanId"] == "00f067aa0ba902b7" || l["responseBytes"] != float64(rec.Body.Len()) {
		t.Errorf("requestId/spanId/responseBytes: %v", l)
	}
	raw, _ := json.Marshal(l)
	if strings.Contains(string(raw), "sekrit") || strings.Contains(string(raw), "?") {
		t.Errorf("query string logged: %s", raw)
	}
}

func TestAccessLogSkipsProbesAndMarksUnmatched(t *testing.T) {
	for _, p := range []string{"/health", "/ready", "/metrics"} {
		if _, lines := serve(t, widgets(t), httptest.NewRequest("GET", p, nil)); len(lines) != 0 {
			t.Errorf("%s logged %v", p, lines)
		}
	}
	_, lines := serve(t, widgets(t), httptest.NewRequest("GET", "/nope/secret-path", nil))
	if lines[0]["route"] != "unmatched" || lines[0]["status"] != float64(404) {
		t.Errorf("unmatched: %v", lines[0])
	}
}

func TestAccessLogPanicIs500(t *testing.T) {
	rec, lines := serve(t, widgets(t), httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != 500 || len(lines) != 2 || lines[0]["message"] != "panic" || lines[1]["status"] != float64(500) || lines[1]["level"] != "error" {
		t.Errorf("code %d lines %v", rec.Code, lines)
	}
}

// Review focus 5.
func TestAccessLogUnsampledTraceparent(t *testing.T) {
	req := httptest.NewRequest("GET", "/nope", nil)
	req.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00")
	if _, lines := serve(t, widgets(t), req); lines[0]["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("unsampled trace lost: %v", lines[0])
	}
	if _, lines := serve(t, widgets(t), httptest.NewRequest("GET", "/nope", nil)); len(lines[0]["traceId"].(string)) != 32 {
		t.Errorf("no new trace: %v", lines[0])
	}
}

// Review focus 1.
func TestAccessLogStreams(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: 1\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: 2\n\n"))
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController.Flush: %v", err)
		}
	})
	rec, lines := serve(t, h, httptest.NewRequest("GET", "/v1/stream", nil))
	if !rec.Flushed || lines[0]["responseBytes"] != float64(18) {
		t.Errorf("flushed %v lines %v", rec.Flushed, lines)
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

// Review focus 2.
func TestAccessLogHijack(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	var buf bytes.Buffer
	h := httpapi.AccessLog(logging.New(&buf, "widgets-api", false), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := http.NewResponseController(w).Hijack(); err != nil {
			t.Errorf("Hijack: %v", err)
		}
	}))
	req := httptest.NewRequest("GET", "/v1/live", nil)
	req.Header.Set("Upgrade", "websocket")
	h.ServeHTTP(&hijackRecorder{httptest.NewRecorder(), server}, req)
	var l map[string]any
	_ = json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &l)
	if l["status"] != float64(101) {
		t.Errorf("line %v", l)
	}
}

// flushFails is a ResponseWriter whose flush reports an error, as a closed SSE connection's does.
type flushFails struct {
	*httptest.ResponseRecorder
	err error
}

func (w flushFails) FlushError() error { return w.err }

// A flush error reaches the handler through AccessLog's writer (obs-stack's SSE tail stops on it).
func TestAccessLogFlushError(t *testing.T) {
	want := errors.New("connection gone")
	var got error
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = http.NewResponseController(w).Flush()
	})
	httpapi.AccessLog(logging.New(io.Discard, "widgets-api", false), h).
		ServeHTTP(flushFails{httptest.NewRecorder(), want}, httptest.NewRequest("GET", "/v1/tail", nil))
	if !errors.Is(got, want) {
		t.Errorf("Flush = %v, want %v", got, want)
	}
}
