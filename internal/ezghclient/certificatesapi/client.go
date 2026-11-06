// Package certificatesapi is a typed client for certificates-api's Private CA routes (EZGH Web
// Certificates): a project's certificate authorities, the certificates they issue, their ACME
// bindings, and the operations that finish the asynchronous changes.
//
// Every write the API declares one for carries an Idempotency-Key, the same on every attempt of
// the call, so a retry after a lost answer is replayed rather than done twice. Export-key carries
// none: no replay of an exported key is ever kept. Writes to a CA or a certificate take the ETag
// it was read with as If-Match.
package certificatesapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/ids"
)

// Client calls certificates-api.
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

func projectPath(orgID, projectID, rest string, values ...string) string {
	return p("/v1/organizations/{}/projects/{}", orgID, projectID) + p(rest, values...)
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) (string, error) {
	resp, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: path, Query: query}, out)
	if err != nil {
		return "", err
	}
	return resp.Header.Get("ETag"), nil
}

// write sends a change with an Idempotency-Key (one per call, reused by its retries), conditional
// on ifMatch when it's set.
func (c *Client) write(ctx context.Context, method, path string, body, out any, ifMatch string) (string, error) {
	resp, err := c.API.Do(ctx, apiclient.Request{Method: method, Path: path, Body: body, IfMatch: ifMatch, IdempotencyKey: ids.NewV7()}, out)
	if err != nil {
		return "", err
	}
	return resp.Header.Get("ETag"), nil
}

// Certificate authorities.

// ListAuthorities reads a page of the project's CAs.
func (c *Client) ListAuthorities(ctx context.Context, orgID, projectID string, limit int, cursor string) (apiclient.Page[Authority], error) {
	var out authorityPage
	_, err := c.get(ctx, projectPath(orgID, projectID, "/certificate-authorities"), pageQuery(limit, cursor), &out)
	if out.CertificateAuthorities == nil {
		out.CertificateAuthorities = []Authority{}
	}
	return apiclient.Page[Authority]{Items: out.CertificateAuthorities, NextCursor: out.NextCursor}, err
}

// GetAuthority reads a CA and its ETag.
func (c *Client) GetAuthority(ctx context.Context, orgID, projectID, caID string) (*Authority, string, error) {
	var out Authority
	etag, err := c.get(ctx, projectPath(orgID, projectID, "/certificate-authorities/{}", caID), nil, &out)
	return &out, etag, err
}

// CreateAuthority creates a CA: 202 with the CA (creating) and the operation that generates its
// key and signs it.
func (c *Client) CreateAuthority(ctx context.Context, orgID, projectID string, body AuthorityCreate) (*Accepted, error) {
	var out Accepted
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificate-authorities"), body, &out, "")
	return &out, err
}

// UpdateAuthority changes a CA if ifMatch is still its ETag.
func (c *Client) UpdateAuthority(ctx context.Context, orgID, projectID, caID string, body AuthorityUpdate, ifMatch string) (*Authority, error) {
	var out Authority
	_, err := c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/certificate-authorities/{}", caID), body, &out, ifMatch)
	return &out, err
}

// DeleteAuthority schedules a CA's deletion (pendingDeletion until the window ends).
func (c *Client) DeleteAuthority(ctx context.Context, orgID, projectID, caID string, body AuthorityDelete, ifMatch string) (*Authority, error) {
	var out Authority
	_, err := c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/certificate-authorities/{}", caID), body, &out, ifMatch)
	return &out, err
}

// authorityAction posts one of a CA's lifecycle actions (disable, enable, restore, revoke).
func (c *Client) authorityAction(ctx context.Context, orgID, projectID, caID, action string, body any, ifMatch string) (*Authority, error) {
	if body == nil {
		body = empty{}
	}
	var out Authority
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificate-authorities/{}/"+action, caID), body, &out, ifMatch)
	return &out, err
}

// DisableAuthority stops a CA signing; its CRLs and OCSP are still published.
func (c *Client) DisableAuthority(ctx context.Context, orgID, projectID, caID, ifMatch string) (*Authority, error) {
	return c.authorityAction(ctx, orgID, projectID, caID, "disable", nil, ifMatch)
}

