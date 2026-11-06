package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

func TestWithSecurityHeaders(t *testing.T) {
	h := httpapi.WithSecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteError(w, httpapi.NotFound("Route"))
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/nope", nil))
	if w.Code != 404 || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}
