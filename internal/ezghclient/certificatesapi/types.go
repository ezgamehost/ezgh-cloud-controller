package certificatesapi

import "encoding/json"

// The shapes of certificates-api's Private CA routes (api/openapi/certificates-api.json). Fields
// are in the API's order.

// Subject is a CA's distinguished name.
type Subject struct {
	CommonName         string   `json:"commonName"`
	Organization       []string `json:"organization"`
	OrganizationalUnit []string `json:"organizationalUnit"`
	Country            []string `json:"country"`
	Province           []string `json:"province"`
	Locality           []string `json:"locality"`
}

// SubjectInput is a subject as it's sent: only the common name is required.
type SubjectInput struct {
	CommonName         string   `json:"commonName"`
	Organization       []string `json:"organization,omitempty"`
	OrganizationalUnit []string `json:"organizationalUnit,omitempty"`
	Country            []string `json:"country,omitempty"`
	Province           []string `json:"province,omitempty"`
	Locality           []string `json:"locality,omitempty"`
}

// NameConstraints are the names a CA, and every certificate under it, may hold. IPs are CIDR
// prefixes; DNS and URI entries a host or .domain; email a mailbox, host or .domain.
type NameConstraints struct {
	PermittedDNS   []string `json:"permittedDns"`
	ExcludedDNS    []string `json:"excludedDns"`
	PermittedIP    []string `json:"permittedIp"`
	ExcludedIP     []string `json:"excludedIp"`
	PermittedEmail []string `json:"permittedEmail"`
	ExcludedEmail  []string `json:"excludedEmail"`
	PermittedURI   []string `json:"permittedUri"`
	ExcludedURI    []string `json:"excludedUri"`
}

// Empty reports whether no constraint is set.
func (n NameConstraints) Empty() bool {
	return len(n.PermittedDNS)+len(n.ExcludedDNS)+len(n.PermittedIP)+len(n.ExcludedIP)+
		len(n.PermittedEmail)+len(n.ExcludedEmail)+len(n.PermittedURI)+len(n.ExcludedURI) == 0
}

// Authority is a private certificate authority: a root, a subordinate signed by a CA in the
// project (parentKind hosted), or one signed by your own root (external).
type Authority struct {
	ID                  string            `json:"id"`
	OrganizationID      string            `json:"organizationId"`
	ProjectID           string            `json:"projectId"`
	Region              string            `json:"region"`
	Slug                string            `json:"slug"`
	Name                string            `json:"name"`
	Description         string            `json:"description"`
	ResourceName        string            `json:"resourceName"`
	Tags                map[string]string `json:"tags"`
	Type                string            `json:"type"`
	ParentID            *string           `json:"parentId"`
	ParentKind          *string           `json:"parentKind"`
	Subject             Subject           `json:"subject"`
	KeyAlgorithm        string            `json:"keyAlgorithm"`
	PathLength          int               `json:"pathLength"`
	NameConstraints     NameConstraints   `json:"nameConstraints"`
	ValidityDays        int               `json:"validityDays"`
	MaxLeafValidityDays *int              `json:"maxLeafValidityDays"`
	// State: creating, pendingCertificate, active, disabled, pendingDeletion, deleted or failed.
	State          string   `json:"state"`
	NotBefore      *string  `json:"notBefore"`
	NotAfter       *string  `json:"notAfter"`
	CertificatePEM *string  `json:"certificatePem"`
	ChainPEM       []string `json:"chainPem"`
	CSRPEM         *string  `json:"csrPem"`
	CRLURL         string   `json:"crlUrl"`
	OCSPURL        string   `json:"ocspUrl"`
	CAIssuerURL    string   `json:"caIssuerUrl"`
	// ACMEDirectoryURL is a subordinate's; nil for roots.
	ACMEDirectoryURL *string `json:"acmeDirectoryUrl"`
	CRLThisUpdate    *string `json:"crlThisUpdate"`
	// RevokedAt and RevocationReason are set once the CA is revoked (unspecified, keyCompromise,
	// affiliationChanged, superseded or cessationOfOperation).
	RevokedAt        *string `json:"revokedAt"`
	RevocationReason *string `json:"revocationReason"`
	DeletionAt       *string `json:"deletionAt"`
	CreatedAt        string  `json:"createdAt"`
	UpdatedAt        string  `json:"updatedAt"`
}

