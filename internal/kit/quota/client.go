package quota

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
	"regexp"
	"time"
)

// Client calls quotas-api's product endpoints (contracts/quotas.md, "The internal API"): check,
// resolve, lease release and renewal. Registration is package registration's.
type Client struct {
	// URL is QUOTAS_API_URL, without a trailing slash.
	URL string
	// Credential is the namespace's qk_<16>.<secret> (QUOTAS_API_CREDENTIAL). Empty sends
	// InternalToken as X-Internal-Token instead, which quotas-api accepts only with APP_ENV=local.
	Credential    string
	InternalToken string
	// HTTP is the client for every call; nil is a shared one with a pool of kept-alive
	// connections (a new connection per request would eat most of the 50 ms).
	HTTP *http.Client
	// Timeout bounds check and resolve, connecting included (default 50 ms): past it the request
	// proceeds as if unlimited.
	Timeout time.Duration
}

// DefaultTimeout is how long a request waits for Quotas before failing open.
const DefaultTimeout = 50 * time.Millisecond

var sharedHTTP = &http.Client{Transport: &http.Transport{
	Proxy:               nil,
	DialContext:         (&net.Dialer{Timeout: time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:        256,
	MaxIdleConnsPerHost: 128,
	IdleConnTimeout:     90 * time.Second,
}}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return sharedHTTP
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

// Item is one quota a check or resolve asks about.
type Item struct {
	Quota string `json:"quota"`
	// ScopeKey: absent for organization scope (the organization) and, in check, for project
	// scope (the request's project); required for resource scope.
	ScopeKey string `json:"scopeKey,omitempty"`
	// Amount: how much to consume (check only; absent is 1).
	Amount int64 `json:"amount,omitempty"`
	// Region: resolve only, for perRegion quotas.
	Region string `json:"region,omitempty"`
}

// CheckRequest is POST /internal/v1/quotas/check's body.
type CheckRequest struct {
	OrganizationID string  `json:"organizationId"`
	ProjectID      *string `json:"projectId"`
	Region         string  `json:"region"`
	RequestID      *string `json:"requestId"`
	Items          []Item  `json:"items"`
}

// Result is one item's outcome: its value, and where it stands.
type Result struct {
	Quota       string  `json:"quota"`
	Kind        string  `json:"kind"`
	ScopeKey    string  `json:"scopeKey"`
	Region      string  `json:"region"`
	Value       int64   `json:"value"`
	Source      string  `json:"source"`
	Used        *int64  `json:"used"`
	Remaining   *int64  `json:"remaining"`
	Window      *string `json:"window"`
	PeriodStart *string `json:"periodStart"`
	ResetsAt    *string `json:"resetsAt"`
}

// Lease is a concurrency quota's lease, held until it's released or expires.
type Lease struct {
	Quota     string `json:"quota"`
	ScopeKey  string `json:"scopeKey"`
	LeaseID   string `json:"leaseId"`
	ExpiresAt string `json:"expiresAt"`
}

// CheckResponse is check's 200: allowed with a result per item and a lease per concurrency item,
// or denied with the first item that didn't fit (and nothing consumed).
type CheckResponse struct {
	Allowed bool     `json:"allowed"`
	Results []Result `json:"results"`
	Leases  []Lease  `json:"leases"`
	Denied  *Result  `json:"denied"`
}

// ResolveRequest is POST /internal/v1/quotas/resolve's body.
type ResolveRequest struct {
	OrganizationID string `json:"organizationId"`
	Items          []Item `json:"items"`
}

// Value is one resolved value.
type Value struct {
	Quota    string `json:"quota"`
	Kind     string `json:"kind"`
	ScopeKey string `json:"scopeKey"`
	Region   string `json:"region"`
	Value    int64  `json:"value"`
	Source   string `json:"source"`
}

// Fail-open reasons: the quotas_fail_open_total reason label.
const (
	ReasonTimeout         = "timeout"
	ReasonUnreachable     = "unreachable"
	ReasonUnavailable     = "unavailable"
	ReasonRefused         = "refused"
	ReasonInvalidResponse = "invalid_response"
)

// Failure is why Quotas gave no usable answer. The request proceeds as if unlimited.
type Failure struct {
	Reason string
	// Status is quotas-api's status, when it answered.
	Status int
	Err    error
}

func (f *Failure) Error() string {
	if f.Status != 0 {
		return fmt.Sprintf("quotas-api: %s (%d): %v", f.Reason, f.Status, f.Err)
	}
	return fmt.Sprintf("quotas-api: %s: %v", f.Reason, f.Err)
}

func (f *Failure) Unwrap() error { return f.Err }

// ErrLeaseNotFound is renew's 404: the lease expired or was released. Carry on without it.
var ErrLeaseNotFound = errors.New("quotas-api: lease not found")

// Check consumes an endpoint's rate, usage and concurrency quotas and resolves its allocation
// quotas, all or nothing, within the timeout. Any error is a *Failure.
func (c *Client) Check(ctx context.Context, req CheckRequest) (*CheckResponse, error) {
	body, status, err := c.post(ctx, "/internal/v1/quotas/check", req)
	if err != nil {
		return nil, err
	}
	resp, err := parseCheck(body, req.Items)
	if err != nil {
		return nil, &Failure{Reason: ReasonInvalidResponse, Status: status, Err: err}
	}
	return resp, nil
}

// Resolve answers values only, consuming nothing, within the timeout. Any error is a *Failure.
func (c *Client) Resolve(ctx context.Context, req ResolveRequest) ([]Value, error) {
	body, status, err := c.post(ctx, "/internal/v1/quotas/resolve", req)
	if err != nil {
		return nil, err
	}
	values, err := parseResolve(body, req.Items)
	if err != nil {
		return nil, &Failure{Reason: ReasonInvalidResponse, Status: status, Err: err}
	}
	return values, nil
}

// ReleaseTimeout bounds a lease release.
const ReleaseTimeout = time.Second

// Release gives a lease back: DELETE, fire-and-forget, with a 1-second timeout. A lease that
// isn't released expires at its leaseTtl, so a failure is only logged by the caller (done gets
// the error, and may be nil).
func (c *Client) Release(leaseID string, done func(error)) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ReleaseTimeout)
		defer cancel()
		err := c.release(ctx, leaseID)
		if done != nil {
			done(err)
		}
	}()
}

