package httpapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestOpenAIBody(t *testing.T) {
	cases := []struct {
		name       string
		e          *Error
		status     int
		typ, code  string
		param      any
		retryAfter string
	}{
		{"validation", Invalid([]Issue{{Path: "input", Message: "must be a string or an array of strings"}}), 400, "invalid_request_error", "invalid_request", "input", ""},
		{"param extra", &Error{Status: 400, Code: "input_too_long", Message: "Input 3 is longer than 8192 tokens", Extra: map[string]any{"param": "input[3]"}}, 400, "invalid_request_error", "input_too_long", "input[3]", ""},
		{"whole-body issue", InvalidRequest("The body isn't JSON", Issue{Path: "", Message: "unexpected end of input"}), 400, "invalid_request_error", "invalid_request", nil, ""},
		{"unauthenticated", Unauthenticated("A valid credential is required"), 401, "authentication_error", "unauthenticated", nil, ""},
		{"denied", AccessDenied("inference.models.invoke", true), 403, "permission_error", "access_denied", nil, ""},
		{"origin", ForbiddenOrigin(), 403, "permission_error", "forbidden_origin", nil, ""},
		{"not found", NotFound("Project"), 404, "not_found_error", "not_found", nil, ""},
		{"retired", &Error{Status: 410, Code: "model_retired", Message: "qwen3-embedding-0.6b:0.9.0 is retired"}, 410, "invalid_request_error", "model_retired", nil, ""},
		{"too large", PayloadTooLarge(1 << 20), 413, "invalid_request_error", "payload_too_large", nil, ""},
		{"rate", TooManyRequests("Slow down", 3), 429, "rate_limit_error", "too_many_requests", nil, "3"},
		{"monthly", &Error{Status: 429, Code: "insufficient_quota", Message: "This organization used its monthly tokens"}, 429, "insufficient_quota", "insufficient_quota", nil, ""},
		{"bad gateway", &Error{Status: 502, Code: "inference_failed", Message: "The model backend failed"}, 502, "api_error", "inference_failed", nil, ""},
		{"unavailable", Unavailable("org-api is unavailable", nil), 503, "api_error", "unavailable", nil, "5"},
		{"timeout", &Error{Status: 504, Code: "inference_timeout", Message: "The request took longer than 60 seconds"}, 504, "api_error", "inference_timeout", nil, ""},
		{"internal", Internal(nil), 500, "api_error", "internal_error", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteOpenAIError(w, tc.e)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
			if got := w.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After %q, want %q", got, tc.retryAfter)
			}
			var body struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error["type"] != tc.typ || body.Error["code"] != tc.code || body.Error["param"] != tc.param {
				t.Fatalf("got %v", body.Error)
			}
			if _, present := body.Error["param"]; !present {
				t.Fatalf("param must be present, null when there's none: %v", body.Error)
			}
			if _, ok := body.Error["message"].(string); !ok {
				t.Fatalf("no message: %v", body.Error)
			}
			if _, ok := body.Error["issues"]; ok {
				t.Fatalf("issues go into the message, not a field of their own: %v", body.Error)
			}
		})
	}
}

func TestOpenAIBodyAppendsIssuesToMessage(t *testing.T) {
	e := Invalid([]Issue{{Path: "dimensions", Message: "must be 32 to 1024"}, {Path: "input", Message: "must not be empty"}})
	got := e.OpenAIBody()["message"]
	want := "Request validation failed: dimensions: must be 32 to 1024; input: must not be empty"
	if got != want {
		t.Fatalf("message %q, want %q", got, want)
	}
}

// A product's extra fields ride along, but never displace OpenAI's four.
func TestOpenAIBodyKeepsExtraFieldsBehindItsOwn(t *testing.T) {
	e := &Error{Status: 400, Code: "dimensions_out_of_range", Message: "dimensions must be 32 to 1024", Extra: map[string]any{"param": "dimensions", "type": "smuggled", "code": "smuggled", "min": 32}}
	body := e.OpenAIBody()
	if body["type"] != "invalid_request_error" || body["code"] != "dimensions_out_of_range" || body["param"] != "dimensions" || body["min"] != 32 {
		t.Fatalf("%v", body)
	}
}

// OpenAI's SDKs retry 429s unless told not to: the monthly limit says x-should-retry: false, and
// nothing else does.
func TestOpenAIInsufficientQuotaIsNotRetried(t *testing.T) {
	w := httptest.NewRecorder()
	WriteOpenAIError(w, &Error{Status: 429, Code: OpenAIInsufficientQuota, Message: "This organization used its monthly tokens"})
	if w.Header().Get("x-should-retry") != "false" || w.Header().Get("Retry-After") != "" {
		t.Fatalf("%v", w.Header())
	}
	for _, e := range []*Error{TooManyRequests("Slow down", 3), Unavailable("down", nil), Invalid(nil)} {
		w := httptest.NewRecorder()
		WriteOpenAIError(w, e)
		if _, ok := w.Header()["X-Should-Retry"]; ok {
			t.Fatalf("%s: %v", e.Code, w.Header())
		}
	}
}
