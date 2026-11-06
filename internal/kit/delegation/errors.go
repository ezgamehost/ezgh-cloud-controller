package delegation

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// CustomerError is what an edge answers its customer for a receiver's answer
// (contracts/service-auth.md, "What the receiver returns to the edge";
// contracts/vectors/delegated-checks.json edgeMapping). status 0 is a timeout or an unreachable
// receiver. false: a domain answer the edge endpoint maps itself, as its API documents (a
// success, a 404 other than the organization's, a 409, 429 quota_exceeded, any other 4xx; an
// unmapped 4xx is the edge's bug, 500).
//
//	401 caller_unauthenticated   401 unauthenticated: the customer's credential ended mid-request
//	401 (the edge's credential)  503
//	403 access_denied            403 access_denied with the receiver's message
//	403 delegation_forbidden     503, logged: a misconfiguration
//	400 invalid_caller_reference 500: the edge's bug
//	404 Organization not found   404 not_found, "Organization not found"
//	429 too_many_requests        429 with the receiver's Retry-After
//	5xx, timeout                 503, the receiver's Retry-After on its 503, else 5
func CustomerError(status int, code, message string, retryAfter int) (*httpapi.Error, bool) {
	unavailable := func(cause string) *httpapi.Error {
		return httpapi.Unavailable("A service this request needs is unavailable; try again shortly", fmt.Errorf("delegated call: %s", cause))
	}
	switch {
	case status == 0:
		return unavailable("the receiver didn't answer"), true
	case status == http.StatusServiceUnavailable && retryAfter > 0:
		e := unavailable("the receiver answered 503")
		e.RetryAfter = min(retryAfter, 3600)
		return e, true
	case status >= 500:
		return unavailable(fmt.Sprintf("the receiver answered %d %s", status, code)), true
	case status == http.StatusUnauthorized && code == "caller_unauthenticated":
		return httpapi.Unauthenticated("Your credential is no longer valid; sign in again"), true
	case status == http.StatusUnauthorized:
		return unavailable("the receiver refused this service's credential (401 " + code + ")"), true
	case status == http.StatusForbidden && code == "access_denied":
		return &httpapi.Error{Status: http.StatusForbidden, Code: "access_denied", Message: message}, true
	case status == http.StatusForbidden && code == "delegation_forbidden":
		return unavailable("the receiver refused the delegation (403 delegation_forbidden): check its delegation allowlist and this service's credential namespace"), true
	case status == http.StatusBadRequest && code == "invalid_caller_reference":
		return httpapi.Internal(fmt.Errorf("delegated call: the receiver refused the caller reference: %s", message)), true
	case status == http.StatusNotFound && code == "not_found" && message == "Organization not found":
		return httpapi.NotFound("Organization"), true
	case status == http.StatusTooManyRequests && code == "too_many_requests":
		return httpapi.TooManyRequests("Too many requests; slow down and try again shortly", min(max(retryAfter, 1), 3600)), true
	}
	return nil, false
}

// ResponseError reads a receiver's non-2xx answer (its error body and Retry-After; it reads, but
// doesn't close, the body) and maps it with CustomerError; also the answer's code and message for
// an edge that maps domain answers itself (ok false).
func ResponseError(resp *http.Response) (e *httpapi.Error, ok bool, code, message string) {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = json.Unmarshal(raw, &body)
	retryAfter, _ := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After")))
	e, ok = CustomerError(resp.StatusCode, body.Error.Code, body.Error.Message, retryAfter)
	return e, ok, body.Error.Code, body.Error.Message
}
