// Package ca signs nodes' CSRs with Private CA (certificates-api), on a product controller's platform
// Bot key, which no other process holds. Lifted from databases-stack's internal/certs (9ff61c1). Keys
// stay with whoever made them: a product holds no CA key and stores no node's private key.
package ca

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
)

// SANs replace whatever a CSR asks for.
type SANs struct {
	URI []string `json:"uri,omitempty"`
	IP  []string `json:"ip,omitempty"`
	DNS []string `json:"dns,omitempty"`
}

// CA is Private CA, on a product controller's platform Bot key: a customer's API like any other.
type CA struct {
	// BaseURL is certificates-api, in-cluster.
	BaseURL string
	// BotKey is the controller's Authorization.
	BotKey string
	// Organization, Project and Authority name EZGH Cloud Intermediate CA 1.
	Organization, Project, Authority string
	HTTP                             *http.Client
	// PollEvery is how often an issue the signer didn't finish at once is checked; zero is 1 s.
	PollEvery time.Duration
}

func (c *CA) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *CA) do(ctx context.Context, method, path, key string, body, into any) (int, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.BotKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, fmt.Errorf("certs: Private CA answered %d", resp.StatusCode)
	}
	return resp.StatusCode, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(into)
}

// Issued is a signed certificate: the leaf, what signed it, and when it ends.
type Issued struct {
	CertificatePEM string    `json:"certificatePem"`
	ChainPEM       []string  `json:"chainPem"`
	NotAfter       time.Time `json:"notAfter"`
}

// Chain is the leaf and what signed it, as a server presents them.
func (i *Issued) Chain() string {
	parts := make([]string, 0, 1+len(i.ChainPEM))
	for _, part := range append([]string{i.CertificatePEM}, i.ChainPEM...) {
		parts = append(parts, strings.TrimRight(part, "\n")+"\n")
	}
	return strings.Join(parts, "")
}

type operation struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Target struct {
		ID string `json:"id"`
	} `json:"target"`
}

// Issue signs csr. sans replaces whatever the CSR asks for. key makes a retry the same issue.
func (c *CA) Issue(ctx context.Context, key, csr, profile string, sans SANs, days int) (*Issued, error) {
	project := "/v1/organizations/" + url.PathEscape(c.Organization) + "/projects/" + url.PathEscape(c.Project)
	var answer struct {
		Issued
		operation
		// The first 202 wraps the operation. A replay of it is the operation alone.
		Operation *operation `json:"operation"`
	}
	body := map[string]any{"csr": csr, "profile": profile, "validityDays": days, "sans": sans}
	status, err := c.do(ctx, http.MethodPost, project+"/certificate-authorities/"+url.PathEscape(c.Authority)+"/certificates", key, body, &answer)
	if err != nil {
		return nil, err
	}
	if status == http.StatusCreated {
		return &answer.Issued, nil
	}
	op := answer.Operation
	if op == nil {
		op = &answer.operation
	}
	if op.ID == "" {
		return nil, errors.New("certs: an issue accepted with no operation")
	}
	every := c.PollEvery
	if every == 0 {
		every = time.Second
	}
	for op.Status != "succeeded" {
		if op.Status == "failed" || op.Status == "cancelled" {
			return nil, fmt.Errorf("certs: the issue %s", op.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
		if _, err := c.do(ctx, http.MethodGet, "/v1/organizations/"+url.PathEscape(c.Organization)+"/operations/"+url.PathEscape(op.ID), "", nil, op); err != nil {
			return nil, err
		}
	}
	var done Issued
	if _, err := c.do(ctx, http.MethodGet, project+"/private-certificates/"+url.PathEscape(op.Target.ID), "", nil, &done); err != nil {
		return nil, err
	}
	return &done, nil
}
