package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// [being built] Batch checks (contracts/identity-and-authz.md, "Batch checks"): one request's
// answers about several resources from org-api's POST /internal/v1/permissions/batch, asked with
// the product's own ok_ credential and the customer's credential in the body. Like Check, nothing
// is cached or reused: every call asks again.

// MaxBatchChecks is the most checks one org-api call carries; CheckBatch splits longer lists.
const MaxBatchChecks = 50

// BatchCheck is one question: may the caller do Action on Resource (a resource name; the
// organization is ezgh::org_…).
type BatchCheck struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
}

// Per-check error codes. org-api's (the product's bugs, logged): an action the catalog doesn't
// have, a resource that isn't a resource name, another organization's resource. More may come.
// The kit's, for the checks of a call that failed: rate limited (with RetryAfterSeconds) or
// unavailable.
const (
	BatchUnknownAction   = "unknown_action"
	BatchInvalidResource = "invalid_resource"
	BatchForeignResource = "foreign_resource"
	BatchRateLimited     = "rate_limited"
	BatchUnavailable     = "unavailable"
)

// BatchError is why a check has no answer.
type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	// RetryAfterSeconds is set with BatchRateLimited: the call's Retry-After, 1-3600.
	RetryAfterSeconds int `json:"retryAfterSeconds,omitempty"`
}

// BatchResult is one check's answer, in request order, echoing its check. Decision is true, false
// or nil; nil exactly when Error says why. Only Allowed() passes a check.
type BatchResult struct {
	Action   string      `json:"action"`
	Resource string      `json:"resource"`
	Decision *bool       `json:"allowed"`
	Error    *BatchError `json:"error,omitempty"`
}

// Allowed reports whether the check passes: only an answer of true. False and no answer (an
// error, org-api's or the kit's) are both not allowed: never fail open.
func (r BatchResult) Allowed() bool { return r.Decision != nil && *r.Decision }

// BatchAnswer is the whole answer: who the caller is, exactly as Check's Permissions names them
// (UserIdentity is recorded in Trails byte for byte), and one result per check, in order.
type BatchAnswer struct {
	Organization Organization    `json:"organization"`
	Principal    Principal       `json:"principal"`
	APIKeyID     *string         `json:"apiKeyId"`
	UserIdentity json.RawMessage `json:"userIdentity"`
	Results      []BatchResult   `json:"results"`
	// [being built] Credential is the customer credential org-api resolved, which an edge names in
	// a caller reference (contracts/identity-and-authz.md, "Delegated checks"); nil when it isn't
	// delegable or org-api predates delegated calls.
	Credential *CredentialRef `json:"credential,omitempty"`
}