type authorityPage struct {
	CertificateAuthorities []Authority `json:"certificateAuthorities"`
	NextCursor             *string     `json:"nextCursor"`
}

// AuthorityCreate creates a CA. Leave a field zero for the API's default.
type AuthorityCreate struct {
	Name                string            `json:"name"`
	Description         string            `json:"description,omitempty"`
	Tags                map[string]string `json:"tags,omitempty"`
	Type                string            `json:"type"`
	ParentID            string            `json:"parentId,omitempty"`
	ParentKind          string            `json:"parentKind,omitempty"`
	Subject             SubjectInput      `json:"subject"`
	KeyAlgorithm        string            `json:"keyAlgorithm"`
	PathLength          *int              `json:"pathLength,omitempty"`
	NameConstraints     *NameConstraints  `json:"nameConstraints,omitempty"`
	ValidityDays        int               `json:"validityDays,omitempty"`
	MaxLeafValidityDays int               `json:"maxLeafValidityDays,omitempty"`
}

// AuthorityUpdate changes a CA's name, description, tags, or shrinks its longest certificate.
type AuthorityUpdate struct {
	Name                *string            `json:"name,omitempty"`
	Description         *string            `json:"description,omitempty"`
	Tags                *map[string]string `json:"tags,omitempty"`
	MaxLeafValidityDays *int               `json:"maxLeafValidityDays,omitempty"`
}

// AuthorityImport is an external subordinate's signed certificate and the CA certificates above
// it, nearest first, up to the root.
type AuthorityImport struct {
	CertificatePEM string   `json:"certificatePem"`
	ChainPEM       []string `json:"chainPem"`
}

// AuthorityDelete schedules deletion after a window of 7–30 days.
type AuthorityDelete struct {
	DeletionWindowDays int `json:"deletionWindowDays,omitempty"`
}

// Revoke is a revocation's RFC 5280 reason.
type Revoke struct {
	Reason string `json:"reason"`
}

type empty struct{}

// AuthorityCSR is an external subordinate's CSR.
type AuthorityCSR struct {
	CSRPEM string `json:"csrPem"`
}

// Accepted is a 202's answer: the CA as it is now and the operation that finishes the change. An
// Idempotency-Key replay answers the operation alone, so Resource may be nil.
type Accepted struct {
	Resource  *Authority `json:"resource,omitempty"`
	Operation *Operation `json:"operation"`
}

// isBareOperation reports whether an answer is an operation itself (a replayed 202), not an
// envelope or a resource: it has an operation's target and status at its top level.
func isBareOperation(data []byte) bool {
	var probe struct {
		Target *OperationTarget `json:"target"`
		Status *string          `json:"status"`
		Action *string          `json:"action"`
	}
	return json.Unmarshal(data, &probe) == nil && probe.Target != nil && probe.Status != nil && probe.Action != nil
}

// UnmarshalJSON reads `{resource, operation}`, or the operation alone (a replay).
func (a *Accepted) UnmarshalJSON(data []byte) error {
	if isBareOperation(data) {
		var op Operation
		if err := json.Unmarshal(data, &op); err != nil {
			return err
		}
		*a = Accepted{Operation: &op}
		return nil
	}
	type envelope Accepted
	var e envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return err
	}
	*a = Accepted(e)
	return nil
}

