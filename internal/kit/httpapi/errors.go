// Package httpapi is the platform's HTTP conventions (contracts/api-conventions.md): the error
// body, JSON responses, request IDs, CORS and the Origin check for cookie writes, page limits,
// and reading request bodies strictly.
package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Issue is one validation problem: a dotted path into the request ("size", "tags.ezgh:owner"),
// "" for the whole body.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Error is an error the caller sees: { "error": { "code", "message", … } }. Codes are stable and
// snake_case; clients branch on them, never on the message.
type Error struct {
	Status  int
	Code    string
	Message string
	// Issues is set for invalid_request.
	Issues []Issue
	// Extra holds the other fields an error carries next to code and message: "quota",
	// "spendControl", or a product's own.
	Extra map[string]any
	// RetryAfter is sent as Retry-After, in whole seconds; 429s and 503s always get one.
	RetryAfter int
	// Cause is logged with the request ID, never sent (a 500's message is always "Something
	// went wrong").
	Cause error
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%d %s: %s: %v", e.Status, e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

// Body is the inner error object, as the response and a failed operation's "error" carry it.
func (e *Error) Body() map[string]any {
	body := map[string]any{"code": e.Code, "message": e.Message}
	if len(e.Issues) > 0 {
		body["issues"] = e.Issues
	}
	for k, v := range e.Extra {
		body[k] = v
	}
	return body
}

// The platform's codes. Products add their own snake_case codes for their own conditions.
func InvalidRequest(message string, issues ...Issue) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: message, Issues: issues}
}

// Invalid is invalid_request with the platform's usual message and every problem listed.
func Invalid(issues []Issue) *Error {
	return InvalidRequest("Request validation failed", issues...)
}

func InvalidCursor() *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_cursor", Message: "The cursor isn't valid for this list"}
}

func Unauthenticated(message string) *Error {
	return &Error{Status: http.StatusUnauthorized, Code: "unauthenticated", Message: message}
}

func AccessDenied(action string, apiKey bool) *Error {
	if apiKey {
		return &Error{Status: http.StatusForbidden, Code: "access_denied", Message: "This API key isn't allowed to " + action}
	}
	return &Error{Status: http.StatusForbidden, Code: "access_denied", Message: "You're not allowed to " + action}
}

// [being built] Delegated calls (contracts/api-conventions.md, "Error codes").

// CallerUnauthenticated is a receiver's answer when the customer's credential a caller reference
// names is no longer valid (or never was): 401, distinct from unauthenticated, which is always the
// calling service's own credential.
func CallerUnauthenticated() *Error {
	return &Error{Status: http.StatusUnauthorized, Code: "caller_unauthenticated", Message: "The caller's credential is no longer valid"}
}

// DelegationForbidden is a delegated call the receiver's allowlist doesn't permit, or a check
// org-api couldn't delegate (not_delegable): a misconfiguration, never the customer's. The cause is
// logged, never sent.
func DelegationForbidden(cause error) *Error {
	return &Error{Status: http.StatusForbidden, Code: "delegation_forbidden", Message: "This service may not make this call for the customer", Cause: cause}
}

// InvalidCallerReference is a delegated call whose X-Ezgh-Caller is missing, repeated or not
// canonical; issue says why, at path X-Ezgh-Caller.
func InvalidCallerReference(issue string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "invalid_caller_reference", Message: "The X-Ezgh-Caller header isn't a valid caller reference", Issues: []Issue{{Path: "X-Ezgh-Caller", Message: issue}}}
}

func ForbiddenOrigin() *Error {
	return &Error{Status: http.StatusForbidden, Code: "forbidden_origin", Message: "This origin isn't allowed to make changes"}
}

// NotFound is "<what> not found": "Organization not found", "Widget not found".
func NotFound(what string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "not_found", Message: what + " not found"}
}

func Conflict(code, message string) *Error {
	return &Error{Status: http.StatusConflict, Code: code, Message: message}
}

func PayloadTooLarge(limit int64) *Error {
	return &Error{Status: http.StatusRequestEntityTooLarge, Code: "payload_too_large", Message: fmt.Sprintf("The body is over %d bytes", limit)}
}

