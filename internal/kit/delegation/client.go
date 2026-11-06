package delegation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Outbound is what one delegated call carries for the customer: the reference, their request's
// X-Forwarded-For (unchanged) and its X-Request-Id. Build it with service.Call.Delegation (an
// edge) or Call.Delegate (a receiver relaying), never by hand, and never keep it past the request.
type Outbound struct {
	Reference    Reference
	ForwardedFor string
	RequestID    string
}

// Client calls one receiver's auth "delegated" routes.
type Client struct {
	// Receiver names the receiver, for errors and logs (images).
	Receiver string
	// BaseURL is the receiver, in-cluster: http://images-api.images-stack.svc.cluster.local:6800.
	// A request whose URL has no host is sent to BaseURL plus its path and query.
	BaseURL string
	// Credential is the receiver-issued service credential, <key_id>.<secret>, bound to this
	// service's namespace (the edge's <RECEIVER>_SERVICE_CREDENTIAL).
	Credential string
	// HTTP is the client to send with; nil is one with a 10-second timeout.
	HTTP *http.Client
}

// dotSegments reports a "." or ".." segment in a decoded path.
func dotSegments(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// ForwardedHeaders are the request headers Do sends on from req, by their canonical names; every
// other header (Cookie, the customer's Authorization, any spelling of them) is dropped.
var ForwardedHeaders = []string{"Accept", "Content-Type", "Idempotency-Key", "If-Match", "If-None-Match", "User-Agent"}

// Do sends req to the receiver as a delegated call: Authorization: Bearer <Credential>,
// X-Ezgh-Caller, X-Forwarded-For and X-Request-Id from out, and of req's own headers only
// ForwardedHeaders, so no customer credential ever leaves the edge. The request goes to BaseURL's
// scheme and host only: a req whose URL has no host is sent there, and one naming another scheme
// or host is refused (it would carry the receiver's credential elsewhere). The path keeps its
// escaping (an ID's %2F stays %2F), and one with a "." or ".." segment once decoded is refused.
// Redirects aren't followed. A nil out, or one whose reference isn't canonical, is an error and no call
// (planner's call 4: no delegated call without a reference). The caller closes the response's
// body.
func (c *Client) Do(ctx context.Context, out *Outbound, req *http.Request) (*http.Response, error) {
	if out == nil {
		return nil, errors.New("delegation: a delegated call needs a caller reference (service.Call.Delegation)")
	}
	header := out.Reference.String()
	if header == "" {
		return nil, fmt.Errorf("delegation: the caller reference isn't canonical: %w", out.Reference.Valid())
	}
	if c.Credential == "" {
		return nil, fmt.Errorf("delegation: no service credential for %s", c.Receiver)
	}
	base, err := url.Parse(c.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("delegation: %s's BaseURL %q isn't an http(s) URL", c.Receiver, c.BaseURL)
	}
	target := *base
	if req.URL.Host != "" || req.URL.Scheme != "" {
		if !strings.EqualFold(req.URL.Scheme, base.Scheme) || !strings.EqualFold(req.URL.Host, base.Host) {
			return nil, fmt.Errorf("delegation: refusing to send %s's credential to %s://%s (BaseURL is %s://%s)", c.Receiver, req.URL.Scheme, req.URL.Host, base.Scheme, base.Host)
		}
	}
	// The request's path as escaped, so an encoded character (an ID's %2F) reaches the receiver
	// encoded; never one with a dot-segment once decoded, which could reach another route.
	if dotSegments(req.URL.Path) {
		return nil, fmt.Errorf("delegation: refusing a path with a dot-segment: %q", req.URL.EscapedPath())
	}
	escaped := req.URL.EscapedPath()
	if req.URL.Host == "" && req.URL.Scheme == "" {
		escaped = strings.TrimSuffix(base.EscapedPath(), "/") + escaped
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil {
		return nil, fmt.Errorf("delegation: %w", err)
	}
	target.Path, target.RawPath = decoded, escaped
	target.RawQuery, target.Fragment, target.User = req.URL.RawQuery, "", nil
	outReq := req.Clone(ctx)
	outReq.URL, outReq.Host = &target, ""
	outReq.Header = http.Header{}
	for _, name := range ForwardedHeaders {
		if values := req.Header.Values(name); len(values) > 0 {
			outReq.Header[name] = append([]string(nil), values...)
		}
	}
	outReq.Header.Set("Authorization", "Bearer "+c.Credential)
	outReq.Header.Set(Header, header)
	if out.ForwardedFor != "" {
		outReq.Header.Set("X-Forwarded-For", out.ForwardedFor)
	}
	if out.RequestID != "" {
		outReq.Header.Set("X-Request-Id", out.RequestID)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if c.HTTP != nil {
		copied := *c.HTTP
		client = &copied
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client.Do(outReq)
}
