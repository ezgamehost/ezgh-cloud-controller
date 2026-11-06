package certificatesapi

import (
	"context"
	"net/http"
)

// What the L7 controllers need beyond the CLI's client: importing a Kubernetes TLS
// Secret as a certificate (certificates-stack P10, D12). Both answers are synchronous.

// CertificateImport is an import's body. The key is sent only here and kept nowhere else.
type CertificateImport struct {
	Name             string            `json:"name"`
	Tags             map[string]string `json:"tags"`
	Certificate      string            `json:"certificate"`
	CertificateChain *string           `json:"certificateChain"`
	PrivateKey       string            `json:"privateKey"`
}

// CertificateReimport is a new version's body for an imported certificate.
type CertificateReimport struct {
	Certificate      string  `json:"certificate"`
	CertificateChain *string `json:"certificateChain"`
	PrivateKey       string  `json:"privateKey"`
}

// ImportCertificate makes an imported certificate: 201 with it.
func (c *Client) ImportCertificate(ctx context.Context, orgID, projectID string, body CertificateImport) (*Certificate, error) {
	var out Certificate
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificates/import"), body, &out, "")
	return &out, err
}

// ReimportCertificate makes a new version of an imported certificate: 200 with it, the same crt_.
// One that wasn't imported is 409 certificate_not_imported.
func (c *Client) ReimportCertificate(ctx context.Context, orgID, projectID, id string, body CertificateReimport) (*Certificate, error) {
	var out Certificate
	_, err := c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/certificates/{}/import", id), body, &out, "")
	return &out, err
}
