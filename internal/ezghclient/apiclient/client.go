// Package apiclient is what every platform API client is built on: it sends the credential,
// a User-Agent and a request ID, retries what the contracts say may be retried, honours
// Retry-After, and turns error answers into *Error.
//
// The per-service clients (orgapi, auditapi, quotasapi) are thin typed layers over Client.Do,
// with their wire types in their own types.go, so they can be replaced by generated code later
// without touching this package or the commands.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/ids"
)

// Authorizer provides the Authorization header. OAuth logins refresh their access token; API
// keys never change.
type Authorizer interface {
	// Authorization returns the header's value, e.g. "Bearer ezgh_…".
	Authorization(ctx context.Context) (string, error)
	// Refresh is called once after a 401, with the header that was refused. It reports whether
	// a retry could succeed with a new header.
	Refresh(ctx context.Context, refused string) (bool, error)
}

// Client calls one service's API.
type Client struct {
	// Service names the API in messages, by its public name ("Organizations", "Trails").
	Service string
	BaseURL string
	HTTP    *http.Client
	// Stream is used for downloads, which can take longer than HTTP's timeout; its transport
	// still bounds how long the server may take to start answering.
	Stream    *http.Client
	Auth      Authorizer
	UserAgent string
	// MaxAttempts bounds retries of 429s and 503s; default 4.
	MaxAttempts int
	// MaxWait is the longest Retry-After worth waiting for; a longer one fails at once, telling
	// the user when to try again. Default one minute.
	MaxWait time.Duration
	// Sleep waits between attempts; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Debug, when set, gets one line per request and answer. Headers are never written, so no
	// credential can end up in it.
	Debug io.Writer
}

// Request is one API call.
type Request struct {
	Method string
	// Path is the path below BaseURL, with every value escaped (build it with Path).
	Path  string
	Query url.Values
	// Body is sent as JSON when it isn't nil.
	Body any
	// RawBody is sent as it is, with ContentType, when Body is nil (a form).
	RawBody     []byte
	ContentType string
	Accept      string
	// Create marks a POST the service replays by its Idempotency-Key: a create, or another POST
	// whose operation declares the header. It carries a key, the same on every attempt, so a retry
	// after a lost answer is replayed by the service instead of done twice
	// (contracts/api-conventions.md#idempotency-keys).
	Create bool
	// RetryIf, when set, replaces the usual rule for which error answers are retried: it's for a
	// request that must not be repeated once the service has done its work, so only answers that
	// say no work was done are retried. quota_exceeded never is, whatever RetryIf says.
	RetryIf func(*Error) bool
	// IfMatch is the ETag the client read, for a write that replaces something whole
	// (contracts/api-conventions.md#conditional-writes). A stale one is 412 precondition_failed.
	IfMatch string
	// Upload, when set, is the body, streamed rather than held in memory: it's opened afresh for
	// every attempt (a file read from disk again), and sent with ContentType and exactly
	// ContentLength bytes. An upload goes with the Stream client, since sending it can take longer
	// than the timeout; the timeout then bounds the wait for the answer once it's sent. An error
	// from Upload itself is returned as it is, never retried.
	Upload        func() (io.ReadCloser, error)
	ContentLength int64
	// IdempotencyKey is a key the caller chose, for a POST whose operation declares the header:
	// one derived from what's sent, so that a later run sending the same thing is replayed too.
	// It's retried like a create. Create generates a key instead.
	IdempotencyKey string
	// Header is other request headers (Range).
	Header http.Header
}

// Response is a successful answer's status and headers; the body has been decoded.
type Response struct {
	Status int
	Header http.Header
}

// Path fills a path template's {} placeholders with escaped values:
// Path("/v1/organizations/{}/projects/{}", orgID, projectID). A value from the command line can
// never change which endpoint is called: "/" is escaped, and "." and ".." (which a server
// would resolve as path steps) are escaped too.
func Path(template string, values ...string) string {
	var b strings.Builder
	parts := strings.Split(template, "{}")
	if len(parts)-1 != len(values) {
		panic(fmt.Sprintf("apiclient.Path: %d placeholders, %d values", len(parts)-1, len(values)))
	}
	for i, part := range parts {
		b.WriteString(part)
		if i < len(values) {
			v := url.PathEscape(values[i])
			if v == "" || strings.Trim(v, ".") == "" {
				v = strings.ReplaceAll(v, ".", "%2E")
				if v == "" {
					v = "%20"
				}
			}
			b.WriteString(v)
		}
	}
	return b.String()
}

// Do sends req and decodes a successful JSON answer into out (unless out is nil or the answer is
// 204).
func (c *Client) Do(ctx context.Context, req Request, out any) (*Response, error) {
	resp, err := c.send(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	result := &Response{Status: resp.StatusCode, Header: resp.Header}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, resp.Body)
		return result, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &TransportError{Service: c.Service, URL: c.BaseURL + req.Path, Err: err}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return nil, fmt.Errorf("%s answered %d with a body that isn't what the CLI expects: %w", c.Service, resp.StatusCode, err)
	}
	return result, nil
}

// Open sends req and returns the answer's body for streaming. The caller closes it.
func (c *Client) Open(ctx context.Context, req Request) (io.ReadCloser, http.Header, error) {
	resp, err := c.send(ctx, req, true)
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, resp.Header, nil
}