// EnableAuthority lets a disabled CA sign again.
func (c *Client) EnableAuthority(ctx context.Context, orgID, projectID, caID, ifMatch string) (*Authority, error) {
	return c.authorityAction(ctx, orgID, projectID, caID, "enable", nil, ifMatch)
}

// RestoreAuthority cancels a scheduled deletion; the CA comes back disabled.
func (c *Client) RestoreAuthority(ctx context.Context, orgID, projectID, caID, ifMatch string) (*Authority, error) {
	return c.authorityAction(ctx, orgID, projectID, caID, "restore", nil, ifMatch)
}

// RevokeAuthority revokes a hosted subordinate through its parent.
func (c *Client) RevokeAuthority(ctx context.Context, orgID, projectID, caID, reason, ifMatch string) (*Authority, error) {
	return c.authorityAction(ctx, orgID, projectID, caID, "revoke", Revoke{Reason: reason}, ifMatch)
}

// RenewAuthority reissues a CA's certificate for the same key: 202 with the operation.
func (c *Client) RenewAuthority(ctx context.Context, orgID, projectID, caID, ifMatch string) (*Accepted, error) {
	var out Accepted
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificate-authorities/{}/renew", caID), empty{}, &out, ifMatch)
	return &out, err
}

// GetAuthorityCSR reads an external subordinate's CSR.
func (c *Client) GetAuthorityCSR(ctx context.Context, orgID, projectID, caID string) (*AuthorityCSR, error) {
	var out AuthorityCSR
	_, err := c.get(ctx, projectPath(orgID, projectID, "/certificate-authorities/{}/csr", caID), nil, &out)
	return &out, err
}

// ImportAuthorityCertificate imports an external subordinate's signed chain: 202 with the
// operation that activates it.
func (c *Client) ImportAuthorityCertificate(ctx context.Context, orgID, projectID, caID string, body AuthorityImport, ifMatch string) (*Accepted, error) {
	var out Accepted
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificate-authorities/{}/import-certificate", caID), body, &out, ifMatch)
	return &out, err
}

// Private certificates.

// ListPrivateCertificates reads a page of a CA's issued certificates, newest first.
func (c *Client) ListPrivateCertificates(ctx context.Context, orgID, projectID, caID string, limit int, cursor string) (apiclient.Page[PrivateCertificate], error) {
	var out privateCertificatePage
	_, err := c.get(ctx, projectPath(orgID, projectID, "/certificate-authorities/{}/certificates", caID), pageQuery(limit, cursor), &out)
	if out.PrivateCertificates == nil {
		out.PrivateCertificates = []PrivateCertificate{}
	}
	return apiclient.Page[PrivateCertificate]{Items: out.PrivateCertificates, NextCursor: out.NextCursor}, err
}

// PrivateCertificateFilters narrow ListProjectPrivateCertificates; empty fields don't filter.
type PrivateCertificateFilters struct {
	CertificateAuthorityID string
	// issued (in use: not revoked, not expired), revoked or expired.
	Status string
	// csr, generated, managed or acme.
	KeySource string
	// Part of a common name or SAN, or the start of a serial (hex) or slug.
	Search string
	// RFC 3339: certificates that expire before it.
	ExpiresBefore string
}

// ListProjectPrivateCertificates reads a page of the project's certificates across its CAs, newest
// first (ListAllPrivateCertificates). A cursor only goes with the filters it came with.
func (c *Client) ListProjectPrivateCertificates(ctx context.Context, orgID, projectID string, filters PrivateCertificateFilters, limit int, cursor string) (apiclient.Page[PrivateCertificate], error) {
	q := pageQuery(limit, cursor)
	for name, value := range map[string]string{
		"certificateAuthorityId": filters.CertificateAuthorityID,
		"status":                 filters.Status,
		"keySource":              filters.KeySource,
		"search":                 filters.Search,
		"expiresBefore":          filters.ExpiresBefore,
	} {
		if value != "" {
			q.Set(name, value)
		}
	}
	var out privateCertificatePage
	_, err := c.get(ctx, projectPath(orgID, projectID, "/private-certificates"), q, &out)
	if out.PrivateCertificates == nil {
		out.PrivateCertificates = []PrivateCertificate{}
	}
	return apiclient.Page[PrivateCertificate]{Items: out.PrivateCertificates, NextCursor: out.NextCursor}, err
}

