// Package registration registers a product with the platform on every deploy, before the new
// pods roll (the chart's registration Job):
//
//   - its IAM surface with org-api (contracts/registration.md, "The registration API"): PUT
//     /internal/v1/registrations/:namespace with the manifest's IAM part and the namespace's rk_
//     credential, so new code checks new actions from its first request;
//   - then its quotas with quotas-api (contracts/quotas.md, "Registering quotas"): the manifest's
//     quotas and its in-cluster URL, with the namespace's qk_ credential.
package registration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// Client registers one namespace.
type Client struct {
	// OrgAPIURL is org-api, in-cluster.
	OrgAPIURL string
	// Credential is the namespace's rk_<16>.<secret>. Empty sends InternalToken as
	// X-Internal-Token instead, which org-api accepts only with APP_ENV=local.
	Credential    string
	InternalToken string
	HTTP          *http.Client
	Logger        *slog.Logger
	// Deadline bounds retries of 5xx and network errors (default 10 minutes).
	Deadline time.Duration
}

// Change is one entry of the response's changes.
type Change struct {
	Kind   string   `json:"kind"`
	ID     string   `json:"id"`
	Change string   `json:"change"`
	Fields []string `json:"fields,omitempty"`
}

// Result is org-api's 200 answer.
type Result struct {
	Namespace    string   `json:"namespace"`
	EventSource  string   `json:"eventSource"`
	Revision     int      `json:"revision"`
	Digest       string   `json:"digest"`
	DryRun       bool     `json:"dryRun"`
	Changes      []Change `json:"changes"`
	RegisteredAt string   `json:"registeredAt"`
	UpdatedAt    string   `json:"updatedAt"`
}

// Refused is a 4xx: the registration can't succeed as it is, so the Job fails at once and the
// sync stops, and the old pods keep serving.
type Refused struct {
	// Service is org-api or quotas-api.
	Service string
	Status  int
	Body    string
}

func (e *Refused) Error() string {
	return fmt.Sprintf("%s refused the registration (%d): %s", e.Service, e.Status, e.Body)
}

// NotFound reports a 404 for the registration route itself: a service that doesn't have the
// registration API yet.
func NotFound(err error) bool {
	var r *Refused
	return errors.As(err, &r) && r.Status == http.StatusNotFound
}

// Register PUTs the manifest's registration. It retries 5xx and network errors with backoff up
// to the deadline; a 4xx fails at once.
func (c *Client) Register(ctx context.Context, m *manifest.Manifest, dryRun bool) (*Result, error) {
	if issues := manifest.RegistrationIssues(m.Registration()); len(issues) > 0 {
		return nil, fmt.Errorf("the registration breaks the rules: %v", issues)
	}
	var r Result
	err := put(ctx, putter{
		service: "org-api", url: c.OrgAPIURL + "/internal/v1/registrations/" + url.PathEscape(m.Namespace),
		credential: c.Credential, internalToken: c.InternalToken, http: c.HTTP, logger: c.Logger, deadline: c.Deadline,
	}, m.Registration(), dryRun, &r)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// QuotasClient registers one namespace's quotas with quotas-api (contracts/quotas.md,
// "Registering quotas"), after org-api's registration, with the namespace's qk_ credential.
type QuotasClient struct {
	// QuotasAPIURL is quotas-api, in-cluster.
	QuotasAPIURL string
	// Credential is the namespace's qk_<16>.<secret>. Empty sends InternalToken as
	// X-Internal-Token instead, which quotas-api accepts only with APP_ENV=local.
	Credential    string
	InternalToken string
	// InternalURL is the chart's own Service, where quotas-api reads allocation usage.
	InternalURL string
	HTTP        *http.Client
	Logger      *slog.Logger
	// Deadline bounds retries of 5xx and network errors (default 10 minutes).
	Deadline time.Duration
}

// QuotasResult is quotas-api's 200 answer.
type QuotasResult struct {
	Namespace    string   `json:"namespace"`
	Revision     int      `json:"revision"`
	Digest       string   `json:"digest"`
	DryRun       bool     `json:"dryRun"`
	Changes      []Change `json:"changes"`
	RegisteredAt string   `json:"registeredAt"`
	UpdatedAt    string   `json:"updatedAt"`
}

// Register PUTs the manifest's quotas, retrying 5xx and network errors up to the deadline; a 4xx
// fails at once, and with it the Job.
func (c *QuotasClient) Register(ctx context.Context, m *manifest.Manifest, dryRun bool) (*QuotasResult, error) {
	if c.InternalURL == "" && m.NeedsInternalURL() {
		return nil, errors.New("INTERNAL_URL is required: quotas-api reads the namespace's allocation usage from it")
	}
	var r QuotasResult
	err := put(ctx, putter{
		service: "quotas-api", url: c.QuotasAPIURL + "/internal/v1/registrations/" + url.PathEscape(m.Namespace),
		credential: c.Credential, internalToken: c.InternalToken, http: c.HTTP, logger: c.Logger, deadline: c.Deadline,
	}, m.QuotasRegistration(c.InternalURL), dryRun, &r)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

type putter struct {
	service                   string
	url                       string
	credential, internalToken string
	http                      *http.Client
	logger                    *slog.Logger
	deadline                  time.Duration
}

func put(ctx context.Context, p putter, payload any, dryRun bool, into any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	u := p.url
	if dryRun {
		u += "?dryRun=true"
	}
	deadline := p.deadline
	if deadline == 0 {
		deadline = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	client := p.http
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	logger := p.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	wait := 2 * time.Second
	for attempt := 1; ; attempt++ {
		retry, err := p.once(ctx, client, u, body, into)
		if err == nil || !retry {
			return err
		}
		logger.Warn("registration failed; retrying", "service", p.service, "attempt", attempt, "error", err.Error(), "in", wait.String())
		select {
		case <-ctx.Done():
			return fmt.Errorf("registration with %s: gave up: %w", p.service, err)
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

func (p putter) once(ctx context.Context, client *http.Client, u string, body []byte, into any) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.credential != "" {
		req.Header.Set("Authorization", "Bearer "+p.credential)
	} else {
		req.Header.Set("X-Internal-Token", p.internalToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return true, err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusOK:
		if err := json.Unmarshal(answer, into); err != nil {
			return false, fmt.Errorf("registration with %s: unexpected answer: %w", p.service, err)
		}
		return false, nil
	case resp.StatusCode >= 500:
		return true, fmt.Errorf("%s answered %d", p.service, resp.StatusCode)
	default:
		return false, &Refused{Service: p.service, Status: resp.StatusCode, Body: string(answer)}
	}
}
