package apiclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error is a platform API's error answer: `{ "error": { "code", "message", … } }`
// (contracts/api-conventions.md#errors). Code is stable and what the CLI branches on; Message is
// for people.
type Error struct {
	// Service is the API that answered, for messages: its public name ("Organizations").
	Service string  `json:"-"`
	Status  int     `json:"status"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Issues  []Issue `json:"issues,omitempty"`
	// Quota is a quota_exceeded answer's quota (contracts/quotas.md#when-a-quota-is-exceeded).
	Quota *Quota `json:"quota,omitempty"`
	// Details are the error's other fields, as sent: audit-api's `line` and `column` for
	// invalid_query, `reason` for query_failed.
	Details map[string]json.RawMessage `json:"details,omitempty"`
	// RequestID is the X-Request-Id of the request that failed, for support.
	RequestID  string        `json:"requestId,omitempty"`
	RetryAfter time.Duration `json:"-"`
}

// Issue is one validation problem, at a dotted path.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Quota is what a quota_exceeded error says about the quota.
type Quota struct {
	ID                string  `json:"id"`
	Kind              string  `json:"kind"`
	Scope             string  `json:"scope"`
	ScopeResourceName *string `json:"scopeResourceName,omitempty"`
	Region            *string `json:"region,omitempty"`
	Limit             int64   `json:"limit"`
	Used              *int64  `json:"used,omitempty"`
	InFlight          *int64  `json:"inFlight,omitempty"`
	Window            *string `json:"window,omitempty"`
	PeriodStart       *string `json:"periodStart,omitempty"`
	ResetsAt          *string `json:"resetsAt,omitempty"`
}

func (e *Error) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Message + " (" + e.Code + ")"
}

// QuotaExceeded reports whether this is a quota refusal. Those are never retried: a quota means
// "you're at your limit", not "try again shortly", whatever the status or headers say.
func (e *Error) QuotaExceeded() bool { return e.Code == "quota_exceeded" }

// AsError returns err's *Error, if it is one.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// parseError reads an error answer. Besides the platform's shape it accepts better-auth's bare
// `{ code, message }` (auth-api), the Prometheus HTTP API's
// `{ "status": "error", "errorType", "error", "code" }` (obs-api's PromQL routes, whose code is
// the platform's), and makes something readable of anything else (a proxy's HTML page).
func parseError(service string, status int, header http.Header, body []byte) *Error {
	e := &Error{Service: service, Status: status, RequestID: header.Get("X-Request-Id")}
	e.RetryAfter = RetryAfter(header.Get("Retry-After"))
	var wrapped struct {
		Error map[string]json.RawMessage `json:"error"`
	}
	fields := map[string]json.RawMessage(nil)
	if json.Unmarshal(body, &wrapped) == nil && wrapped.Error != nil {
		fields = wrapped.Error
	} else {
		var bare map[string]json.RawMessage
		if json.Unmarshal(body, &bare) == nil {
			switch {
			case bare["code"] != nil && bare["message"] != nil:
				fields = bare
			case string(bare["status"]) == `"error"` && bare["errorType"] != nil:
				fields = map[string]json.RawMessage{}
				for k, v := range bare {
					switch k {
					case "status":
					case "error":
						fields["message"] = v
					default:
						fields[k] = v
					}
				}
			}
		}
	}
	for key, raw := range fields {
		switch key {
		case "code":
			_ = json.Unmarshal(raw, &e.Code)
		case "message":
			_ = json.Unmarshal(raw, &e.Message)
		case "issues":
			_ = json.Unmarshal(raw, &e.Issues)
		case "quota":
			var q Quota
			if json.Unmarshal(raw, &q) == nil {
				e.Quota = &q
			}
		default:
			if e.Details == nil {
				e.Details = map[string]json.RawMessage{}
			}
			e.Details[key] = raw
		}
	}
	if e.Code == "" {
		e.Code = codeForStatus(status)
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("%s answered %d %s", service, status, http.StatusText(status))
		if text := strings.TrimSpace(string(body)); text != "" && len(fields) == 0 && !strings.HasPrefix(text, "<") {
			if len(text) > 200 {
				text = text[:200] + "…"
			}
			e.Message += ": " + text
		}
	}
	return e
}

// codeForStatus names an answer that came without a platform error body (a gateway's 502, a
// better-auth answer without a code), so exit codes and retries still work.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusUnauthorized:
		return "unauthenticated"
	case http.StatusForbidden:
		return "access_denied"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusTooManyRequests:
		return "too_many_requests"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "unavailable"
	}
	return ""
}

// RetryAfter reads a Retry-After value: whole seconds, or an HTTP date. The contract says at least one
// second; zero or garbage is treated as one, so a retry never spins.
func RetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(seconds, 1)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t).Round(time.Second), time.Second)
	}
	return time.Second
}

// TransportError is a request that got no answer: DNS, TLS, a refused connection, a timeout.
type TransportError struct {
	Service string
	URL     string
	Err     error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("couldn't reach %s at %s: %v", e.Service, e.URL, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }
