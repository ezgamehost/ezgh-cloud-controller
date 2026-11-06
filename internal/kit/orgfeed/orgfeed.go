// Package orgfeed is a product worker's client of org-api's lifecycle reads
// (contracts/org-lifecycle.md, "The lifecycle feed"; login-stack services/org-api
// routes/lifecycle-service.ts), called with the product's ok_ credential, its manifest declaring
// the org-api operation lifecycle.events:
//
//   - GET /internal/v1/lifecycle-events?after=<seq|latest>&limit=<1-1000>[&kinds=<k,…>]: what
//     was removed or deleted (and, only on a client from WithKinds that asks for it, which
//     projects were created), oldest first, with the feed's head; 410 feed_expired when after is older than
//     what's kept (7 days) or past the head.
//   - POST /internal/v1/principals/exists: which of an organization's principals and projects
//     still exist, for reconciling after a 410.
//
// Nothing is cached: every call asks org-api.
package orgfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Event kinds. The five deletion kinds are what the feed returns without WithKinds.
const (
	MemberRemoved       = "member.removed"
	BotDeleted          = "bot.deleted"
	SSOUserDeleted      = "ssoUser.deleted"
	ProjectDeleted      = "project.deleted"
	OrganizationDeleted = "organization.deleted"
	// [being built] ProjectCreated is returned only when asked for with WithKinds. A reader skips
	// it for a project it already saw deleted, and still sets up lazily on first use: the feed is
	// pulled, so the event can arrive after the project is first used.
	ProjectCreated = "project.created"
)

// MaxPage is the most events one read returns; MaxExists the most principals, and the most
// projects, one existence check names.
const (
	MaxPage   = 1000
	MaxExists = 100
)

// Principal is a user (member.removed), bot or directory user (ssoUser).
type Principal struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Event is one lifecycle event. ProjectID is set for project.deleted and project.created;
// Principal for the three principal kinds.
type Event struct {
	Seq            int64      `json:"seq"`
	Kind           string     `json:"kind"`
	OrganizationID string     `json:"organizationId"`
	ProjectID      *string    `json:"projectId"`
	Principal      *Principal `json:"principal"`
	OccurredAt     time.Time  `json:"occurredAt"`
}

// Page is one read of the feed: events after the position asked for, and the head and time from
// the same snapshot.
type Page struct {
	Events []Event   `json:"events"`
	Head   int64     `json:"head"`
	AsOf   time.Time `json:"asOf"`
}

// ExistsRequest asks about up to 100 principals and 100 projects of one organization.
type ExistsRequest struct {
	OrganizationID string      `json:"organizationId"`
	Principals     []Principal `json:"principals,omitempty"`
	Projects       []string    `json:"projects,omitempty"`
}

// PrincipalExists is one principal's answer. Exists is nil when org-api couldn't tell (a
// directory user while sso-api is unavailable): keep it and ask again later.
type PrincipalExists struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Exists *bool  `json:"exists"`
}

// ProjectExists is one project's answer.
type ProjectExists struct {
	ID     string `json:"id"`
	Exists bool   `json:"exists"`
}

// ExistsAnswer answers in the order asked. When OrganizationExists is false, nothing in it
// exists.
type ExistsAnswer struct {
	OrganizationExists bool              `json:"organizationExists"`
	Principals         []PrincipalExists `json:"principals"`
	Projects           []ProjectExists   `json:"projects"`
}

var (
	// ErrFeedExpired is 410 feed_expired: reconcile, then resume from the head.
	ErrFeedExpired = errors.New("org-api: the lifecycle feed expired under the cursor")
	// ErrRateLimited is 429; Error.RetryAfter says when.
	ErrRateLimited = errors.New("org-api: rate limited")
	// ErrDenied is 401 or 403: the ok_ is wrong, or the product's registration lacks
	// lifecycle.events.
	ErrDenied = errors.New("org-api: denied")
	// ErrInvalid is 400.
	ErrInvalid = errors.New("org-api: invalid request")
	// ErrUnavailable is anything else: 5xx, no answer, an unreadable answer.
	ErrUnavailable = errors.New("org-api: unavailable")
)

// Error is a failed org-api call.
type Error struct {
	Op         string
	Status     int
	Code       string
	RetryAfter time.Duration
	kind       error
	cause      error
}

func (e *Error) Error() string {
	s := "org-api " + e.Op
	if e.Status != 0 {
		s += fmt.Sprintf(": %d", e.Status)
	}
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.cause != nil {
		s += ": " + e.cause.Error()
	}
	return s
}

// Is matches the error's kind.
func (e *Error) Is(target error) bool { return target == e.kind }

// Unwrap is the transport's error, if any.
func (e *Error) Unwrap() error { return e.cause }

// Client calls org-api with the product's ok_ credential.
type Client struct {
	base       string
	credential string
	http       *http.Client
	// [being built] kinds are what WithKinds asked for; filtered says it was called.
	kinds    []string
	filtered bool
}

// New is a client of org-api at baseURL (ORG_API_URL) with credential (ORG_API_SERVICE_CREDENTIAL).
// A nil httpClient means one with a 10-second timeout.
func New(baseURL, credential string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), credential: credential, http: httpClient}
}

