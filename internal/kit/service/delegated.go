package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/credential"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/delegation"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resource"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

// [being built] Delegated calls (contracts/service-auth.md, "Delegated calls";
// contracts/identity-and-authz.md, "Delegated checks"). A receiver's auth "delegated" endpoint
// serves another product's call made for a customer: the caller's receiver-issued credential,
// bound to its namespace, and an X-Ezgh-Caller reference. Its handler is the public route's: on a
// delegated call, Authorize, AuthorizeProject(Only), AuthorizeMissing, Check and CheckBatch each
// ask org-api's delegated check (one call each, nothing reused), so the call is attributable and
// recorded as the customer's, invoked by the calling service. An edge builds its outbound
// reference with Delegation (from its CheckBatch), a receiver relaying with Delegate.

// prepareDelegated runs a delegated endpoint's checks, in order, before the handler: (1) the
// caller's service credential (401 unauthenticated; X-Internal-Token never authorizes a
// delegated route), (2) exactly one canonical X-Ezgh-Caller (400 invalid_caller_reference, a
// missing one too), (3) the credential's namespace is the reference's caller and has an entry in
// the manifest's delegation, whose origins name the reference's origin for a two-entry chain
// (403 delegation_forbidden, logged, with no org-api call); then (4) the organization is the
// reference's, the project the path's :projectId, and the body is read as on caller routes.
// None of these refusals is recorded.
func (c *Call) prepareDelegated() error {
	s := c.Service
	if s.Credentials == nil {
		return httpapi.Internal(errors.New("service: delegated endpoints need Options.Credentials (the service's own credential store)"))
	}
	// Exactly one Authorization header: a second one is never a credential to choose between.
	if len(c.R.Header.Values("Authorization")) != 1 {
		return httpapi.Unauthenticated("A service credential is required")
	}
	authorization := c.R.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, "Bearer ") {
		return httpapi.Unauthenticated("A service credential is required")
	}
	caller, err := s.Credentials.VerifyCaller(c.Ctx, authorization)
	if errors.Is(err, credential.ErrInvalid) {
		return httpapi.Unauthenticated("A service credential is required")
	}
	if err != nil {
		return httpapi.Unavailable("Credentials can't be checked right now", err)
	}
	// ServiceCaller stays empty: on a delegated route the caller authorizes nothing by itself, so a
	// handler can't mistake "compute-api called" for permission (only org-api's answer is).

	values := c.R.Header.Values(delegation.Header)
	ref, err := delegation.FromHeaders(values)
	if err != nil {
		return httpapi.InvalidCallerReference(err.Error())
	}

	forbidden := func(format string, args ...any) error {
		cause := fmt.Errorf(format, args...)
		s.Logger.WarnContext(c.Ctx, "delegated call refused", "route", c.Endpoint.Path, "caller", caller.Name, "chain", strings.Join(ref.Chain, ","), "error", cause.Error())
		return httpapi.DelegationForbidden(cause)
	}
	switch {
	case caller.Namespace == "":
		return forbidden("the credential for %s is bound to no namespace (issue it with --namespace)", caller.Name)
	case caller.Namespace != ref.Caller():
		return forbidden("the credential is bound to namespace %s, but the reference's caller is %s", caller.Namespace, ref.Caller())
	}
	entry, ok := s.Manifest.Delegation.Entry(caller.Namespace)
	if !ok {
		return forbidden("the manifest's delegation has no entry for %s", caller.Namespace)
	}
	if len(ref.Chain) == 2 && !slices.Contains(entry.Origins, ref.Origin()) {
		return forbidden("%s's delegation entry doesn't name origin %s", caller.Namespace, ref.Origin())
	}
	c.delegated = &ref
	c.delegatedHeader = values[0]

	// A malformed ID is 404, like an unknown one, and a prefixed ID becomes its UUID, as on caller
	// routes.
	if err := c.resolvePathIDs(ref.Org); err != nil {
		return err
	}
	c.OrganizationID = ref.Org
	if project := c.R.PathValue("projectId"); project != "" {
		c.ProjectID = &project
	}
	if c.private {
		return nil
	}
	return c.readBody()
}