func (c *Client) release(ctx context.Context, leaseID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.URL+"/internal/v1/quotas/leases/"+url.PathEscape(leaseID), nil)
	if err != nil {
		return err
	}
	c.authenticate(req)
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("quotas-api answered %d releasing %s", resp.StatusCode, leaseID)
	}
	return nil
}

// Renew extends a lease to its leaseTtl from now. A lease that has expired or been released is
// ErrLeaseNotFound: the holder carries on without it.
func (c *Client) Renew(ctx context.Context, leaseID string) (*Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, ReleaseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/internal/v1/quotas/leases/"+url.PathEscape(leaseID)+"/renew", nil)
	if err != nil {
		return nil, err
	}
	c.authenticate(req)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		var lease Lease
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&lease); err != nil || !leaseIDPattern.MatchString(lease.LeaseID) {
			return nil, fmt.Errorf("quotas-api: an invalid lease renewal: %v", err)
		}
		return &lease, nil
	case http.StatusNotFound:
		return nil, ErrLeaseNotFound
	default:
		return nil, fmt.Errorf("quotas-api answered %d renewing %s", resp.StatusCode, leaseID)
	}
}

func (c *Client) authenticate(req *http.Request) {
	if c.Credential != "" {
		req.Header.Set("Authorization", "Bearer "+c.Credential)
	} else if c.InternalToken != "" {
		req.Header.Set("X-Internal-Token", c.InternalToken)
	}
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
}