// WithKinds is a client whose Events asks only for these kinds (kinds=<comma-separated>). The
// receiver is unchanged, and the result has the same method set, so it fits wherever the client
// does. Without it the feed returns the five deletion kinds; ProjectCreated is only ever returned
// when asked for. Calling WithKinds again replaces the kinds. No kinds, an empty kind or one
// containing a comma makes Events ErrInvalid before any call; org-api answers 400 (ErrInvalid)
// for a kind it doesn't know.
func (c *Client) WithKinds(kinds ...string) *Client {
	d := *c
	d.kinds, d.filtered = append([]string(nil), kinds...), true
	return &d
}

// Events reads up to limit events after after (a seq, or "latest" for none: just the head). On a
// client from WithKinds, an event of a kind not asked for (an org-api that ignored the filter) is
// ErrUnavailable.
func (c *Client) Events(ctx context.Context, after string, limit int) (*Page, error) {
	if limit < 1 || limit > MaxPage {
		return nil, &Error{Op: "lifecycle-events", kind: ErrInvalid, cause: errors.New("limit must be 1-1000")}
	}
	q := url.Values{"after": {after}, "limit": {strconv.Itoa(limit)}}
	if c.filtered {
		if len(c.kinds) == 0 {
			return nil, &Error{Op: "lifecycle-events", kind: ErrInvalid, cause: errors.New("WithKinds needs at least one kind")}
		}
		for _, k := range c.kinds {
			if k == "" || strings.Contains(k, ",") {
				return nil, &Error{Op: "lifecycle-events", kind: ErrInvalid, cause: fmt.Errorf("bad kind %q", k)}
			}
		}
		q.Set("kinds", strings.Join(c.kinds, ","))
	}
	var page Page
	if err := c.do(ctx, "lifecycle-events", http.MethodGet, "/internal/v1/lifecycle-events?"+q.Encode(), nil, &page); err != nil {
		return nil, err
	}
	for i, e := range page.Events {
		if i > 0 && e.Seq <= page.Events[i-1].Seq {
			return nil, &Error{Op: "lifecycle-events", Status: http.StatusOK, kind: ErrUnavailable, cause: errors.New("events out of order")}
		}
		if c.filtered && !slices.Contains(c.kinds, e.Kind) {
			return nil, &Error{Op: "lifecycle-events", Status: http.StatusOK, kind: ErrUnavailable, cause: fmt.Errorf("event %d is a %s, not asked for", e.Seq, e.Kind)}
		}
	}
	return &page, nil
}

// Exists asks which of the principals and projects still exist.
func (c *Client) Exists(ctx context.Context, in ExistsRequest) (*ExistsAnswer, error) {
	if len(in.Principals) > MaxExists || len(in.Projects) > MaxExists {
		return nil, &Error{Op: "principals-exists", kind: ErrInvalid, cause: errors.New("at most 100 principals and 100 projects")}
	}
	var out ExistsAnswer
	if err := c.do(ctx, "principals-exists", http.MethodPost, "/internal/v1/principals/exists", in, &out); err != nil {
		return nil, err
	}
	if len(out.Principals) != len(in.Principals) || len(out.Projects) != len(in.Projects) {
		return nil, &Error{Op: "principals-exists", Status: http.StatusOK, kind: ErrUnavailable, cause: errors.New("answer doesn't match the question")}
	}
	for i, p := range out.Principals {
		if p.Type != in.Principals[i].Type || p.ID != in.Principals[i].ID {
			return nil, &Error{Op: "principals-exists", Status: http.StatusOK, kind: ErrUnavailable, cause: errors.New("answer out of order")}
		}
	}
	for i, p := range out.Projects {
		if p.ID != in.Projects[i] {
			return nil, &Error{Op: "principals-exists", Status: http.StatusOK, kind: ErrUnavailable, cause: errors.New("answer out of order")}
		}
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, op, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return &Error{Op: op, kind: ErrInvalid, cause: err}
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return &Error{Op: op, kind: ErrInvalid, cause: err}
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.credential)
	res, err := c.http.Do(req)
	if err != nil {
		return &Error{Op: op, kind: ErrUnavailable, cause: err}
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return &Error{Op: op, Status: res.StatusCode, kind: ErrUnavailable, cause: err}
	}
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{Op: op, Status: res.StatusCode, kind: ErrUnavailable, cause: errors.New("unreadable answer")}
		}
		return nil
	}
	var eb struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &eb)
	e := &Error{Op: op, Status: res.StatusCode, Code: eb.Error.Code}
	switch {
	case res.StatusCode == http.StatusGone:
		e.kind = ErrFeedExpired
	case res.StatusCode == http.StatusTooManyRequests:
		e.kind = ErrRateLimited
		e.RetryAfter = retryAfter(res.Header)
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		e.kind = ErrDenied
	case res.StatusCode == http.StatusBadRequest:
		e.kind = ErrInvalid
	default:
		e.kind = ErrUnavailable
	}
	return e
}

// retryAfter is Retry-After (or better-auth's X-Retry-After) in whole seconds, 1–3600.
func retryAfter(h http.Header) time.Duration {
	for _, name := range []string{"Retry-After", "X-Retry-After"} {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Get(name))); err == nil {
			return time.Duration(min(max(n, 1), 3600)) * time.Second
		}
	}
	return time.Second
}
