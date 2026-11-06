package certificatesapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/openapitest"
)

// The client against certificates-api's own OpenAPI document (api/openapi/certificates-api.json,
// rendered from certificates-stack's manifest and overlay).
func TestClientMatchesCertificatesAPIsOpenAPI(t *testing.T) {
	spec := openapitest.Load(t, "../openapi/certificates-api.json")
	rec := openapitest.NewRecorder(t)
	c := &Client{API: &apiclient.Client{Service: "Web Certificates", BaseURL: rec.URL, HTTP: rec.Client()}}
	ctx := context.Background()
	o, p := "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	ca, pct, eab, op := "0199a3c2-7000-7000-8000-000000000001", "0199a3c2-7000-7000-8000-000000000002", "0199a3c2-7000-7000-8000-000000000003", "0199a3c2-7000-7000-8000-000000000004"
	dau := "0199a3c2-7000-7000-8000-000000000005"
	zero, days, name := 0, 30, "corp-issuing"
	tags := map[string]string{"team": "platform"}
	sans := &SANs{DNS: []string{"a.corp.example"}, IP: []string{"10.0.0.7"}, URI: []string{"spiffe://corp.example/a"}, Email: []string{}}
	calls := []openapitest.Call{
		{Name: "ListCertificateAuthorities", Run: func() error { _, err := c.ListAuthorities(ctx, o, p, 20, "c"); return err }, Response: authorityPage{}},
		{Name: "GetCertificateAuthority", Run: func() error { _, _, err := c.GetAuthority(ctx, o, p, ca); return err }, Response: Authority{}},
		{Name: "CreateCertificateAuthority", Run: func() error {
			_, err := c.CreateAuthority(ctx, o, p, AuthorityCreate{
				Name: name, Description: "d", Tags: tags, Type: "subordinate", ParentID: ca, ParentKind: "hosted",
				Subject:      SubjectInput{CommonName: "Corp Issuing", Organization: []string{"Corp"}, OrganizationalUnit: []string{"PKI"}, Country: []string{"US"}, Province: []string{"CA"}, Locality: []string{"SF"}},
				KeyAlgorithm: "ecdsa-p256", PathLength: &zero, NameConstraints: &NameConstraints{PermittedDNS: []string{"corp.example"}},
				ValidityDays: 1825, MaxLeafValidityDays: 90,
			})
			return err
		}, Request: AuthorityCreate{}, Response: Accepted{}},
		{Name: "UpdateCertificateAuthority", Run: func() error {
			_, err := c.UpdateAuthority(ctx, o, p, ca, AuthorityUpdate{Name: &name, Description: &name, Tags: &tags, MaxLeafValidityDays: &days}, `"e1"`)
			return err
		}, Request: AuthorityUpdate{}, Response: Authority{}},
		{Name: "DeleteCertificateAuthority", Run: func() error {
			_, err := c.DeleteAuthority(ctx, o, p, ca, AuthorityDelete{DeletionWindowDays: 7}, `"e1"`)
			return err
		}, Request: AuthorityDelete{}, Response: Authority{}},
		{Name: "DisableCertificateAuthority", Run: func() error { _, err := c.DisableAuthority(ctx, o, p, ca, `"e1"`); return err }, Request: empty{}, Response: Authority{}},
		{Name: "EnableCertificateAuthority", Run: func() error { _, err := c.EnableAuthority(ctx, o, p, ca, `"e1"`); return err }, Request: empty{}, Response: Authority{}},
		{Name: "RestoreCertificateAuthority", Run: func() error { _, err := c.RestoreAuthority(ctx, o, p, ca, `"e1"`); return err }, Request: empty{}, Response: Authority{}},
		{Name: "RevokeCertificateAuthority", Run: func() error { _, err := c.RevokeAuthority(ctx, o, p, ca, "superseded", `"e1"`); return err }, Request: Revoke{}, Response: Authority{}},
		{Name: "RenewCertificateAuthority", Run: func() error { _, err := c.RenewAuthority(ctx, o, p, ca, `"e1"`); return err }, Request: empty{}, Response: Accepted{}},
		{Name: "GetCertificateAuthorityCsr", Run: func() error { _, err := c.GetAuthorityCSR(ctx, o, p, ca); return err }, Response: AuthorityCSR{}},
		{Name: "ImportCertificateAuthorityCertificate", Run: func() error {
			_, err := c.ImportAuthorityCertificate(ctx, o, p, ca, AuthorityImport{CertificatePEM: "pem", ChainPEM: []string{"root"}}, `"e1"`)
			return err
		}, Request: AuthorityImport{}, Response: Accepted{}},
		{Name: "ListPrivateCertificates", Run: func() error { _, err := c.ListPrivateCertificates(ctx, o, p, ca, 20, "c"); return err }, Response: privateCertificatePage{}},
		{Name: "ListAllPrivateCertificates", Run: func() error {
			_, err := c.ListProjectPrivateCertificates(ctx, o, p, PrivateCertificateFilters{
				CertificateAuthorityID: ca, Status: "issued", KeySource: "acme", Search: "api.corp", ExpiresBefore: "2026-11-01T00:00:00Z",
			}, 20, "c")
			return err
		}, Response: privateCertificatePage{}},
		{Name: "IssueCertificate (CSR)", Run: func() error {
			_, err := c.IssueCertificate(ctx, o, p, ca, Issue{CSR: "pem", SANs: sans, Profile: "tlsServer", ValidityDays: 30})
			return err
		}, Request: Issue{}, Response: Issued{}},
		{Name: "IssueCertificate (generated key)", Run: func() error {
			_, err := c.IssueCertificate(ctx, o, p, ca, Issue{KeyAlgorithm: "ecdsa-p256", CommonName: "a.corp.example", SANs: sans, Profile: "tlsServerClient", ValidityDays: 30})
			return err
		}, Request: Issue{}, Response: Issued{}},
		{Name: "GetPrivateCertificate", Run: func() error { _, _, err := c.GetPrivateCertificate(ctx, o, p, pct); return err }, Response: PrivateCertificate{}},
		{Name: "RevokePrivateCertificate", Run: func() error {
			_, err := c.RevokePrivateCertificate(ctx, o, p, pct, "keyCompromise", `"t1"`)
			return err
		}, Request: Revoke{}, Response: PrivateCertificate{}},
		{Name: "ExportCertificatePrivateKey", Run: func() error { _, err := c.ExportKey(ctx, o, p, pct, "correct horse battery"); return err }, Request: Export{}, Response: Exported{}},
		{Name: "ForgetCertificatePrivateKey", Run: func() error { _, err := c.ForgetKey(ctx, o, p, pct, `"t1"`); return err }, Request: empty{}, Response: PrivateCertificate{}},
		{Name: "DeletePrivateCertificate", Run: func() error { return c.DeletePrivateCertificate(ctx, o, p, pct, `"t1"`) }, Request: empty{}},
		{Name: "ListAcmeBindings", Run: func() error { _, err := c.ListBindings(ctx, o, p, ca, 20, "c"); return err }, Response: bindingPage{}},
		{Name: "GetAcmeBinding", Run: func() error { _, _, err := c.GetBinding(ctx, o, p, ca, eab); return err }, Response: Binding{}},
		{Name: "CreateAcmeBinding", Run: func() error {
			_, err := c.CreateBinding(ctx, o, p, ca, BindingCreate{Name: "cert-manager", Profile: "tlsServer", MaxValidityDays: 30, AllowedNames: []string{"*.svc.corp.example"}})
			return err
		}, Request: BindingCreate{}, Response: BindingCreated{}},
		{Name: "DeleteAcmeBinding", Run: func() error { return c.DeleteBinding(ctx, o, p, ca, eab, `"b1"`) }},
		{Name: "ListDomainAuthorizations", Run: func() error { _, err := c.ListDomainAuthorizations(ctx, o, 20, "c"); return err }, Response: domainAuthorizationPage{}},
		{Name: "GetDomainAuthorization", Run: func() error { _, err := c.GetDomainAuthorization(ctx, o, dau); return err }, Response: DomainAuthorization{}},
		{Name: "CreateDomainAuthorization", Run: func() error {
			_, err := c.CreateDomainAuthorization(ctx, o, DomainAuthorizationCreate{DomainName: "www.example.com", Name: name, Tags: tags, DNSZoneID: "zon_aaaaaaaaaaaa"})
			return err
		}, Request: DomainAuthorizationCreate{}, Response: DomainAuthorization{}},
		{Name: "UpdateDomainAuthorization", Run: func() error {
			_, err := c.UpdateDomainAuthorization(ctx, o, dau, DomainAuthorizationUpdate{Name: &name, Tags: &tags, DNSZoneID: json.RawMessage("null")})
			return err
		}, Request: DomainAuthorizationUpdate{}, Response: DomainAuthorization{}},
		{Name: "DeleteDomainAuthorization", Run: func() error { return c.DeleteDomainAuthorization(ctx, o, dau) }},
		{Name: "ListCertificates", Run: func() error { _, err := c.ListCertificates(ctx, o, p, 20, "c"); return err }, Response: certificatePage{}},
		{Name: "GetCertificate", Run: func() error { _, err := c.GetCertificate(ctx, o, p, pct); return err }, Response: Certificate{}},
		{Name: "RequestCertificate", Run: func() error {
			_, err := c.CreateCertificate(ctx, o, p, CertificateCreate{Name: name, DNSNames: []string{"www.example.com"}, DomainAuthorizationIDs: []string{dau}})
			return err
		}, Request: CertificateCreate{}, Response: Operation{}},
		{Name: "DeleteCertificate", Run: func() error { _, err := c.DeleteCertificate(ctx, o, p, pct); return err }, Response: Operation{}},
		{Name: "RotateDomainBinding", Run: func() error { _, err := c.RotateDomainAuthorization(ctx, o, dau); return err }, Request: empty{}, Response: DomainAuthorization{}},
		{Name: "GetOperation", Run: func() error { _, err := c.GetOperation(ctx, o, op); return err }, Response: Operation{}},
	}
	later := "recheck, an operation list/cancel, certificate updates, attachments, retry, revoke and versions aren't in ezgh yet"
	leftOut := map[string]string{}
	for _, op := range []string{
		"POST /v1/organizations/{orgId}/domain-authorizations/{authorizationId}/recheck",
		"GET /v1/organizations/{orgId}/operations",
		"DELETE /v1/organizations/{orgId}/operations/{operationId}",
		"PATCH /v1/organizations/{orgId}/projects/{projectId}/certificates/{certificateId}",
		"GET /v1/organizations/{orgId}/projects/{projectId}/certificates/{certificateId}/attachments",
		"POST /v1/organizations/{orgId}/projects/{projectId}/certificates/{certificateId}/attachments",
		"DELETE /v1/organizations/{orgId}/projects/{projectId}/certificates/{certificateId}/attachments/{attachmentId}",
		"POST /v1/organizations/{orgId}/projects/{projectId}/certificates/{certificateId}/retry",
		"POST /v1/organizations/{orgId}/projects/{projectId}/certificates/{certificateId}/revoke",
		"GET /v1/organizations/{orgId}/projects/{projectId}/certificates/{certificateId}/versions",
	} {
		leftOut[op] = later
	}
	openapitest.Check(t, spec, rec, calls, leftOut)
}
