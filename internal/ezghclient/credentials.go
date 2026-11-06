// Package ezghclient is what the control-plane controllers call EZGH's public APIs with: ezgh-cli's
// API clients (copied by sync.sh, unedited) authenticated as the cluster's Bot by a
// CredentialSource, plus the errors the controllers turn into Kubernetes events.
package ezghclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// CredentialSource hands out the cluster Bot's current API key.
type CredentialSource interface {
	APIKey(ctx context.Context) (string, error)
}

// DefaultCredentialsURL is k8sagent's cluster credentials endpoint (D8).
const DefaultCredentialsURL = "http://127.0.0.1:9444/v1/cluster-credentials"

// refreshEarly is how long before its expiry a key is replaced.
const refreshEarly = 5 * time.Minute

// LocalSource reads short-lived cluster Bot keys from k8sagent. It holds a key until five minutes
// before it expires, and fetches one at a time however many callers ask at once.
type LocalSource struct {
	URL  string
	HTTP *http.Client
	Now  func() time.Time

	mu      sync.Mutex // held across a fetch: concurrent callers wait for it instead of fetching too
	key     string
	expires time.Time
}

// CredentialsError is a key that couldn't be had: k8sagent unreachable, or mgmt down behind it.
type CredentialsError struct {
	Err        error
	RetryAfter time.Duration
}

func (e *CredentialsError) Error() string {
	return "the cluster's EZGH credentials are unavailable: " + e.Err.Error()
}
func (e *CredentialsError) Unwrap() error { return e.Err }

// APIKey returns a key valid for at least five more minutes.
func (s *LocalSource) APIKey(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if s.key != "" && now().Before(s.expires.Add(-refreshEarly)) {
		return s.key, nil
	}
	key, expires, err := s.fetch(ctx)
	if err != nil {
		// A key that hasn't expired yet still works: keep using it through a blip.
		if s.key != "" && now().Before(s.expires) {
			return s.key, nil
		}
		s.key = ""
		return "", err
	}
	s.key, s.expires = key, expires
	return key, nil
}

// Invalidate drops the held key after a service refused it: only if it's still the refused one
// (refused is the key, or "" for whatever is held), so concurrent 401s cost one fetch.
func (s *LocalSource) Invalidate(refused string) {
	s.mu.Lock()
	if refused == "" || refused == s.key {
		s.key = ""
	}
	s.mu.Unlock()
}

func (s *LocalSource) fetch(ctx context.Context) (string, time.Time, error) {
	url := s.URL
	if url == "" {
		url = DefaultCredentialsURL
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second} // loopback: k8sagent answers at once or not at all
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", time.Time{}, &CredentialsError{Err: err}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, &CredentialsError{Err: err, RetryAfter: time.Second}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		msg := fmt.Sprintf("k8sagent answered %d", resp.StatusCode)
		if body.Error.Code != "" {
			msg += ": " + body.Error.Message + " (" + body.Error.Code + ")"
		}
		return "", time.Time{}, &CredentialsError{Err: errors.New(msg), RetryAfter: apiclient.RetryAfter(resp.Header.Get("Retry-After"))}
	}
	var body struct {
		APIKey    string    `json:"apiKey"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.APIKey == "" || body.ExpiresAt.IsZero() {
		return "", time.Time{}, &CredentialsError{Err: errors.New("k8sagent's answer has no apiKey and expiresAt")}
	}
	return body.APIKey, body.ExpiresAt, nil
}

// authorizer adapts a CredentialSource to the copied apiclient.
type authorizer struct{ src CredentialSource }

func (a authorizer) Authorization(ctx context.Context) (string, error) {
	key, err := a.src.APIKey(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + key, nil
}

// Refresh drops a refused key so the one retry fetches a fresh one.
func (a authorizer) Refresh(_ context.Context, refused string) (bool, error) {
	if inv, ok := a.src.(interface{ Invalidate(string) }); ok {
		inv.Invalidate(strings.TrimPrefix(refused, "Bearer "))
		return true, nil
	}
	return false, nil
}
