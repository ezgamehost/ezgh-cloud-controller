package ezghclient

import (
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// PermissionError is a 403: the cluster's Bot lacks Action (empty when the answer didn't name one).
type PermissionError struct {
	Action, Code, Message string
	Status                int
}

func (e *PermissionError) Error() string { return e.Message + " (" + e.Code + ")" }

// The kit's access_denied messages end with the action that was checked: "This API key isn't
// allowed to lb.loadBalancers.create". That's the action that failed, which may not be the route's
// own (a listener create checks certificates.certificates.use as the caller too).
var deniedAction = regexp.MustCompile(`allowed to ([a-z][A-Za-z0-9]*(?:\.[A-Za-z0-9]+)+)\s*\.?$`)

// AsPermission reports whether err is a 403, and which action it names.
func AsPermission(err error) (*PermissionError, bool) {
	e, ok := apiclient.AsError(err)
	if !ok || e.Status != http.StatusForbidden {
		return nil, false
	}
	p := &PermissionError{Code: e.Code, Message: e.Message, Status: e.Status}
	if m := deniedAction.FindStringSubmatch(e.Message); m != nil {
		p.Action = m[1]
	}
	return p, true
}

// AsQuota reports whether err is a quota refusal, with its code and message.
func AsQuota(err error) (code, message string, ok bool) {
	e, isAPI := apiclient.AsError(err)
	if !isAPI || !e.QuotaExceeded() {
		return "", "", false
	}
	return e.Code, e.Message, true
}

// AsCredentials reports whether err is a key that couldn't be had.
func AsCredentials(err error) (*CredentialsError, bool) {
	var e *CredentialsError
	ok := errors.As(err, &e)
	return e, ok
}

// IsNotFound reports whether err is a 404 answer.
func IsNotFound(err error) bool {
	e, ok := apiclient.AsError(err)
	return ok && e.Status == http.StatusNotFound
}

// Code is an API error's code, or "".
func Code(err error) string {
	if e, ok := apiclient.AsError(err); ok {
		return e.Code
	}
	return ""
}

// RetryAfter is how long err's answer asked the caller to wait, or 0.
func RetryAfter(err error) time.Duration {
	if e, ok := apiclient.AsError(err); ok {
		return e.RetryAfter
	}
	if e, ok := AsCredentials(err); ok {
		return e.RetryAfter
	}
	return 0
}