// Operation is an asynchronous change, followed until FinishedAt is set.
type Operation struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organizationId"`
	ProjectID      *string         `json:"projectId"`
	Type           string          `json:"type"`
	Action         string          `json:"action"`
	Status         string          `json:"status"`
	Target         OperationTarget `json:"target"`
	Cancellable    bool            `json:"cancellable"`
	CreatedBy      json.RawMessage `json:"createdBy"`
	CreatedAt      string          `json:"createdAt"`
	UpdatedAt      string          `json:"updatedAt"`
	StartedAt      *string         `json:"startedAt"`
	FinishedAt     *string         `json:"finishedAt"`
	// Error is why it failed (an open object; ErrorText reads its code and message), or null.
	Error json.RawMessage `json:"error"`
}

// ErrorText is a failed operation's message, with its code; "" without an error.
func (o *Operation) ErrorText() string {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if len(o.Error) == 0 || string(o.Error) == "null" || json.Unmarshal(o.Error, &e) != nil {
		return ""
	}
	switch {
	case e.Message != "" && e.Code != "":
		return e.Message + " (" + e.Code + ")"
	case e.Message != "":
		return e.Message
	}
	return e.Code
}

// OperationTarget is what an operation changes.
type OperationTarget struct {
	ID           string `json:"id"`
	ResourceName string `json:"resourceName"`
	Type         string `json:"type"`
}

// SANs are a leaf's subject alternative names.
type SANs struct {
	DNS   []string `json:"dns"`
	IP    []string `json:"ip"`
	URI   []string `json:"uri"`
	Email []string `json:"email"`
}

// Len is how many names there are.
func (s SANs) Len() int { return len(s.DNS) + len(s.IP) + len(s.URI) + len(s.Email) }

// PrivateCertificate is a certificate a private CA issued.
type PrivateCertificate struct {
	ID                     string `json:"id"`
	OrganizationID         string `json:"organizationId"`
	ProjectID              string `json:"projectId"`
	Region                 string `json:"region"`
	Slug                   string `json:"slug"`
	ResourceName           string `json:"resourceName"`
	CertificateAuthorityID string `json:"certificateAuthorityId"`
	Serial                 string `json:"serial"`
	CommonName             string `json:"commonName"`
	SANs                   SANs   `json:"sans"`
	Profile                string `json:"profile"`
	// KeySource: csr, generated, managed or acme. KeyState: none, stored, forgotten or retired.
	KeySource            string   `json:"keySource"`
	KeyState             string   `json:"keyState"`
	Fingerprint          string   `json:"fingerprint"`
	NotBefore            string   `json:"notBefore"`
	NotAfter             string   `json:"notAfter"`
	Status               string   `json:"status"`
	RevokedAt            *string  `json:"revokedAt"`
	RevocationReason     *string  `json:"revocationReason"`
	ACMEBindingID        *string  `json:"acmeBindingId"`
	ManagedCertificateID *string  `json:"managedCertificateId"`
	CertificatePEM       string   `json:"certificatePem"`
	ChainPEM             []string `json:"chainPem"`
	CreatedAt            string   `json:"createdAt"`
	UpdatedAt            string   `json:"updatedAt"`
}

type privateCertificatePage struct {
	PrivateCertificates []PrivateCertificate `json:"privateCertificates"`
	NextCursor          *string              `json:"nextCursor"`
}

// Issue issues a certificate: from a CSR (CSR set; SANs replace the CSR's), or from a key EZGH
// generates (KeyAlgorithm set, SANs required).
type Issue struct {
	CSR          string `json:"csr,omitempty"`
	KeyAlgorithm string `json:"keyAlgorithm,omitempty"`
	CommonName   string `json:"commonName,omitempty"`
	SANs         *SANs  `json:"sans,omitempty"`
	Profile      string `json:"profile"`
	ValidityDays int    `json:"validityDays"`
}