// post sends a check or resolve and classifies anything but a 200.
func (c *Client) post(ctx context.Context, path string, payload any) ([]byte, int, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, &Failure{Reason: ReasonRefused, Err: err}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(b))
	if err != nil {
		return nil, 0, &Failure{Reason: ReasonUnreachable, Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	c.authenticate(req)
	resp, err := c.http().Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, 0, &Failure{Reason: ReasonTimeout, Err: err}
		}
		return nil, 0, &Failure{Reason: ReasonUnreachable, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return nil, resp.StatusCode, &Failure{Reason: ReasonTimeout, Status: resp.StatusCode, Err: err}
		}
		return nil, resp.StatusCode, &Failure{Reason: ReasonUnreachable, Status: resp.StatusCode, Err: err}
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return body, resp.StatusCode, nil
	case resp.StatusCode == http.StatusServiceUnavailable:
		return nil, resp.StatusCode, &Failure{Reason: ReasonUnavailable, Status: resp.StatusCode, Err: errors.New(snippet(body))}
	default:
		return nil, resp.StatusCode, &Failure{Reason: ReasonRefused, Status: resp.StatusCode, Err: errors.New(snippet(body))}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func snippet(body []byte) string {
	if len(body) > 300 {
		body = body[:300]
	}
	return string(body)
}

// ---- Validating answers ------------------------------------------------------------------------

var (
	leaseIDPattern = regexp.MustCompile(`^qls_[a-z0-9]{24}$`)
	uuidPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	regionPattern  = regexp.MustCompile(`^([a-z]{2}-[a-z]+-[0-9]+)?$`)
	instantPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
	kinds          = map[string]bool{"allocation": true, "rate": true, "usage": true, "concurrency": true}
	sources        = map[string]bool{SourceOverride: true, SourceOrganizationOverride: true, SourceTier: true, SourceDefault: true}
)

// wire forms with every field required: a missing one is an invalid answer, not a zero.
type wireResult struct {
	Quota       *string         `json:"quota"`
	Kind        *string         `json:"kind"`
	ScopeKey    *string         `json:"scopeKey"`
	Region      *string         `json:"region"`
	Value       *int64          `json:"value"`
	Source      *string         `json:"source"`
	Used        json.RawMessage `json:"used"`
	Remaining   json.RawMessage `json:"remaining"`
	Window      json.RawMessage `json:"window"`
	PeriodStart json.RawMessage `json:"periodStart"`
	ResetsAt    json.RawMessage `json:"resetsAt"`
}

func (w *wireResult) result(at string, withCounts bool) (Result, error) {
	if w.Quota == nil || w.Kind == nil || w.ScopeKey == nil || w.Region == nil || w.Value == nil || w.Source == nil {
		return Result{}, fmt.Errorf("%s: quota, kind, scopeKey, region, value and source are required", at)
	}
	r := Result{Quota: *w.Quota, Kind: *w.Kind, ScopeKey: *w.ScopeKey, Region: *w.Region, Value: *w.Value, Source: *w.Source}
	if !kinds[r.Kind] || !sources[r.Source] || !uuidPattern.MatchString(r.ScopeKey) || !regionPattern.MatchString(r.Region) || r.Value < 0 {
		return Result{}, fmt.Errorf("%s: a malformed kind, source, scopeKey, region or value", at)
	}
	if !withCounts {
		return r, nil
	}
	var err error
	for _, f := range []struct {
		name string
		raw  json.RawMessage
		into **int64
	}{{"used", w.Used, &r.Used}, {"remaining", w.Remaining, &r.Remaining}} {
		if len(f.raw) == 0 {
			return Result{}, fmt.Errorf("%s.%s is required", at, f.name)
		}
		if err = json.Unmarshal(f.raw, f.into); err != nil || (*f.into != nil && **f.into < 0) {
			return Result{}, fmt.Errorf("%s.%s: an integer ≥ 0 or null", at, f.name)
		}
	}
	for _, f := range []struct {
		name string
		raw  json.RawMessage
		into **string
		form *regexp.Regexp
	}{{"window", w.Window, &r.Window, nil}, {"periodStart", w.PeriodStart, &r.PeriodStart, instantPattern}, {"resetsAt", w.ResetsAt, &r.ResetsAt, instantPattern}} {
		if len(f.raw) == 0 {
			return Result{}, fmt.Errorf("%s.%s is required", at, f.name)
		}
		if err = json.Unmarshal(f.raw, f.into); err != nil || (*f.into != nil && f.form != nil && !f.form.MatchString(**f.into)) {
			return Result{}, fmt.Errorf("%s.%s: a string or null", at, f.name)
		}
	}
	return r, nil
}

// parseCheck reads a check answer strictly: the schema's required fields, and its agreement with
// what was asked (a result per item, in order; a lease per concurrency result; denied naming an
// item that could be refused).
func parseCheck(body []byte, items []Item) (*CheckResponse, error) {
	var w struct {
		Allowed *bool           `json:"allowed"`
		Results *[]wireResult   `json:"results"`
		Leases  *[]Lease        `json:"leases"`
		Denied  json.RawMessage `json:"denied"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	if w.Allowed == nil || w.Results == nil || w.Leases == nil || len(w.Denied) == 0 {
		return nil, errors.New("allowed, results, leases and denied are required")
	}
	resp := &CheckResponse{Allowed: *w.Allowed, Leases: *w.Leases, Results: []Result{}}
	for i := range *w.Results {
		r, err := (*w.Results)[i].result(fmt.Sprintf("results.%d", i), true)
		if err != nil {
			return nil, err
		}
		resp.Results = append(resp.Results, r)
	}
	for i, l := range resp.Leases {
		if !leaseIDPattern.MatchString(l.LeaseID) || l.Quota == "" || !uuidPattern.MatchString(l.ScopeKey) || !instantPattern.MatchString(l.ExpiresAt) {
			return nil, fmt.Errorf("leases.%d: malformed", i)
		}
	}
	if string(w.Denied) != "null" {
		var d wireResult
		if err := json.Unmarshal(w.Denied, &d); err != nil {
			return nil, fmt.Errorf("denied: %w", err)
		}
		r, err := d.result("denied", true)
		if err != nil {
			return nil, err
		}
		resp.Denied = &r
	}
	if resp.Allowed {
		if resp.Denied != nil || len(resp.Results) != len(items) {
			return nil, fmt.Errorf("allowed, with %d results for %d items, denied %v", len(resp.Results), len(items), resp.Denied != nil)
		}
		concurrency := map[string]int{}
		for i, r := range resp.Results {
			if r.Quota != items[i].Quota {
				return nil, fmt.Errorf("results.%d is %s, not %s", i, r.Quota, items[i].Quota)
			}
			if r.Kind == "concurrency" {
				concurrency[r.Quota]++
			}
		}
		for _, l := range resp.Leases {
			concurrency[l.Quota]--
		}
		for q, n := range concurrency {
			if n != 0 {
				return nil, fmt.Errorf("%s: a lease per concurrency item", q)
			}
		}
		return resp, nil
	}
	if resp.Denied == nil || len(resp.Results) != 0 || len(resp.Leases) != 0 || resp.Denied.Kind == "allocation" {
		return nil, errors.New("denied, but not with exactly one refusable item and nothing consumed")
	}
	for _, it := range items {
		if it.Quota == resp.Denied.Quota {
			return resp, nil
		}
	}
	return nil, fmt.Errorf("denied %s, which wasn't asked about", resp.Denied.Quota)
}

func parseResolve(body []byte, items []Item) ([]Value, error) {
	var w struct {
		Values *[]wireResult `json:"values"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, err
	}
	if w.Values == nil || len(*w.Values) != len(items) {
		return nil, errors.New("a value per item is required")
	}
	values := make([]Value, len(items))
	for i := range *w.Values {
		r, err := (*w.Values)[i].result(fmt.Sprintf("values.%d", i), false)
		if err != nil {
			return nil, err
		}
		if r.Quota != items[i].Quota {
			return nil, fmt.Errorf("values.%d is %s, not %s", i, r.Quota, items[i].Quota)
		}
		values[i] = Value{Quota: r.Quota, Kind: r.Kind, ScopeKey: r.ScopeKey, Region: r.Region, Value: r.Value, Source: r.Source}
	}
	return values, nil
}
