package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// [being built] Principal checks (contracts/identity-and-authz.md, "Principal checks"): org-api's
// answers about a stored principal, a user or a bot of the organization, from POST
// /internal/v1/permissions/principal-batch, asked with the product's own ok_ credential alone.
// Only where a product-held credential that can't be forwarded to org-api authenticated the request
// (Private CA's ACME bindings, Object Storage's access keys), for the credential's owner; a request
// carrying a forwardable customer credential is checked as the caller. Like Check, nothing is cached
// or reused: every call asks again.

// Principal types a principal check takes.
const (
	PrincipalUser = "user"
	PrincipalBot  = "bot"
)

// PrincipalNotFound is org-api's error code for a principal that isn't, or is no longer, a user
// (a current member) or a live bot of the organization, or an organization that's gone.
const PrincipalNotFound = "principal_not_found"

// CheckPrincipalBatch asks org-api about every check at once, for principal p of organization
// orgID as it is now: POST /internal/v1/permissions/principal-batch with Authorization: Bearer
// <ServiceCredential> and a body of exactly { organizationId, principal, checks }. No customer
// credential, no requestUrl and no X-Forwarded-For: the request carries nothing org-api can use.
//
// One call carries 1 to MaxBatchChecks checks; more, none, or a principal that isn't a user or a
// bot is the product's bug (500) without a call, and no ServiceCredential is 503 without one.
// org-api's answer:
//
//	200                       the answer, which must be for orgID and p and answer checks one for
//	                          one, in order, echoing each (anything else is 503)
//	404 principal_not_found   an answer with every check denied (Decision false, no Error) and no
//	                          Organization or UserIdentity: nothing to record. Callers deny
//	429                       429 with org-api's Retry-After (1-3600)
//	400                       500: the product sent a malformed body
//	401, 403, any other 404   503, logged (the error's Cause): the product's ok_ credential isn't
//	                          valid, its namespace hasn't declared permissions.principalBatch, or
//	                          org-api lacks the route
//	anything else             503 (5xx, a timeout, unreachable)
//
// A check passes only when its Allowed() is true.
func (c *Client) CheckPrincipalBatch(ctx context.Context, orgID string, p Principal, checks []BatchCheck) (*BatchAnswer, *httpapi.Error) {
	switch {
	case len(checks) == 0 || len(checks) > MaxBatchChecks:
		return nil, httpapi.Internal(fmt.Errorf("authz.CheckPrincipalBatch: %d checks; a call carries 1-%d", len(checks), MaxBatchChecks))
	case (p.Type != PrincipalUser && p.Type != PrincipalBot) || p.ID == "":
		return nil, httpapi.Internal(fmt.Errorf("authz.CheckPrincipalBatch: principal {%q %q} isn't a user or a bot", p.Type, p.ID))
	}
	unavailable := func(cause error) *httpapi.Error {
		return httpapi.Unavailable("Authorization is unavailable; try again shortly", cause)
	}
	if c.ServiceCredential == "" {
		return nil, unavailable(errors.New("authz.Client.ServiceCredential is empty: CheckPrincipalBatch needs the product's org-api ok_ credential (ORG_API_SERVICE_CREDENTIAL)"))
	}
	payload, err := json.Marshal(struct {
		OrganizationID string       `json:"organizationId"`
		Principal      Principal    `json:"principal"`
		Checks         []BatchCheck `json:"checks"`
	}{orgID, Principal{Type: p.Type, ID: p.ID}, checks}) // never the slug: org-api refuses unknown fields
	if err != nil {
		return nil, httpapi.Internal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/internal/v1/permissions/principal-batch", bytes.NewReader(payload))
	if err != nil {
		return nil, httpapi.Internal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.ServiceCredential)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	setRequestID(ctx, req)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, unavailable(fmt.Errorf("org-api principal batch: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		answer, err := decodeBatchAnswer(raw, checks)
		if err != nil {
			return nil, unavailable(fmt.Errorf("org-api principal batch: unexpected body (%v): %s", err, truncate(raw)))
		}
		if answer.Organization.ID != orgID {
			return nil, unavailable(fmt.Errorf("org-api principal batch: answered for organization %q, not %q", answer.Organization.ID, orgID))
		}
		if !answer.Principal.Same(p) {
			return nil, unavailable(fmt.Errorf("org-api principal batch: answered for principal {%q %q}, not the one asked about", answer.Principal.Type, answer.Principal.ID))
		}
		return answer, nil
	case http.StatusNotFound:
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &body) != nil || body.Error.Code != PrincipalNotFound {
			return nil, unavailable(fmt.Errorf("org-api answered 404 to the principal batch without %s (does it serve the route?): %s", PrincipalNotFound, truncate(raw)))
		}
		answer := &BatchAnswer{Principal: p, Results: make([]BatchResult, len(checks))}
		for i, check := range checks {
			denied := false
			answer.Results[i] = BatchResult{Action: check.Action, Resource: check.Resource, Decision: &denied}
		}
		return answer, nil
	case http.StatusTooManyRequests:
		return nil, httpapi.TooManyRequests("Too many requests; slow down and try again shortly", retryAfter(resp.Header))
	case http.StatusBadRequest:
		return nil, httpapi.Internal(fmt.Errorf("org-api refused the principal batch as malformed: %s", truncate(raw)))
	case http.StatusUnauthorized:
		return nil, unavailable(errors.New("org-api answered 401 to the principal batch: this namespace's ok_ credential isn't valid"))
	case http.StatusForbidden:
		return nil, unavailable(errors.New("org-api answered 403 to the principal batch: this namespace's ok_ credential isn't allowed permissions.principalBatch (declare it in the manifest's orgApi.operations)"))
	default:
		return nil, unavailable(fmt.Errorf("org-api principal batch answered %d", resp.StatusCode))
	}
}