// CredentialRef names a customer credential by kind and ID, never its secret. Kind is session,
// federation, oauth, apiKey or ssoOauth; ID is 1-64 of [A-Za-z0-9_-].
type CredentialRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// CheckBatch asks org-api about every check at once, as the caller: POST
// /internal/v1/permissions/batch with Authorization: Bearer <ServiceCredential>, the caller's
// credential in the body (their Authorization header's value, or their better-auth cookies; never
// both, and never in this call's own Authorization), their X-Forwarded-For unchanged and, with an
// EZGH SSO resource token only, requestUrl built as Check builds it (a path with no canonical form
// is 401 without a call). Zero checks is a bug (500) without a call.
//
// More than MaxBatchChecks are split in order into calls of at most MaxBatchChecks, sent
// concurrently; their results come back merged in request order. Each call's answer:
//
//	200            its results, which must answer its checks one for one, in order, echoing each
//	               (anything else is that call unavailable)
//	401, 404       the caller isn't signed in, or isn't in the organization
//	400            a malformed body: a bug
//	403            the product's namespace hasn't declared permissions.batch: unavailable, logged
//	               (the error's Cause), never allowed
//	429            rate limited, with org-api's Retry-After (1-3600)
//	anything else  unavailable (5xx, a timeout, unreachable)
//
// combine into the error or the answer:
//
//   - any 401 is 401 unauthenticated; else any 404 is 404 "Organization not found"; else any 400 is
//     500 internal_error;
//   - no call succeeded: 429 too_many_requests (the longest Retry-After) when any was rate limited,
//     else 503 unavailable;
//   - otherwise the answer, naming the caller from the first successful call, and each check of a
//     failed call answered nil with BatchError{BatchRateLimited, RetryAfterSeconds} or
//     BatchError{BatchUnavailable}: those fail closed, and the product leaves them out or answers
//     503.
func (c *Client) CheckBatch(ctx context.Context, cred Credential, orgID string, checks []BatchCheck) (*BatchAnswer, *httpapi.Error) {
	if len(checks) == 0 {
		return nil, httpapi.Internal(errors.New("authz.CheckBatch: no checks"))
	}
	if c.ServiceCredential == "" {
		return nil, httpapi.Unavailable("Authorization is unavailable; try again shortly", errors.New("authz.Client.ServiceCredential is empty: CheckBatch needs the product's org-api ok_ credential (ORG_API_SERVICE_CREDENTIAL)"))
	}
	body := batchRequest{OrganizationID: orgID}
	switch cred.Header {
	case "Authorization":
		body.Credential.Authorization = cred.Value
	case "Cookie":
		body.Credential.Cookie = cred.Value
	default:
		return nil, httpapi.Internal(fmt.Errorf("authz.CheckBatch: a credential from CredentialFrom, not %q", cred.Header))
	}
	if cred.IsSSO() {
		requestURL, ok, err := c.requestURL(cred)
		if err != nil {
			return nil, httpapi.Internal(err)
		}
		if !ok {
			return nil, httpapi.Unauthenticated("This access token isn't valid for this URL")
		}
		body.RequestURL = requestURL
	}

	var calls []batchCall
	for start := 0; start < len(checks); start += MaxBatchChecks {
		calls = append(calls, batchCall{checks: checks[start:min(start+MaxBatchChecks, len(checks))]})
	}
	var wg sync.WaitGroup
	for i := range calls {
		wg.Add(1)
		go func(call *batchCall) {
			defer wg.Done()
			request := body
			request.Checks = call.checks
			c.batchCall(ctx, cred, request, call)
		}(&calls[i])
	}
	wg.Wait()
	answer, failure := combineBatch(calls)
	if answer != nil && c.Logger != nil {
		// The answer stands, so no error carries these: log each failed call's cause (never a
		// credential: org-api's status, or its body, truncated).
		for _, call := range calls {
			if call.outcome != callOK {
				c.Logger.Warn("org-api permissions batch: a call failed; its checks fail closed", "checks", len(call.checks), "error", fmt.Sprint(call.cause))
			}
		}
	}
	return answer, failure
}

type batchRequest struct {
	OrganizationID string `json:"organizationId"`
	Credential     struct {
		Authorization string `json:"authorization,omitempty"`
		Cookie        string `json:"cookie,omitempty"`
	} `json:"credential"`
	RequestURL string       `json:"requestUrl,omitempty"`
	Checks     []BatchCheck `json:"checks"`
}

// The outcome of one call to org-api.
const (
	callOK = iota
	callUnauthenticated
	callNotFound
	callBadRequest
	callRateLimited
	callUnavailable
)

type batchCall struct {
	checks     []BatchCheck
	outcome    int
	answer     *BatchAnswer
	retryAfter int
	cause      error
}