func TooManyRequests(message string, retryAfter int) *Error {
	return &Error{Status: http.StatusTooManyRequests, Code: "too_many_requests", Message: message, RetryAfter: retryAfter}
}

// Internal is a bug. The cause goes to the log under the request ID, never to the caller.
func Internal(cause error) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: "internal_error", Message: "Something went wrong", Cause: cause}
}

// Unavailable is a dependency the request needs being down (org-api, the database).
func Unavailable(message string, cause error) *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: message, RetryAfter: 5, Cause: cause}
}

// WriteError writes err's body and status, with Retry-After where it has one.
func WriteError(w http.ResponseWriter, e *Error) {
	noteErrorCode(w, e.Code)
	writeRetryAfter(w, e)
	WriteJSON(w, e.Status, map[string]any{"error": e.Body()})
}

// errorCoder is AccessLog's writer: an error response's code goes on the access line.
type errorCoder interface{ SetErrorCode(code string) }

func noteErrorCode(w http.ResponseWriter, code string) {
	for w != nil {
		if c, ok := w.(errorCoder); ok {
			c.SetErrorCode(code)
			return
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = u.Unwrap()
	}
}

func writeRetryAfter(w http.ResponseWriter, e *Error) {
	retry := e.RetryAfter
	// quota_exceeded never carries one (contracts/quotas.md): it isn't a failure to retry on a timer.
	// Nor does the OpenAI surface's insufficient_quota, its monthly limit.
	if retry < 1 && e.Code != "quota_exceeded" && e.Code != OpenAIInsufficientQuota && (e.Status == http.StatusTooManyRequests || e.Status == http.StatusServiceUnavailable) {
		retry = 1
	}
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
}

// Prometheus errorTypes (the Prometheus HTTP API's, plus unauthorized and forbidden, which clients
// treat as opaque strings).
const (
	PromBadData      = "bad_data"
	PromUnauthorized = "unauthorized"
	PromForbidden    = "forbidden"
	PromNotFound     = "not_found"
	PromExecution    = "execution"
	PromTimeout      = "timeout"
	PromUnavailable  = "unavailable"
	PromInternal     = "internal"
)

// PrometheusErrorType is the errorType a Prometheus client sees for e (contracts/api-conventions.md,
// "Prometheus query surface"): bad_data for 400 and 413, unauthorized, forbidden, not_found;
// execution for 422, or timeout when the time budget ran out (budget "time"); unavailable for 429
// and 503; internal for 500; otherwise bad_data for a 4xx and internal for a 5xx.
func PrometheusErrorType(e *Error) string {
	switch e.Status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return PromBadData
	case http.StatusUnauthorized:
		return PromUnauthorized
	case http.StatusForbidden:
		return PromForbidden
	case http.StatusNotFound:
		return PromNotFound
	case http.StatusUnprocessableEntity:
		if e.Extra["budget"] == "time" {
			return PromTimeout
		}
		return PromExecution
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return PromUnavailable
	}
	if e.Status >= 500 {
		return PromInternal
	}
	return PromBadData
}

// PrometheusBody is e as the Prometheus HTTP API's error body: {"status": "error", "errorType",
// "error": the message, "code": the platform's code}, plus e's extra fields ("budget"). A
// validation failure's issues are appended to the message, which is all Prometheus clients show.
func (e *Error) PrometheusBody() map[string]any {
	body := map[string]any{}
	for k, v := range e.Extra {
		body[k] = v
	}
	body["status"], body["errorType"], body["error"], body["code"] = "error", PrometheusErrorType(e), e.messageWithIssues(), e.Code
	return body
}

// messageWithIssues is the message with a validation failure's issues appended ("Request
// validation failed: dimensions: must be 32 to 1024; input: must not be empty"), for the
// third-party error bodies that have nowhere else to put them.
func (e *Error) messageWithIssues() string {
	if len(e.Issues) == 0 {
		return e.Message
	}
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.Message
		if issue.Path != "" {
			parts[i] = issue.Path + ": " + issue.Message
		}
	}
	return e.Message + ": " + strings.Join(parts, "; ")
}