// Issued is IssueCertificate's answer: the certificate (201, a CSR signed at once) or the
// operation that finishes it (202: `{operation}`, or the operation alone on a replay). Exactly one
// of Certificate and Operation is set.
type Issued struct {
	PrivateCertificate
	Operation *Operation `json:"operation,omitempty"`
}

// Certificate is the issued certificate, or nil when it's still being issued.
func (i *Issued) Certificate() *PrivateCertificate {
	if i.Operation != nil || i.ID == "" {
		return nil
	}
	return &i.PrivateCertificate
}

// UnmarshalJSON reads a certificate, `{operation}`, or the operation alone.
func (i *Issued) UnmarshalJSON(data []byte) error {
	if isBareOperation(data) {
		var op Operation
		if err := json.Unmarshal(data, &op); err != nil {
			return err
		}
		*i = Issued{Operation: &op}
		return nil
	}
	var e struct {
		Operation *Operation `json:"operation"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return err
	}
	if e.Operation != nil {
		*i = Issued{Operation: e.Operation}
		return nil
	}
	*i = Issued{}
	return json.Unmarshal(data, &i.PrivateCertificate)
}

// Export is the passphrase an exported key is encrypted with (12–1024 characters).
type Export struct {
	Passphrase string `json:"passphrase"`
}

// Exported is the key as encrypted PKCS#8 PEM, the certificate and its chain.
type Exported struct {
	PrivateKey  string   `json:"privateKey"`
	Certificate string   `json:"certificate"`
	Chain       []string `json:"chain"`
}

// BindingOwner is the principal an ACME binding acts as.
type BindingOwner struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Binding is an ACME External Account Binding; its slug is the EAB key ID.
type Binding struct {
	ID                     string       `json:"id"`
	OrganizationID         string       `json:"organizationId"`
	ProjectID              string       `json:"projectId"`
	Region                 string       `json:"region"`
	Slug                   string       `json:"slug"`
	ResourceName           string       `json:"resourceName"`
	CertificateAuthorityID string       `json:"certificateAuthorityId"`
	Name                   string       `json:"name"`
	Profile                string       `json:"profile"`
	MaxValidityDays        int          `json:"maxValidityDays"`
	AllowedNames           []string     `json:"allowedNames"`
	Owner                  BindingOwner `json:"owner"`
	State                  string       `json:"state"`
	DirectoryURL           string       `json:"directoryUrl"`
	CreatedAt              string       `json:"createdAt"`
	UpdatedAt              string       `json:"updatedAt"`
}

type bindingPage struct {
	ACMEBindings []Binding `json:"acmeBindings"`
	NextCursor   *string   `json:"nextCursor"`
}

// BindingCreate creates an ACME binding owned by the caller.
type BindingCreate struct {
	Name            string   `json:"name"`
	Profile         string   `json:"profile"`
	MaxValidityDays int      `json:"maxValidityDays"`
	AllowedNames    []string `json:"allowedNames,omitempty"`
}

// BindingCreated is the binding and, only from the create that made it, its HMAC key
// (base64url). An Idempotency-Key replay answers a nil key.
type BindingCreated struct {
	Binding
	HMACKey *string `json:"hmacKey"`
}

// Web Certificates: domain authorizations and certificates (the shapes of certificates-api's org
// and project routes).

// DNSInstruction is a DNS record to publish for an authorization.
type DNSInstruction struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ManagedDNS is the state of the validation records EZGH writes into the linked zone: status
// synced, pending or failed.
type ManagedDNS struct {
	ZoneID    string  `json:"zoneId"`
	Status    string  `json:"status"`
	Detail    *string `json:"detail"`
	UpdatedAt string  `json:"updatedAt"`
}

// DomainAuthorization proves control of a domain for the organization's public certificates.
type DomainAuthorization struct {
	ID                string            `json:"id"`
	OrganizationID    string            `json:"organizationId"`
	Slug              string            `json:"slug"`
	Name              string            `json:"name"`
	ResourceName      string            `json:"resourceName"`
	CreatedAt         string            `json:"createdAt"`
	UpdatedAt         string            `json:"updatedAt"`
	DomainName        string            `json:"domainName"`
	DisplayName       string            `json:"displayName"`
	Enabled           bool              `json:"enabled"`
	Status            string            `json:"status"`
	BindingGeneration int               `json:"bindingGeneration"`
	DNSInstructions   []DNSInstruction  `json:"dnsInstructions"`
	Diagnostics       json.RawMessage   `json:"diagnostics"`
	VerifiedAt        *string           `json:"verifiedAt"`
	NextCheckAt       *string           `json:"nextCheckAt"`
	Tags              map[string]string `json:"tags"`
	ProjectID         *string           `json:"projectId"`
	Region            string            `json:"region"`
	DNSZoneID         *string           `json:"dnsZoneId"`
	ManagedDNS        *ManagedDNS       `json:"managedDns"`
}

type domainAuthorizationPage struct {
	DomainAuthorizations []DomainAuthorization `json:"domainAuthorizations"`
	NextCursor           *string               `json:"nextCursor"`
}

// DomainAuthorizationCreate creates an authorization, or (with DNSZoneID) links an existing one
// for the domain to the zone.
type DomainAuthorizationCreate struct {
	DomainName string            `json:"domainName"`
	Name       string            `json:"name,omitempty"`
	Tags       map[string]string `json:"tags,omitempty"`
	DNSZoneID  string            `json:"dnsZoneId,omitempty"`
}

// DomainAuthorizationUpdate changes an authorization. DNSZoneID is `"zon_…"` to link, `null` to
// unlink, absent to leave alone.
type DomainAuthorizationUpdate struct {
	Name      *string            `json:"name,omitempty"`
	Enabled   *bool              `json:"enabled,omitempty"`
	Tags      *map[string]string `json:"tags,omitempty"`
	DNSZoneID json.RawMessage    `json:"dnsZoneId,omitempty"`
}

// Certificate is a public (ACME) or private-CA certificate request in a project.
type Certificate struct {
	ID                      string            `json:"id"`
	OrganizationID          string            `json:"organizationId"`
	Slug                    string            `json:"slug"`
	Name                    string            `json:"name"`
	ResourceName            string            `json:"resourceName"`
	CreatedAt               string            `json:"createdAt"`
	UpdatedAt               string            `json:"updatedAt"`
	ProjectID               string            `json:"projectId"`
	Region                  string            `json:"region"`
	Tags                    map[string]string `json:"tags"`
	DNSNames                []string          `json:"dnsNames"`
	DomainAuthorizationIDs  []string          `json:"domainAuthorizationIds"`
	KeyAlgorithm            string            `json:"keyAlgorithm"`
	RenewalEnabled          bool              `json:"renewalEnabled"`
	ConfigurationGeneration int               `json:"configurationGeneration"`
	IssuanceStatus          string            `json:"issuanceStatus"`
	RenewalStatus           string            `json:"renewalStatus"`
	DeploymentStatus        string            `json:"deploymentStatus"`
	ActiveVersionID         *string           `json:"activeVersionId"`
	NotAfter                *string           `json:"notAfter"`
	RenewAt                 *string           `json:"renewAt"`
	NextAttemptAt           *string           `json:"nextAttemptAt"`
	Error                   json.RawMessage   `json:"error"`
	Status                  string            `json:"status"`
	Issuer                  json.RawMessage   `json:"issuer"`
}

type certificatePage struct {
	Certificates []Certificate `json:"certificates"`
	NextCursor   *string       `json:"nextCursor"`
}

// CertificateCreate requests a public certificate for the names, through their authorizations.
type CertificateCreate struct {
	Name                   string   `json:"name"`
	DNSNames               []string `json:"dnsNames"`
	DomainAuthorizationIDs []string `json:"domainAuthorizationIds"`
}
