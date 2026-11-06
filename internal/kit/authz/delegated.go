package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// [being built] Delegated checks (contracts/identity-and-authz.md, "Delegated checks";
// contracts/service-auth.md, "Delegated calls"): a receiver asks org-api's POST
// /internal/v1/permissions/delegated whether the customer an X-Ezgh-Caller reference names may do
// its checks. Like every check, nothing is cached or reused.

// DelegatedRequest is one delegated check.
type DelegatedRequest struct {
	// Caller is the calling namespace: the namespace the receiver's own credential table binds the
	// calling credential to, never a value from the request.
	Caller string
	// Reference is the X-Ezgh-Caller header exactly as received.
	Reference string
	// ForwardedFor is the edge's X-Forwarded-For, unchanged.
	ForwardedFor string
	// Checks are 1 to MaxBatchChecks checks, asked in one call: a delegated check is never split,
	// since a split would resolve the customer's credential twice.
	Checks []BatchCheck
}

// CheckDelegated asks org-api's delegated check with the receiver's own ok_ (ServiceCredential),
// the edge's X-Forwarded-For and the request's X-Request-Id, and maps org-api's answer to the
// receiver's (vectors/delegated-checks.json receiverMapping):
//
//	200                       the answer, results as org-api gave them: the caller maps a false
//	                          result to 403 access_denied and a null one to 403
//	                          delegation_forbidden. A 200 without credential, or whose results
//	                          don't answer the checks one for one, is 503
//	401 caller_unauthenticated  401 caller_unauthenticated
//	401, 403 forbidden        503 unavailable: the receiver's own ok_ (logged through Cause)
//	400 at reference          400 invalid_caller_reference
//	400 otherwise             500 internal_error: the receiver kit's bug
//	403 delegation_forbidden  403 delegation_forbidden
//	404 not_found             404 not_found, "Organization not found"; another 404 (no such
//	                          route: an older org-api) is 503
//	429                       429 too_many_requests with org-api's Retry-After
//	anything else             503 unavailable (Retry-After 5)
//
// No checks, or more than MaxBatchChecks, is a bug (500) without a call.
func (c *Client) CheckDelegated(ctx context.Context, r DelegatedRequest) (*BatchAnswer, *httpapi.Error) {
	if len(r.Checks) == 0 || len(r.Checks) > MaxBatchChecks {
		return nil, httpapi.Internal(fmt.Errorf("authz.CheckDelegated: %d checks; a delegated check asks 1 to %d in one call", len(r.Checks), MaxBatchChecks))
	}
	unavailable := func(cause error) *httpapi.Error {
		return httpapi.Unavailable("Authorization is unavailable; try again shortly", cause)
	}
	if c.ServiceCredential == "" {
		return nil, unavailable(errors.New("authz.Client.ServiceCredential is empty: CheckDelegated needs the receiver's org-api ok_ credential (ORG_API_SERVICE_CREDENTIAL)"))
	}
	payload, err := json.Marshal(struct {
		Caller    string       `json:"caller"`
		Reference string       `json:"reference"`
		Checks    []BatchCheck `json:"checks"`
	}{r.Caller, r.Reference, r.Checks})
	if err != nil {
		return nil, httpapi.Internal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/internal/v1/permissions/delegated", bytes.NewReader(payload))
	if err != nil {
		return nil, httpapi.Internal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.ServiceCredential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if r.ForwardedFor != "" {
		req.Header.Set("X-Forwarded-For", r.ForwardedFor)
	}
	setRequestID(ctx, req)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, unavailable(fmt.Errorf("org-api delegated check: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	code, issues := errorOf(raw)
	switch resp.StatusCode {
	case http.StatusOK:
		answer, err := decodeBatchAnswer(raw, r.Checks)
		if err == nil && answer.Credential == nil {
			err = errors.New("no credential")
		}
		if err != nil {
			return nil, unavailable(fmt.Errorf("org-api delegated check: unexpected body (%v): %s", err, truncate(raw)))
		}
		return answer, nil
	case http.StatusBadRequest:
		for _, issue := range issues {
			if issue.Path == "reference" || strings.HasPrefix(issue.Path, "reference.") {
				return nil, httpapi.InvalidCallerReference(issue.Message)
			}
		}
		return nil, httpapi.Internal(fmt.Errorf("org-api refused the delegated check as malformed: %s", truncate(raw)))
	case http.StatusUnauthorized:
		if code == "caller_unauthenticated" {
			return nil, httpapi.CallerUnauthenticated()
		}
		return nil, unavailable(errors.New("org-api answered 401 to the delegated check: this service's org-api ok_ credential is missing, bad, expired or revoked"))
	case http.StatusForbidden:
		if code == "delegation_forbidden" {
			return nil, httpapi.DelegationForbidden(fmt.Errorf("org-api refused delegation from %s: %s", r.Caller, truncate(raw)))
		}
		return nil, unavailable(errors.New("org-api answered 403 to the delegated check: this namespace's ok_ credential isn't allowed permissions.delegated (declare it in the manifest's orgApi.operations)"))
	case http.StatusNotFound:
		if code == "not_found" {
			return nil, httpapi.NotFound("Organization")
		}
		// No such route: an org-api that predates delegated checks, never the customer's 404.
		return nil, unavailable(fmt.Errorf("org-api answered 404 %q to the delegated check: is it deployed with permissions.delegated?", code))
	case http.StatusTooManyRequests:
		return nil, httpapi.TooManyRequests("Too many requests; slow down and try again shortly", retryAfter(resp.Header))
	default:
		return nil, unavailable(fmt.Errorf("org-api delegated check answered %d", resp.StatusCode))
	}
}

// errorOf reads an error body's code and issues; empty for anything else.
func errorOf(raw []byte) (string, []httpapi.Issue) {
	var body struct {
		Error struct {
			Code   string          `json:"code"`
			Issues []httpapi.Issue `json:"issues"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return "", nil
	}
	return body.Error.Code, body.Error.Issues
}
