package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
)

// MaxBodyBytes is a request body's limit unless an endpoint says otherwise (1 MiB).
const MaxBodyBytes = 1 << 20

// RequestID is the request's X-Request-Id when it's 1–128 printable ASCII characters, otherwise a
// new UUIDv7. It's echoed on the response, logged, and recorded as the Trails event's requestId.
func RequestID(r *http.Request) string { return NormalizeRequestID(r.Header.Get("X-Request-Id")) }

// NormalizeRequestID is RequestID for a caller's ID from anywhere (gRPC metadata, a queue).
func NormalizeRequestID(id string) string {
	if len(id) >= 1 && len(id) <= 128 && printableASCII(id) {
		return id
	}
	return ids.NewV7()
}

type requestIDKey struct{}

// ContextWithRequestID is ctx carrying a request's ID, so calls made for the request (org-api's
// permission checks, a delegated call to another product) send it on as X-Request-Id.
func ContextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom is the request ID ctx carries (WithRequestID and AccessLog put it there), or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// WithRequestID gives every response the request's ID (RequestID), whatever answers it: a route,
// a 404 or 405 from the router, or a CORS preflight. Wrap the whole handler in it, outermost. The
// request's X-Request-Id is replaced with the ID, so everything after it (the route's Call, its
// Trails event and log lines) sees the same one, and its context carries it (RequestIDFrom).
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := RequestID(r)
		if r.Header.Get("X-Request-Id") != id {
			r = r.Clone(r.Context())
			r.Header.Set("X-Request-Id", id)
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(ContextWithRequestID(r.Context(), id)))
	})
}

func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// ReadBody reads the whole body, up to limit bytes; more is 413.
func ReadBody(r *http.Request, limit int64) ([]byte, *Error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, InvalidRequest("The body couldn't be read")
	}
	if int64(len(b)) > limit {
		return nil, PayloadTooLarge(limit)
	}
	return b, nil
}

// DecodeObject reads a JSON object body, keeping numbers exact. An empty body is an empty object
// only when allowEmpty.
func DecodeObject(body []byte, allowEmpty bool) (map[string]any, *Error) {
	if len(bytes.TrimSpace(body)) == 0 {
		if allowEmpty {
			return map[string]any{}, nil
		}
		return nil, InvalidRequest("Expected a JSON object body", Issue{Path: "", Message: "is required"})
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return nil, InvalidRequest("The body isn't valid JSON", Issue{Path: "", Message: "must be a JSON object"})
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, InvalidRequest("Expected a JSON object body", Issue{Path: "", Message: "must be a JSON object"})
	}
	return obj, nil
}

// Fields validates a JSON object field by field, collecting every problem, so one response lists
// them all. Unknown fields are refused: a typo ("tag" for "tags") never silently does nothing.
type Fields struct {
	obj    map[string]any
	known  map[string]bool
	Issues []Issue
}

// NewFields starts validating obj.
func NewFields(obj map[string]any) *Fields {
	return &Fields{obj: obj, known: map[string]bool{}}
}

// Add records a problem.
func (f *Fields) Add(path, message string) {
	f.Issues = append(f.Issues, Issue{Path: path, Message: message})
}

// Has reports whether the object has key (null included).
func (f *Fields) Has(key string) bool {
	f.known[key] = true
	_, ok := f.obj[key]
	return ok
}

// String reads a string field. required: absent or null is an issue.
func (f *Fields) String(key string, required bool) (string, bool) {
	f.known[key] = true
	v, ok := f.obj[key]
	if !ok || v == nil {
		if required {
			f.Add(key, "is required")
		}
		return "", false
	}
	s, isString := v.(string)
	if !isString {
		f.Add(key, "must be a string")
		return "", false
	}
	return s, true
}

// StringMap reads an object of strings (tags).
func (f *Fields) StringMap(key string) (map[string]string, bool) {
	f.known[key] = true
	v, ok := f.obj[key]
	if !ok || v == nil {
		return nil, false
	}
	obj, isObject := v.(map[string]any)
	if !isObject {
		f.Add(key, "must be an object of strings")
		return nil, false
	}
	out := make(map[string]string, len(obj))
	good := true
	for k, item := range obj {
		s, isString := item.(string)
		if !isString {
			f.Add(key+"."+k, "must be a string")
			good = false
			continue
		}
		out[k] = s
	}
	return out, good
}

// Finish adds an issue for every field nobody asked about, and returns the invalid_request error
// when there's any issue.
func (f *Fields) Finish() *Error {
	for key := range f.obj {
		if !f.known[key] {
			f.Add(key, "isn't a field of this request")
		}
	}
	if len(f.Issues) == 0 {
		return nil
	}
	return Invalid(f.Issues)
}

// UTF16Len is a string's length in UTF-16 code units, as JavaScript and the platform's limits
// count them.
func UTF16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// Page limits.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Limit reads ?limit: 1–100, default 20. Out of range is 400, never a clamp, so a client never
// silently gets fewer rows than it asked for.
func Limit(q url.Values) (int, *Error) {
	raw, ok := q["limit"]
	if !ok {
		return DefaultLimit, nil
	}
	n, err := strconv.Atoi(raw[0])
	if err != nil || n < 1 || n > MaxLimit || len(raw) > 1 {
		return 0, InvalidRequest("Request validation failed", Issue{Path: "limit", Message: fmt.Sprintf("must be an integer from 1 to %d", MaxLimit)})
	}
	return n, nil
}

// ClientIP is the client's address (contracts/api-conventions.md, "Client IP"): the RIGHTMOST
// X-Forwarded-For entry, which the Gateway appended (anything to its left came from the client,
// which can put anything there), else the peer address; at most 64 characters. Several
// X-Forwarded-For headers are one list, in order.
func ClientIP(r *http.Request) string {
	ip := ""
	for _, value := range r.Header.Values("X-Forwarded-For") {
		for _, entry := range strings.Split(value, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				ip = entry
			}
		}
	}
	if ip == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip = host
	}
	if len(ip) > 64 {
		ip = ip[:64]
	}
	return ip
}

// ErrorOf turns any error into the caller's error: *Error as is, anything else a 500.
func ErrorOf(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Internal(err)
}