// Delegated is the caller reference a delegated call came with (nil off auth "delegated"
// endpoints). Its Caller is the service that called, its Origin the edge the customer's request
// arrived at.
func (c *Call) Delegated() *delegation.Reference { return c.delegated }

// Delegation is an edge's outbound reference for this request, for delegation.Client.Do: built
// from the call's last CheckBatch answer (an edge checks its own request with the batch check
// first), at the time the request arrived, with the service's namespace as the chain; the
// request's X-Forwarded-For unchanged (its peer address when it has none) and its X-Request-Id.
// An answer that names no credential is 503, logged: no delegated call is made without a
// reference. A receiver relays with Delegate instead.
func (c *Call) Delegation() (*delegation.Outbound, error) {
	if c.delegated != nil {
		return nil, httpapi.Internal(errors.New("service: Delegation on a delegated call; a receiver relays with Delegate"))
	}
	if c.lastBatch == nil {
		return nil, httpapi.Internal(errors.New("service: Delegation before CheckBatch: an edge builds its reference from its own batch answer"))
	}
	ref, err := delegation.FromAnswer(c.lastBatch, c.Received, c.Service.Manifest.Namespace)
	if err != nil {
		c.Service.Logger.WarnContext(c.Ctx, "no delegated call: org-api's answer names no delegable credential", "route", c.Endpoint.Path, "error", err.Error())
		return nil, httpapi.Unavailable("A service this request needs is unavailable; try again shortly", err)
	}
	return c.outbound(ref), nil
}

// Delegate is a receiver's outbound reference for a call it makes onward for the same customer:
// the reference it received with its own namespace appended (never a third entry), and the
// request's X-Forwarded-For and X-Request-Id. Off a delegated call it's 500, a bug.
func (c *Call) Delegate() (*delegation.Outbound, error) {
	if c.delegated == nil {
		return nil, httpapi.Internal(errors.New("service: Delegate off a delegated call; an edge uses Delegation"))
	}
	ref, err := c.delegated.Relay(c.Service.Manifest.Namespace)
	if err != nil {
		return nil, httpapi.Internal(fmt.Errorf("service: relaying the caller reference: %w", err))
	}
	return c.outbound(ref), nil
}

func (c *Call) outbound(ref delegation.Reference) *delegation.Outbound {
	return &delegation.Outbound{Reference: ref, ForwardedFor: forwardedFor(c.R), RequestID: c.RequestID}
}

// forwardedFor is the request's X-Forwarded-For values joined unchanged, or its peer address when
// it has none (a direct in-cluster call), so whoever counts the customer counts them, never us.
func forwardedFor(r *http.Request) string {
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		return strings.Join(values, ", ")
	}
	return httpapi.ClientIP(r)
}