func (c *Client) batchCall(ctx context.Context, cred Credential, body batchRequest, call *batchCall) {
	call.outcome = callUnavailable
	payload, err := json.Marshal(body)
	if err != nil {
		call.cause = err
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/internal/v1/permissions/batch", bytes.NewReader(payload))
	if err != nil {
		call.cause = err
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.ServiceCredential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cred.ForwardedFor != "" {
		req.Header.Set("X-Forwarded-For", cred.ForwardedFor)
	}
	setRequestID(ctx, req)
	resp, err := c.http().Do(req)
	if err != nil {
		call.cause = fmt.Errorf("org-api permissions batch: %w", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		answer, err := decodeBatchAnswer(raw, body.Checks)
		if err != nil {
			call.cause = fmt.Errorf("org-api permissions batch: unexpected body (%v): %s", err, truncate(raw))
			return
		}
		call.outcome, call.answer = callOK, answer
	case http.StatusUnauthorized:
		call.outcome = callUnauthenticated
	case http.StatusNotFound:
		call.outcome = callNotFound
	case http.StatusBadRequest:
		call.outcome = callBadRequest
		call.cause = fmt.Errorf("org-api refused the permissions batch as malformed: %s", truncate(raw))
	case http.StatusTooManyRequests:
		call.outcome, call.retryAfter = callRateLimited, retryAfter(resp.Header)
		call.cause = fmt.Errorf("org-api permissions batch answered 429 (Retry-After %d)", call.retryAfter)
	case http.StatusForbidden:
		call.cause = errors.New("org-api answered 403 to the permissions batch: this namespace's ok_ credential isn't allowed permissions.batch (declare it in the manifest's orgApi.operations)")
	default:
		call.cause = fmt.Errorf("org-api permissions batch answered %d", resp.StatusCode)
	}
}

// decodeBatchAnswer reads a 200 strictly enough never to misattribute an answer: it names the
// caller, and answers exactly checks, one for one, in order, echoing each; allowed is true, false,
// or null with an error code. Fields it doesn't know are ignored.
func decodeBatchAnswer(raw []byte, checks []BatchCheck) (*BatchAnswer, error) {
	var wire struct {
		Organization Organization    `json:"organization"`
		Principal    Principal       `json:"principal"`
		APIKeyID     *string         `json:"apiKeyId"`
		UserIdentity json.RawMessage `json:"userIdentity"`
		Credential   *CredentialRef  `json:"credential"`
		Results      []struct {
			Action   *string         `json:"action"`
			Resource *string         `json:"resource"`
			Allowed  json.RawMessage `json:"allowed"`
			Error    *BatchError     `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	if wire.Organization.ID == "" || len(wire.UserIdentity) == 0 || string(wire.UserIdentity) == "null" {
		return nil, errors.New("no organization or userIdentity")
	}
	if len(wire.Results) != len(checks) {
		return nil, fmt.Errorf("%d results for %d checks", len(wire.Results), len(checks))
	}
	if wire.Credential != nil && (wire.Credential.Kind == "" || wire.Credential.ID == "") {
		return nil, errors.New("a credential without kind or id")
	}
	answer := &BatchAnswer{Organization: wire.Organization, Principal: wire.Principal, APIKeyID: wire.APIKeyID, UserIdentity: wire.UserIdentity, Credential: wire.Credential, Results: make([]BatchResult, len(checks))}
	for i, r := range wire.Results {
		if r.Action == nil || r.Resource == nil || *r.Action != checks[i].Action || *r.Resource != checks[i].Resource {
			return nil, fmt.Errorf("result %d doesn't answer its check", i)
		}
		result := BatchResult{Action: *r.Action, Resource: *r.Resource}
		switch string(bytes.TrimSpace(r.Allowed)) {
		case "true", "false":
			decision := string(bytes.TrimSpace(r.Allowed)) == "true"
			if r.Error != nil {
				return nil, fmt.Errorf("result %d is answered and has an error", i)
			}
			result.Decision = &decision
		case "null":
			if r.Error == nil || r.Error.Code == "" {
				return nil, fmt.Errorf("result %d has no answer and no error code", i)
			}
			result.Error = &BatchError{Code: r.Error.Code, Message: r.Error.Message}
		default:
			return nil, fmt.Errorf("result %d: allowed is true, false or null", i)
		}
		answer.Results[i] = result
	}
	return answer, nil
}

func combineBatch(calls []batchCall) (*BatchAnswer, *httpapi.Error) {
	first := func(outcome int) *batchCall {
		for i := range calls {
			if calls[i].outcome == outcome {
				return &calls[i]
			}
		}
		return nil
	}
	if first(callUnauthenticated) != nil {
		return nil, httpapi.Unauthenticated("Sign in, or send a valid API key or access token")
	}
	if first(callNotFound) != nil {
		return nil, httpapi.NotFound("Organization")
	}
	if bad := first(callBadRequest); bad != nil {
		return nil, httpapi.Internal(bad.cause)
	}
	answered := first(callOK)
	if answered == nil {
		longest := 0
		for _, call := range calls {
			if call.outcome == callRateLimited {
				longest = max(longest, call.retryAfter)
			}
		}
		if longest > 0 {
			return nil, httpapi.TooManyRequests("Too many requests; slow down and try again shortly", longest)
		}
		var causes []error
		for _, call := range calls {
			causes = append(causes, call.cause)
		}
		return nil, httpapi.Unavailable("Authorization is unavailable; try again shortly", errors.Join(causes...))
	}
	answer := &BatchAnswer{Organization: answered.answer.Organization, Principal: answered.answer.Principal, APIKeyID: answered.answer.APIKeyID, UserIdentity: answered.answer.UserIdentity, Credential: answered.answer.Credential}
	for _, call := range calls {
		if call.outcome == callOK {
			answer.Results = append(answer.Results, call.answer.Results...)
			continue
		}
		failed := &BatchError{Code: BatchUnavailable}
		if call.outcome == callRateLimited {
			failed = &BatchError{Code: BatchRateLimited, RetryAfterSeconds: call.retryAfter}
		}
		for _, check := range call.checks {
			e := *failed
			answer.Results = append(answer.Results, BatchResult{Action: check.Action, Resource: check.Resource, Error: &e})
		}
	}
	return answer, nil
}
