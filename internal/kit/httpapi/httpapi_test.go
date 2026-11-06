package httpapi_test

import (
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConditionalWriteCORS(t *testing.T) {
	h := httpapi.CORS(httpapi.Origins{List: []string{"https://console.ezghcloud.com"}}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httptest.NewRequest("OPTIONS", "/", nil)
	r.Header.Set("Origin", "https://console.ezghcloud.com")
	r.Header.Set("Access-Control-Request-Method", "PATCH")
	r.Header.Set("Access-Control-Request-Headers", "If-Match")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "If-Match") || !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "ETag") {
		t.Fatal(w.Header())
	}
}

// CORSExcept answers no CORS for the requests it skips, preflights included, and CORS for the rest.
func TestCORSExcept(t *testing.T) {
	origin := "https://console.ezghcloud.com"
	reached := 0
	h := httpapi.CORSExcept(httpapi.Origins{List: []string{origin}}, func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/skip/") },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++; w.WriteHeader(http.StatusMethodNotAllowed) }))
	do := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("OPTIONS", "/skip/x"); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Vary") != "" || reached != 1 {
		t.Fatalf("skipped preflight: %d %v", w.Code, w.Header())
	}
	if w := do("OPTIONS", "/other"); w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Methods") == "" || reached != 1 {
		t.Fatalf("preflight: %d %v", w.Code, w.Header())
	}
}

// CORSAnyOrigin answers every origin with *, never with credentials, preflights included.
func TestCORSAnyOrigin(t *testing.T) {
	reached := 0
	h := httpapi.CORSAnyOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ }))
	for _, origin := range []string{"https://evil.example", "https://console.ezghcloud.com", ""} {
		r := httptest.NewRequest("OPTIONS", "/v1/receipts/ack", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Access-Control-Request-Method", "POST")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		hd := w.Header()
		if w.Code != http.StatusNoContent || hd.Get("Access-Control-Allow-Origin") != "*" || hd.Get("Access-Control-Allow-Credentials") != "" ||
			hd.Get("Access-Control-Allow-Methods") != "POST, OPTIONS" || hd.Get("Access-Control-Allow-Headers") != "Content-Type" || hd.Get("Access-Control-Max-Age") != "600" {
			t.Fatalf("%q preflight: %d %v", origin, w.Code, hd)
		}
		r = httptest.NewRequest("POST", "/v1/receipts/ack", nil)
		r.Header.Set("Origin", origin)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Header().Get("Access-Control-Allow-Origin") != "*" || w.Header().Get("Access-Control-Allow-Credentials") != "" || w.Header().Get("Access-Control-Expose-Headers") != "X-Request-Id, Retry-After" {
			t.Fatalf("%q POST: %v", origin, w.Header())
		}
	}
	if reached != 3 {
		t.Fatalf("reached %d times", reached)
	}
}
