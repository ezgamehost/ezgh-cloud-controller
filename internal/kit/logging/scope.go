package logging

import (
	"context"
	"sync"
)

// Scope is one request's logging state (docs/logging-design.md, "The request scope"): created by
// httpapi.AccessLog, filled in by the kit's auth and error helpers, read by every line logged with
// the request's context and by the access line. Writes after Finish are ignored.
type Scope struct {
	mu        sync.Mutex
	done      bool
	requestID string
	orgID     string
	route     string
	caller    Caller
	errorCode string
}

// Caller is who made a request, as org-api answered: Type is user, bot or ssoUser, or internal
// (with no ID) for a valid internal call; APIKeyID when they used an API key.
type Caller struct {
	Type           string
	ID             string
	APIKeyID       string
	OrganizationID string
}

type scopeKey struct{}

func NewScope(requestID string) *Scope { return &Scope{requestID: requestID} }
func WithScope(ctx context.Context, s *Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, s)
}

// ScopeFrom is ctx's request scope, nil outside a request.
func ScopeFrom(ctx context.Context) *Scope {
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	return s
}

func (s *Scope) set(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.done {
		f()
	}
}

// SetCaller records who the request is from (and their organization, when given).
func SetCaller(ctx context.Context, c Caller) {
	if s := ScopeFrom(ctx); s != nil {
		s.set(func() {
			s.caller = c
			if c.OrganizationID != "" {
				s.orgID = c.OrganizationID
			}
		})
	}
}

// SetOrganization records the organization once it's known and authorized.
func SetOrganization(ctx context.Context, id string) {
	if s := ScopeFrom(ctx); s != nil {
		s.set(func() { s.orgID = id })
	}
}

// SetRoute records the route template (RouteOf's form).
func SetRoute(ctx context.Context, route string) {
	if s := ScopeFrom(ctx); s != nil {
		s.set(func() { s.route = route })
	}
}

// SetErrorCode records the error response's code.
func (s *Scope) SetErrorCode(code string) { s.set(func() { s.errorCode = code }) }

func (s *Scope) ids() (requestID, organizationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requestID, s.orgID
}

// Finish ends the request: later writes are ignored. It returns what the access line needs.
func (s *Scope) Finish() (route string, caller Caller, errorCode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	return s.route, s.caller, s.errorCode
}