// IssueCertificate issues from a CA: the certificate (201) or the operation that finishes it (202).
func (c *Client) IssueCertificate(ctx context.Context, orgID, projectID, caID string, body Issue) (*Issued, error) {
	var out Issued
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificate-authorities/{}/certificates", caID), body, &out, "")
	return &out, err
}

// GetPrivateCertificate reads a certificate and its ETag.
func (c *Client) GetPrivateCertificate(ctx context.Context, orgID, projectID, id string) (*PrivateCertificate, string, error) {
	var out PrivateCertificate
	etag, err := c.get(ctx, projectPath(orgID, projectID, "/private-certificates/{}", id), nil, &out)
	return &out, etag, err
}

// RevokePrivateCertificate revokes a certificate; it's final.
func (c *Client) RevokePrivateCertificate(ctx context.Context, orgID, projectID, id, reason, ifMatch string) (*PrivateCertificate, error) {
	var out PrivateCertificate
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/private-certificates/{}/revoke", id), Revoke{Reason: reason}, &out, ifMatch)
	return &out, err
}

// ExportKey exports a generated key, encrypted with the passphrase. No Idempotency-Key.
func (c *Client) ExportKey(ctx context.Context, orgID, projectID, id, passphrase string) (*Exported, error) {
	var out Exported
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/private-certificates/{}/export-key", id), Body: Export{Passphrase: passphrase}}, &out)
	return &out, err
}

// ForgetKey destroys the key EZGH holds for a certificate.
func (c *Client) ForgetKey(ctx context.Context, orgID, projectID, id, ifMatch string) (*PrivateCertificate, error) {
	var out PrivateCertificate
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/private-certificates/{}/forget-key", id), empty{}, &out, ifMatch)
	return &out, err
}

// DeletePrivateCertificate deletes a certificate's record (its serial stays in its CA's
// revocation data).
func (c *Client) DeletePrivateCertificate(ctx context.Context, orgID, projectID, id, ifMatch string) error {
	_, err := c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/private-certificates/{}", id), empty{}, nil, ifMatch)
	return err
}

// ACME bindings.

// ListBindings reads a page of a CA's ACME bindings.
func (c *Client) ListBindings(ctx context.Context, orgID, projectID, caID string, limit int, cursor string) (apiclient.Page[Binding], error) {
	var out bindingPage
	_, err := c.get(ctx, projectPath(orgID, projectID, "/certificate-authorities/{}/acme-bindings", caID), pageQuery(limit, cursor), &out)
	if out.ACMEBindings == nil {
		out.ACMEBindings = []Binding{}
	}
	return apiclient.Page[Binding]{Items: out.ACMEBindings, NextCursor: out.NextCursor}, err
}

// GetBinding reads an ACME binding and its ETag.
func (c *Client) GetBinding(ctx context.Context, orgID, projectID, caID, bindingID string) (*Binding, string, error) {
	var out Binding
	etag, err := c.get(ctx, projectPath(orgID, projectID, "/certificate-authorities/{}/acme-bindings/{}", caID, bindingID), nil, &out)
	return &out, etag, err
}

// CreateBinding creates an ACME binding owned by the caller; the answer carries its HMAC key once.
func (c *Client) CreateBinding(ctx context.Context, orgID, projectID, caID string, body BindingCreate) (*BindingCreated, error) {
	var out BindingCreated
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificate-authorities/{}/acme-bindings", caID), body, &out, "")
	return &out, err
}

// DeleteBinding deletes an ACME binding: accounts bound with it stop working.
func (c *Client) DeleteBinding(ctx context.Context, orgID, projectID, caID, bindingID, ifMatch string) error {
	_, err := c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/certificate-authorities/{}/acme-bindings/{}", caID, bindingID), nil, nil, ifMatch)
	return err
}

// Operations.

// GetOperation reads an operation.
func (c *Client) GetOperation(ctx context.Context, orgID, operationID string) (*Operation, error) {
	var out Operation
	_, err := c.get(ctx, p("/v1/organizations/{}/operations/{}", orgID, operationID), nil, &out)
	return &out, err
}