func (c *Client) send(ctx context.Context, req Request, stream bool) (*http.Response, error) {
	maxAttempts := c.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 4
	}
	maxWait := c.MaxWait
	if maxWait <= 0 {
		maxWait = time.Minute
	}
	body, contentType := req.RawBody, req.ContentType
	if req.Body != nil {
		var err error
		if body, err = json.Marshal(req.Body); err != nil {
			return nil, err
		}
		contentType = "application/json"
	}
	// Only creates carry an Idempotency-Key, one for every attempt of the call: the service
	// replays the first success instead of creating twice. Go's transport resends a request that
	// has one when a reused connection drops before the answer, which is safe exactly because the
	// service honours the key. Any other POST (accepting an invitation, leaving an organization)
	// goes without one, so neither the transport nor this loop ever sends it twice.
	idempotencyKey := req.IdempotencyKey
	if idempotencyKey == "" && req.Create {
		idempotencyKey = ids.NewV7()
	}
	refreshed := false
	for attempt := 1; ; attempt++ {
		// A credential that can't be used (a login whose refresh was refused) fails the call as
		// itself: no request was sent, so it isn't retried like one that got no answer.
		authorization := ""
		if c.Auth != nil {
			var err error
			if authorization, err = c.Auth.Authorization(ctx); err != nil {
				return nil, err
			}
		}
		var upload io.ReadCloser
		if req.Upload != nil {
			var err error
			if upload, err = req.Upload(); err != nil {
				return nil, err
			}
		}
		resp, requestID, err := c.attempt(ctx, req, body, contentType, idempotencyKey, authorization, upload, stream || req.Upload != nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// Without an answer there's no knowing whether a write happened, so only reads, and
			// creates the service will replay by their key, are tried again.
			if (idempotent(req.Method) || idempotencyKey != "") && attempt < maxAttempts {
				if err := c.sleep(ctx, time.Duration(attempt)*time.Second); err != nil {
					return nil, err
				}
				continue
			}
			return nil, &TransportError{Service: c.Service, URL: c.BaseURL + req.Path, Err: err}
		}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		apiErr := parseError(c.Service, resp.StatusCode, resp.Header, data)
		if apiErr.RequestID == "" {
			apiErr.RequestID = requestID
		}
		// An access token can expire (or be revoked and replaced by another ezgh process)
		// between the check and the call: refresh once and try again.
		if resp.StatusCode == http.StatusUnauthorized && c.Auth != nil && !refreshed && authorization != "" {
			refreshed = true
			ok, err := c.Auth.Refresh(ctx, authorization)
			if err != nil {
				return nil, err
			}
			if ok {
				attempt--
				continue
			}
		}
		retry := retryable(apiErr)
		if req.RetryIf != nil {
			retry = !apiErr.QuotaExceeded() && req.RetryIf(apiErr)
		}
		if retry && attempt < maxAttempts {
			wait := apiErr.RetryAfter
			if wait == 0 {
				wait = time.Second
			}
			if wait <= maxWait {
				c.debugf("   retrying in %s (%s)", wait, apiErr.Code)
				if err := c.sleep(ctx, wait); err != nil {
					return nil, err
				}
				continue
			}
		}
		return nil, apiErr
	}
}

func (c *Client) attempt(ctx context.Context, req Request, body []byte, contentType, idempotencyKey, authorization string, upload io.ReadCloser, stream bool) (*http.Response, string, error) {
	u := c.BaseURL + req.Path
	if len(req.Query) > 0 {
		u += "?" + req.Query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	if upload != nil {
		reader = upload
	}
	r, err := http.NewRequestWithContext(ctx, req.Method, u, reader)
	if err != nil {
		if upload != nil {
			_ = upload.Close()
		}
		return nil, "", err
	}
	if upload != nil {
		// Exactly this many bytes, and a way for Go's transport to send them again if a reused
		// connection drops first (it does so only for a request with an Idempotency-Key).
		r.ContentLength = req.ContentLength
		r.GetBody = req.Upload
	}
	for k, v := range req.Header {
		r.Header[k] = v
	}
	requestID := ids.NewV7()
	r.Header.Set("X-Request-Id", requestID)
	r.Header.Set("User-Agent", c.UserAgent)
	accept := req.Accept
	if accept == "" {
		accept = "application/json"
	}
	r.Header.Set("Accept", accept)
	if body != nil || upload != nil {
		r.Header.Set("Content-Type", contentType)
	}
	if req.IfMatch != "" {
		r.Header.Set("If-Match", req.IfMatch)
	}
	if idempotencyKey != "" {
		r.Header.Set("Idempotency-Key", idempotencyKey)
	}
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	client := c.HTTP
	if stream && c.Stream != nil {
		client = c.Stream
	}
	if client == nil {
		client = http.DefaultClient
	}
	started := time.Now()
	c.debugf("-> %s %s (request %s)", req.Method, u, requestID)
	resp, err := client.Do(r)
	if err != nil {
		c.debugf("<- %v", err)
		// The error from client.Do names the URL, which is already in the TransportError.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, requestID, err
	}
	c.debugf("<- %d in %s", resp.StatusCode, time.Since(started).Round(time.Millisecond))
	return resp, requestID, nil
}

// retryable: rate limits other than quotas, services that are briefly unavailable, and a
// duplicate request still in flight. Never quota_exceeded, even from a service that sends a
// Retry-After with it.
func retryable(e *Error) bool {
	if e.QuotaExceeded() {
		return false
	}
	switch e.Status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true
	case http.StatusConflict:
		return e.Code == "idempotency_in_progress"
	}
	return false
}

func idempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) debugf(format string, args ...any) {
	if c.Debug != nil {
		fmt.Fprintf(c.Debug, "[%s] "+format+"\n", append([]any{c.Service}, args...)...)
	}
}