// checkDelegated asks org-api's delegated check (one call: never split) and makes the call
// attributable from its answer. A check org-api couldn't delegate (null: not_delegable, or a
// per-check bug) is 403 delegation_forbidden, logged and not recorded: the call isn't attributed.
// false results are left for the caller (403 access_denied, recorded).
func (c *Call) checkDelegated(checks []authz.BatchCheck) (*authz.BatchAnswer, error) {
	answer, herr := c.Service.OrgAPI.CheckDelegated(c.Ctx, authz.DelegatedRequest{
		Caller: c.delegated.Caller(), Reference: c.delegatedHeader, ForwardedFor: forwardedFor(c.R), Checks: checks,
	})
	if herr != nil {
		// org-api's refusal of the delegation itself is never the customer's call, even after an
		// earlier check of this call succeeded (contracts/trails.md, "Delegated calls").
		c.delegationRefused = true
		if herr.Code == "delegation_forbidden" {
			c.Service.Logger.WarnContext(c.Ctx, "delegated call refused by org-api", "route", c.Endpoint.Path, "caller", c.delegated.Caller(), "error", fmt.Sprint(herr.Cause))
		}
		return nil, herr
	}
	for _, r := range answer.Results {
		if r.Decision == nil {
			code, message := "", ""
			if r.Error != nil {
				code, message = r.Error.Code, r.Error.Message
			}
			cause := fmt.Errorf("org-api didn't evaluate %s on %s for %s: %s %s", r.Action, r.Resource, c.delegated.Caller(), code, message)
			c.Service.Logger.WarnContext(c.Ctx, "delegated call refused", "route", c.Endpoint.Path, "error", cause.Error())
			c.delegationRefused = true
			return nil, httpapi.DelegationForbidden(cause)
		}
	}
	// The answer must be about the reference's organization and name the calling service, or it
	// isn't this call's: never act or record on it.
	var who struct {
		InvokedBy *string `json:"invokedBy"`
	}
	_ = json.Unmarshal(answer.UserIdentity, &who)
	want := trails.DelegatedUserAgent(c.delegated.Caller())
	if answer.Organization.ID != c.delegated.Org || who.InvokedBy == nil || *who.InvokedBy != want {
		c.delegationRefused = true
		return nil, httpapi.Unavailable("Authorization is unavailable; try again shortly", fmt.Errorf("org-api's delegated answer is for organization %q invoked by %v, not %q invoked by %q", answer.Organization.ID, who.InvokedBy, c.delegated.Org, want))
	}
	c.attribute(&authz.Permissions{Organization: answer.Organization, Principal: answer.Principal, APIKeyID: answer.APIKeyID, UserIdentity: answer.UserIdentity})
	return answer, nil
}

// checkDelegatedActions is Check on a delegated call: one check per action on resourceName (the
// organization's own name for ""), answered as GetPermissions would be.
func (c *Call) checkDelegatedActions(actions []string, resourceName string) (*authz.Permissions, error) {
	if resourceName == "" {
		dir, err := c.Service.OrgAPI.Lookup(c.Ctx, []string{c.OrganizationID}, nil)
		if err != nil {
			return nil, httpapi.Unavailable("The directory is unavailable; try again shortly", err)
		}
		org, known := dir.Organizations[c.OrganizationID]
		if !known || org.State == authz.StateDeleted {
			return nil, httpapi.NotFound("Organization")
		}
		resourceName = resource.Organization(org.Slug)
	}
	checks := make([]authz.BatchCheck, len(actions))
	for i, a := range actions {
		checks[i] = authz.BatchCheck{Action: a, Resource: resourceName}
	}
	answer, err := c.checkDelegated(checks)
	if err != nil {
		return nil, err
	}
	perm := &authz.Permissions{Organization: answer.Organization, Principal: answer.Principal, APIKeyID: answer.APIKeyID, UserIdentity: answer.UserIdentity, Resource: resourceName, Allowed: []string{}}
	for _, r := range answer.Results {
		if r.Allowed() && !slices.Contains(perm.Allowed, r.Action) {
			perm.Allowed = append(perm.Allowed, r.Action)
		}
	}
	return perm, nil
}

// errDelegatedUnauthorized: a delegated route answered a success without asking org-api. A
// delegated call is the customer's only once org-api's delegated check said so; the caller's word
// is never enough (NX2: no blind trust).
var errDelegatedUnauthorized = httpapi.Internal(errors.New("service: a delegated endpoint answered a success without asking org-api (call Authorize, Check or CheckBatch first)"))

// unauthorizedDelegated reports a 2xx on a delegated endpoint the kit must refuse: org-api hasn't
// vouched for the customer.
func (c *Call) unauthorizedDelegated(status int) bool {
	return c.Endpoint.Delegated() && !c.Attributable() && status >= 200 && status < 300
}

// callerUsedAPIKey: a refusal says "This API key isn't allowed to …" for an API key, as on the
// public route.
func (c *Call) callerUsedAPIKey() bool {
	if c.delegated != nil {
		return c.delegated.CredentialKind == "apiKey"
	}
	return c.credential.IsAPIKey()
}
