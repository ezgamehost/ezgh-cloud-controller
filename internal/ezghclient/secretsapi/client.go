// Package secretsapi is a typed client for secrets-api: a project's secrets, their versions and
// field values, and the grants that let products read them.
//
// Only a create carries an Idempotency-Key (the secret's). Writing a version is never replayed
// by the client: after a lost answer the caller can't know whether the version was written, and
// writing again would make another one.
package secretsapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// Client calls secrets-api.
type Client struct {
	API *apiclient.Client
}

var p = apiclient.Path

func pageQuery(limit int, cursor string) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return q
}

func secretsPath(orgID, projectID string) string {
	return p("/v1/organizations/{}/projects/{}/secrets", orgID, projectID)
}

func secretPath(orgID, projectID, secretID string) string {
	return p("/v1/organizations/{}/projects/{}/secrets/{}", orgID, projectID, secretID)
}

func versionPath(orgID, projectID, secretID, version string) string {
	return secretPath(orgID, projectID, secretID) + p("/versions/{}", version)
}

// ListSecrets reads a page of a project's secrets; name, when set, asks for the secret of that
// name.
func (c *Client) ListSecrets(ctx context.Context, orgID, projectID, name string, limit int, cursor string) (apiclient.Page[Secret], error) {
	var out struct {
		Secrets    []Secret `json:"secrets"`
		NextCursor *string  `json:"nextCursor"`
	}
	q := pageQuery(limit, cursor)
	if name != "" {
		q.Set("name", name)
	}
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: secretsPath(orgID, projectID), Query: q}, &out)
	if out.Secrets == nil {
		out.Secrets = []Secret{}
	}
	return apiclient.Page[Secret]{Items: out.Secrets, NextCursor: out.NextCursor}, err
}

// GetSecret reads a secret by ID.
func (c *Client) GetSecret(ctx context.Context, orgID, projectID, secretID string) (*Secret, error) {
	var out Secret
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: secretPath(orgID, projectID, secretID)}, &out)
	return &out, err
}

// CreateSecret creates a secret (with its first version when body has fields), with an
// Idempotency-Key the service replays on a retry.
func (c *Client) CreateSecret(ctx context.Context, orgID, projectID string, body CreateBody) (*Secret, error) {
	var out Secret
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: secretsPath(orgID, projectID), Body: body, Create: true}, &out)
	return &out, err
}

// DeleteSecret erases a secret and every version. It can't be undone.
func (c *Client) DeleteSecret(ctx context.Context, orgID, projectID, secretID string) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodDelete, Path: secretPath(orgID, projectID, secretID)}, nil)
	return err
}

// CreateVersion writes a new version.
func (c *Client) CreateVersion(ctx context.Context, orgID, projectID, secretID string, body VersionBody) (*Version, error) {
	var out Version
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: secretPath(orgID, projectID, secretID) + "/versions", Body: body}, &out)
	return &out, err
}

// ListVersions reads a page of a secret's versions, newest first.
func (c *Client) ListVersions(ctx context.Context, orgID, projectID, secretID string, limit int, cursor string) (apiclient.Page[Version], error) {
	var out struct {
		Versions   []Version `json:"versions"`
		NextCursor *string   `json:"nextCursor"`
	}
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: secretPath(orgID, projectID, secretID) + "/versions", Query: pageQuery(limit, cursor)}, &out)
	if out.Versions == nil {
		out.Versions = []Version{}
	}
	return apiclient.Page[Version]{Items: out.Versions, NextCursor: out.NextCursor}, err
}

// GetVersion reads a version's metadata; version is a number or "latest".
func (c *Client) GetVersion(ctx context.Context, orgID, projectID, secretID, version string) (*Version, error) {
	var out Version
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: versionPath(orgID, projectID, secretID, version)}, &out)
	return &out, err
}

// ReadValue reads every field's value of a version (a billed read).
func (c *Client) ReadValue(ctx context.Context, orgID, projectID, secretID, version string) (*ValueAnswer, error) {
	var out ValueAnswer
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: versionPath(orgID, projectID, secretID, version) + "/value"}, &out)
	return &out, err
}

// ReadField reads one field's value of a version (a billed read).
func (c *Client) ReadField(ctx context.Context, orgID, projectID, secretID, version, field string) (*FieldAnswer, error) {
	var out FieldAnswer
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: versionPath(orgID, projectID, secretID, version) + p("/fields/{}", field)}, &out)
	return &out, err
}

// DestroyVersion destroys a version's values for good; its record stays, as destroyed.
func (c *Client) DestroyVersion(ctx context.Context, orgID, projectID, secretID string, version int) (*Version, error) {
	var out Version
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: versionPath(orgID, projectID, secretID, strconv.Itoa(version)) + "/destroy"}, &out)
	return &out, err
}

// ListGrants reads a page of a secret's grants.
func (c *Client) ListGrants(ctx context.Context, orgID, projectID, secretID string, limit int, cursor string) (apiclient.Page[Grant], error) {
	var out struct {
		Grants     []Grant `json:"grants"`
		NextCursor *string `json:"nextCursor"`
	}
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: secretPath(orgID, projectID, secretID) + "/grants", Query: pageQuery(limit, cursor)}, &out)
	if out.Grants == nil {
		out.Grants = []Grant{}
	}
	return apiclient.Page[Grant]{Items: out.Grants, NextCursor: out.NextCursor}, err
}

// CreateGrant lets an EZGH product read the secret for one of its resources (the registry's
// consumer, consumerResource a cache rule's resource name). A second grant for the same resource
// is 409 grant_exists.
func (c *Client) CreateGrant(ctx context.Context, orgID, projectID, secretID string, body GrantBody) (*Grant, error) {
	var out Grant
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: secretPath(orgID, projectID, secretID) + "/grants", Body: body, Create: true}, &out)
	return &out, err
}

// DeleteGrant revokes a grant.
func (c *Client) DeleteGrant(ctx context.Context, orgID, projectID, secretID, grantID string) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodDelete, Path: secretPath(orgID, projectID, secretID) + p("/grants/{}", grantID)}, nil)
	return err
}