// WritePrometheusError writes e as the Prometheus HTTP API's error body, with e's status and, as
// WriteError, Retry-After where it has one: the kit's failures on an endpoint with errorModel
// prometheus, and a product's own answers on those routes (obs-api's forwarder).
func WritePrometheusError(w http.ResponseWriter, e *Error) {
	noteErrorCode(w, e.Code)
	writeRetryAfter(w, e)
	WriteJSON(w, e.Status, e.PrometheusBody())
}

// OpenAI error types (the OpenAI API's error.type values; clients branch on them and on code).
const (
	OpenAIInvalidRequest    = "invalid_request_error"
	OpenAIAuthentication    = "authentication_error"
	OpenAIPermission        = "permission_error"
	OpenAINotFound          = "not_found_error"
	OpenAIRateLimit         = "rate_limit_error"
	OpenAIInsufficientQuota = "insufficient_quota"
	OpenAIAPI               = "api_error"
)

// OpenAIErrorType is the error.type an OpenAI client sees for e (contracts/api-conventions.md,
// "OpenAI-compatible surface"): authentication_error for 401, permission_error for 403,
// not_found_error for 404, rate_limit_error for 429 (insufficient_quota for the monthly limit's
// code of the same name), api_error for any 5xx, and invalid_request_error for every other 4xx.
func OpenAIErrorType(e *Error) string {
	switch {
	case e.Status == http.StatusUnauthorized:
		return OpenAIAuthentication
	case e.Status == http.StatusForbidden:
		return OpenAIPermission
	case e.Status == http.StatusNotFound:
		return OpenAINotFound
	case e.Status == http.StatusTooManyRequests && e.Code == OpenAIInsufficientQuota:
		return OpenAIInsufficientQuota
	case e.Status == http.StatusTooManyRequests:
		return OpenAIRateLimit
	case e.Status >= 500:
		return OpenAIAPI
	}
	return OpenAIInvalidRequest
}

// OpenAIBody is e as the OpenAI API's inner error object: {"message", "type", "param", "code":
// the platform's code}, plus e's extra fields, which never displace those four. param is
// Extra["param"], else the first issue's path, else null. A validation failure's issues are
// appended to the message, as PrometheusBody does.
func (e *Error) OpenAIBody() map[string]any {
	body := map[string]any{}
	for k, v := range e.Extra {
		body[k] = v
	}
	var param any
	if p, ok := e.Extra["param"]; ok {
		param = p
	} else if len(e.Issues) > 0 && e.Issues[0].Path != "" {
		param = e.Issues[0].Path
	}
	body["message"], body["type"], body["param"], body["code"] = e.messageWithIssues(), OpenAIErrorType(e), param, e.Code
	return body
}

// WriteOpenAIError writes e as {"error": OpenAIBody()} with e's status and, as WriteError,
// Retry-After where it has one: the kit's failures on an endpoint with errorModel openai. The
// monthly limit's insufficient_quota also gets x-should-retry: false, which OpenAI's SDKs honor
// instead of retrying a 429. A handler returns its errors to the kit rather than writing them with
// this: the kit writes them the same way and records them in Trails, which matters most on a
// refusals-only endpoint, where a failure the handler writes itself is never recorded.
func WriteOpenAIError(w http.ResponseWriter, e *Error) {
	noteErrorCode(w, e.Code)
	writeRetryAfter(w, e)
	if e.Code == OpenAIInsufficientQuota {
		w.Header().Set("x-should-retry", "false")
	}
	WriteJSON(w, e.Status, map[string]any{"error": e.OpenAIBody()})
}

// WriteJSON writes body as JSON (no HTML escaping: responses aren't embedded in HTML).
func WriteJSON(w http.ResponseWriter, status int, body any) {
	b, err := Marshal(body)
	if err != nil {
		status = http.StatusInternalServerError
		b = []byte(`{"error":{"code":"internal_error","message":"Something went wrong"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// Marshal is json.Marshal without HTML escaping and without the trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