// Domain authorizations (organization-wide).

func orgAuthorizations(orgID, rest string, values ...string) string {
	return p("/v1/organizations/{}/domain-authorizations", orgID) + p(rest, values...)
}

// ListDomainAuthorizations reads a page of the organization's domain authorizations.
func (c *Client) ListDomainAuthorizations(ctx context.Context, orgID string, limit int, cursor string) (apiclient.Page[DomainAuthorization], error) {
	var out domainAuthorizationPage
	_, err := c.get(ctx, orgAuthorizations(orgID, ""), pageQuery(limit, cursor), &out)
	if out.DomainAuthorizations == nil {
		out.DomainAuthorizations = []DomainAuthorization{}
	}
	return apiclient.Page[DomainAuthorization]{Items: out.DomainAuthorizations, NextCursor: out.NextCursor}, err
}

// GetDomainAuthorization reads an authorization.
func (c *Client) GetDomainAuthorization(ctx context.Context, orgID, id string) (*DomainAuthorization, error) {
	var out DomainAuthorization
	_, err := c.get(ctx, orgAuthorizations(orgID, "/{}", id), nil, &out)
	return &out, err
}

// CreateDomainAuthorization creates an authorization (201), or answers the existing one for the
// domain (200), linked when the body names a zone.
func (c *Client) CreateDomainAuthorization(ctx context.Context, orgID string, body DomainAuthorizationCreate) (*DomainAuthorization, error) {
	var out DomainAuthorization
	_, err := c.write(ctx, http.MethodPost, orgAuthorizations(orgID, ""), body, &out, "")
	return &out, err
}

// UpdateDomainAuthorization changes an authorization.
func (c *Client) UpdateDomainAuthorization(ctx context.Context, orgID, id string, body DomainAuthorizationUpdate) (*DomainAuthorization, error) {
	var out DomainAuthorization
	_, err := c.write(ctx, http.MethodPatch, orgAuthorizations(orgID, "/{}", id), body, &out, "")
	return &out, err
}

// RotateDomainAuthorization issues the authorization a new validation token (and rewrites the
// linked zone's records).
func (c *Client) RotateDomainAuthorization(ctx context.Context, orgID, id string) (*DomainAuthorization, error) {
	var out DomainAuthorization
	_, err := c.write(ctx, http.MethodPost, orgAuthorizations(orgID, "/{}/rotate-binding", id), empty{}, &out, "")
	return &out, err
}

// DeleteDomainAuthorization deletes an authorization (and the record sets it wrote).
func (c *Client) DeleteDomainAuthorization(ctx context.Context, orgID, id string) error {
	_, err := c.write(ctx, http.MethodDelete, orgAuthorizations(orgID, "/{}", id), nil, nil, "")
	return err
}

// Web certificates (project).

// ListCertificates reads a page of the project's certificates.
func (c *Client) ListCertificates(ctx context.Context, orgID, projectID string, limit int, cursor string) (apiclient.Page[Certificate], error) {
	var out certificatePage
	_, err := c.get(ctx, projectPath(orgID, projectID, "/certificates"), pageQuery(limit, cursor), &out)
	if out.Certificates == nil {
		out.Certificates = []Certificate{}
	}
	return apiclient.Page[Certificate]{Items: out.Certificates, NextCursor: out.NextCursor}, err
}

// GetCertificate reads a certificate.
func (c *Client) GetCertificate(ctx context.Context, orgID, projectID, id string) (*Certificate, error) {
	var out Certificate
	_, err := c.get(ctx, projectPath(orgID, projectID, "/certificates/{}", id), nil, &out)
	return &out, err
}

// CreateCertificate requests a certificate: 202 with the operation.
func (c *Client) CreateCertificate(ctx context.Context, orgID, projectID string, body CertificateCreate) (*Operation, error) {
	var out Operation
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificates"), body, &out, "")
	return &out, err
}

// DeleteCertificate deletes a certificate: 202 with the operation.
func (c *Client) DeleteCertificate(ctx context.Context, orgID, projectID, id string) (*Operation, error) {
	var out Operation
	_, err := c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/certificates/{}", id), nil, &out, "")
	return &out, err
}
