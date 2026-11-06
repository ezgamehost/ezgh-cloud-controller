package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// The client is the rightmost X-Forwarded-For entry, the Gateway's; the rest is client-supplied.
func TestClientIP(t *testing.T) {
	for _, c := range []struct {
		name    string
		headers []string
		want    string
	}{
		{"one entry", []string{"203.0.113.9"}, "203.0.113.9"},
		{"the rightmost entry, whatever the client put to its left", []string{"1.2.3.4, 203.0.113.9"}, "203.0.113.9"},
		{"several headers are one list", []string{"1.2.3.4", "198.51.100.1, 203.0.113.9 "}, "203.0.113.9"},
		{"empty entries are skipped", []string{"2001:db8::1, "}, "2001:db8::1"},
		{"the peer without one", nil, "192.0.2.1"},
		{"the peer when it's all blank", []string{" , "}, "192.0.2.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		for _, h := range c.headers {
			r.Header.Add("X-Forwarded-For", h)
		}
		if got := httpapi.ClientIP(r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "x, "+strings.Repeat("a", 100))
	if got := httpapi.ClientIP(r); len(got) != 64 {
		t.Errorf("not cut to 64: %d", len(got))
	}
}

func TestWithRequestID(t *testing.T) {
	var seen string
	h := httpapi.WithRequestID(httpapi.CORS(httpapi.Origins{List: []string{"https://console.ezghcloud.com"}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Request-Id")
		w.WriteHeader(http.StatusMethodNotAllowed)
	})))
	do := func(method, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/v1/organizations/x/widgets", nil)
		if id != "" {
			r.Header.Set("X-Request-Id", id)
		}
		r.Header.Set("Origin", "https://console.ezghcloud.com")
		r.Header.Set("Access-Control-Request-Method", "POST")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	// The caller's, when it's 1–128 printable ASCII characters: on a preflight and on what the
	// handler answers, which sees the same ID.
	if w := do("OPTIONS", "req-preflight"); w.Code != http.StatusNoContent || w.Header().Get("X-Request-Id") != "req-preflight" {
		t.Fatalf("preflight: %d %v", w.Code, w.Header())
	}
	if w := do("GET", "req-1"); w.Header().Get("X-Request-Id") != "req-1" || seen != "req-1" {
		t.Fatalf("got %q, handler saw %q", w.Header().Get("X-Request-Id"), seen)
	}
	// Otherwise a new UUIDv7, which the handler sees too.
	for _, bad := range []string{"", "has space", strings.Repeat("r", 129)} {
		w := do("GET", bad)
		id := w.Header().Get("X-Request-Id")
		if len(id) != 36 || id[14] != '7' || id == bad || seen != id {
			t.Fatalf("%q: answered %q, handler saw %q", bad, id, seen)
		}
	}
}

// The request's ID reaches its context, so calls made for it (org-api's) can send it on.
func TestRequestIDContext(t *testing.T) {
	if got := httpapi.RequestIDFrom(context.Background()); got != "" {
		t.Errorf("no ID: %q", got)
	}
	for _, header := range []string{"edge-7", "", strings.Repeat("x", 129)} {
		var inContext string
		h := httpapi.WithRequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inContext = httpapi.RequestIDFrom(r.Context())
		}))
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("X-Request-Id", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if inContext == "" || inContext != w.Header().Get("X-Request-Id") || (header == "edge-7" && inContext != "edge-7") {
			t.Errorf("X-Request-Id %q: context %q, response %q", header, inContext, w.Header().Get("X-Request-Id"))
		}
	}
}
