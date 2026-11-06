package httpapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// The Prometheus HTTP API's error body (contracts/api-conventions.md, "Prometheus query surface"):
// errorType by status, the platform's code, extra fields, and Retry-After as WriteError.
func TestWritePrometheusError(t *testing.T) {
	cases := []struct {
		err       *httpapi.Error
		errorType string
		retry     string
		want      map[string]any
	}{
		{httpapi.InvalidRequest("Request validation failed", httpapi.Issue{Path: "query", Message: "is required"}, httpapi.Issue{Message: "a form body"}), "bad_data", "",
			map[string]any{"error": "Request validation failed: query: is required; a form body", "code": "invalid_request"}},
		{httpapi.Unauthenticated("Sign in"), "unauthorized", "", nil},
		{httpapi.AccessDenied("query metrics", true), "forbidden", "", nil},
		{httpapi.ForbiddenOrigin(), "forbidden", "", nil},
		{httpapi.NotFound("Organization"), "not_found", "", map[string]any{"error": "Organization not found", "code": "not_found"}},
		{httpapi.PayloadTooLarge(262144), "bad_data", "", nil},
		{&httpapi.Error{Status: 422, Code: "query_too_expensive", Message: "over", Extra: map[string]any{"budget": "samples"}}, "execution", "", map[string]any{"budget": "samples"}},
		{&httpapi.Error{Status: 422, Code: "query_too_expensive", Message: "slow", Extra: map[string]any{"budget": "time"}}, "timeout", "", map[string]any{"budget": "time"}},
		{httpapi.TooManyRequests("Slow down", 2), "unavailable", "2", nil},
		{httpapi.Unavailable("org-api is down", errors.New("dial")), "unavailable", "5", nil},
		{&httpapi.Error{Status: 503, Code: "source_unavailable", Message: "x"}, "unavailable", "1", nil},
		{httpapi.Internal(errors.New("bug")), "internal", "", map[string]any{"error": "Something went wrong", "code": "internal_error"}},
		{httpapi.Conflict("operation_in_progress", "busy"), "bad_data", "", nil},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		httpapi.WritePrometheusError(w, c.err)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if w.Code != c.err.Status || body["status"] != "error" || body["errorType"] != c.errorType || body["code"] != c.err.Code || w.Header().Get("Retry-After") != c.retry {
			t.Errorf("%s: %d %v Retry-After %q", c.err.Code, w.Code, body, w.Header().Get("Retry-After"))
		}
		if w.Header().Get("Content-Type") != "application/json; charset=utf-8" {
			t.Errorf("%s: %s", c.err.Code, w.Header().Get("Content-Type"))
		}
		for k, v := range c.want {
			if !reflect.DeepEqual(body[k], v) {
				t.Errorf("%s: %s = %v, want %v", c.err.Code, k, body[k], v)
			}
		}
		if _, nested := body["message"]; nested {
			t.Errorf("%s: the platform's fields leaked: %v", c.err.Code, body)
		}
	}
	// The platform's reserved fields win over extras of the same name.
	w := httptest.NewRecorder()
	httpapi.WritePrometheusError(w, &httpapi.Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: "m", Extra: map[string]any{"status": "success", "code": "x"}})
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "error" || body["code"] != "invalid_request" {
		t.Errorf("%v", body)
	}
}
