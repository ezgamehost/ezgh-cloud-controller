// Package manifest reads a product's service-manifest.json (contracts/registration.md, "The
// service manifest"): its namespace, IAM actions, managed policies, resource types, endpoints
// (the Trails catalog), quotas and meters. The registration Job sends its IAM part to org-api;
// the kit reads the rest at start.
//
// Check applies the rules JSON Schema can't express, as scripts/lib/manifest.mjs does (org-api
// applies the same ones to a registration). A manifest that breaks them doesn't load.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Manifest is manifestVersion 1.
type Manifest struct {
	Schema          string `json:"$schema,omitempty"`
	ManifestVersion int    `json:"manifestVersion"`
	Namespace       string `json:"namespace"`
	DirectoryUsable *bool  `json:"directoryUsable,omitempty"`
	// [being built] ProjectDeletionCheck says org-api asks the product's ProjectDeletionCheckPath
	// before deleting any project (contracts/org-lifecycle.md, "Products that refuse"). Registered
	// as is; nil (omitted) is false.
	ProjectDeletionCheck *bool `json:"projectDeletionCheck,omitempty"`
	// OrgAPI is the org-api operations the namespace's ok_ credential may call, registered as is;
	// nil (omitted) is none.
	OrgAPI *OrgAPI `json:"orgApi,omitempty"`
	// Scopes are the namespace's scope ceilings for EZGH SSO resource tokens, registered as is;
	// nil (omitted) is none.
	Scopes []Scope `json:"scopes,omitempty"`
	// [being built] Delegation is the receiver's allowlist for delegated calls: which calling
	// namespaces may call its auth "delegated" endpoints for a customer, for which of its actions
	// (contracts/registration.md, "Delegation"). Registered as is; nil (omitted) is none.
	Delegation  *Delegation `json:"delegation,omitempty"`
	DisplayName string      `json:"displayName"`
	Description string      `json:"description"`
	API         API         `json:"api"`
	// Ingest is the namespace's ingest host, present exactly when an endpoint is on it.
	Ingest *IngestHost `json:"ingest,omitempty"`
	// Edge is the product's edge host, present exactly when an endpoint is on it
	// (contracts/registration.md, "Edge endpoints").
	Edge *EdgeHost `json:"edge,omitempty"`
	// Live is the product's live host, present exactly when an endpoint is on it
	// (contracts/registration.md, "Live endpoints").
	Live *LiveHost `json:"live,omitempty"`
	// Registry is the product's registry host, present exactly when an endpoint is on it
	// (contracts/registration.md, "Registry endpoints").
	Registry *RegistryHost `json:"registry,omitempty"`
	// Data is the product's data host, present exactly when an endpoint is on it
	// (contracts/registration.md, "Data endpoints").
	Data *DataHost `json:"data,omitempty"`
	// Git is the product's Git host, present exactly when an endpoint is on it
	// (contracts/registration.md, "Git hosts").
	Git *GitHost `json:"git,omitempty"`
	// PKI is the PKI host, pki.ezghcloud.com, which only namespace certificates declares, present
	// exactly when an endpoint is on it (contracts/registration.md, "PKI endpoints").
	PKI *PKIHost `json:"pki,omitempty"`
	// S3 is Object Storage's S3 host, which only namespace objects declares and no endpoint is on
	// (contracts/registration.md, "The S3 host").
	S3                 *S3Host             `json:"s3,omitempty"`
	ResourceTypes      []ResourceType      `json:"resourceTypes"`
	ManagedPolicies    []ManagedPolicy     `json:"managedPolicies"`
	Actions            []Action            `json:"actions"`
	Endpoints          []Endpoint          `json:"endpoints"`
	Quotas             []Quota             `json:"quotas"`
	Metering           *Metering           `json:"metering,omitempty"`
	ServiceCredentials *ServiceCredentials `json:"serviceCredentials,omitempty"`
}

// OrgAPI declares which of org-api's internal operations the namespace's org-api ok_ credential
// may call (contracts/registration.md, "org-api operations"): permissions.batch,
// principals.deleted, …. The kit checks only the names' shape and that each is listed once, so a
// new org-api operation needs no kit release; org-api refuses names it doesn't have.
type OrgAPI struct {
	Operations []string `json:"operations"`
}

// [being built] Delegation is a receiver's allowlist for delegated calls (contracts/registration.md,
// "Delegation"): at most MaxDelegationCallers entries, each calling namespace once.
type Delegation struct {
	Callers []DelegationCaller `json:"callers"`
}

// DelegationCaller allows one calling namespace to delegate Actions (1 to MaxDelegationActions
// patterns of the receiver's own actions, policy globs). Origins (at most MaxDelegationOrigins)
// are the edges whose references it may relay onward (a two-entry chain origin,caller).
type DelegationCaller struct {
	Namespace string   `json:"namespace"`
	Actions   []string `json:"actions"`
	Origins   []string `json:"origins,omitempty"`
}

// The delegation allowlist's limits.
const (
	MaxDelegationCallers = 20
	MaxDelegationActions = 50
	MaxDelegationOrigins = 5
)

// Entry is the allowlist's entry for a calling namespace; false when it has none (a nil
// Delegation has none).
func (d *Delegation) Entry(namespace string) (DelegationCaller, bool) {
	if d == nil {
		return DelegationCaller{}, false
	}
	for _, e := range d.Callers {
		if e.Namespace == namespace {
			return e, true
		}
	}
	return DelegationCaller{}, false
}

// Covers reports whether one of the entry's patterns covers action (org-api's glob match, as
// scopes are matched).
func (e DelegationCaller) Covers(action string) bool {
	return slices.ContainsFunc(e.Actions, func(p string) bool { return MatchesActionPattern(p, action) })
}

// covers reports whether any entry covers action.
func (d *Delegation) covers(action string) bool {
	return d != nil && slices.ContainsFunc(d.Callers, func(e DelegationCaller) bool { return e.Covers(action) })
}

// Scope is one scope ceiling (contracts/registration.md, "Scopes"): an EZGH SSO resource token
// carrying Name may do, at most, the namespace's actions its Actions patterns cover. org-api reads
// them from the registration on every evaluation.
type Scope struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
	// Status is active (the default, "") or deprecated: a scope left out of a later registration
	// is deprecated, and still caps the tokens that carry it.
	Status string `json:"status,omitempty"`
}

// MaxTimeoutSeconds is the longest an endpoint's requests may take: the Gateway's longest route
// timeout, 15 minutes.
const MaxTimeoutSeconds = 900

// MaxScopes is the most scopes a namespace declares; MaxScopeActions the most patterns a scope has.
const (
	MaxScopes       = 20
	MaxScopeActions = 50
)

type API struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
}

// IngestHost is the namespace's ingest host (contracts/registration.md, "Ingest endpoints"):
// <namespace>-ingest.ezghcloud.com, its HTTP port and, when it serves gRPC, its gRPC port.
type IngestHost struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	GRPCPort int    `json:"grpcPort,omitempty"`
}

// EdgeHost is a product's edge host (contracts/registration.md, "Edge endpoints"): one host per
// EZGH SSO directory, https://<alias>.<Domain>, where the organization comes from the alias, not
// the path. MCP Central's is mcp.ezgh.app. Port is the edge component's port, present exactly
// when an endpoint names component edge.
type EdgeHost struct {
	Domain string `json:"domain"`
	Port   int    `json:"port,omitempty"`
	// Cookie is the product's own cookie on the edge host, present exactly when an endpoint takes
	// it (credentials: cookie).
	Cookie *EdgeCookie `json:"cookie,omitempty"`
}

// LiveHost is a product's live host (contracts/registration.md, "Live endpoints"):
// <namespace>-live.ezghcloud.com, where it serves long-lived streams (obs-live.ezghcloud.com, the
// live tail), and Port, the live component's port, which no other server of the product uses.
type LiveHost struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
}

// RegistryHost is a container registry's data host (contracts/registration.md, "Registry
// endpoints"): one host per project, https://<host id>.<Domain>, served by the product's own OCI
// server on Port, never by Service.Handler. Domain is exactly <namespace>.ezghcloud.com
// (registry.ezghcloud.com), so a product never claims another product's host, and the API host
// is then <namespace>-api.ezghcloud.com.
type RegistryHost struct {
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

// GitHost is EZGH Git's data host (contracts/registration.md, "Git hosts"): Git's smart HTTP and
// Git LFS on one host per project, https://<host id>.<Domain>, served by the product's own Git
// server on Port, never by Service.Handler. Domain is exactly <namespace>.ezghcloud.com
// (git.ezghcloud.com), and the API host is then <namespace>-api.ezghcloud.com.
type GitHost struct {
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

// DataHost is a product's data host (contracts/registration.md, "Data endpoints"): its own data
// plane on exactly <namespace>.ezghcloud.com (flags.ezghcloud.com), served by the product's own
// data server on Port, never by Service.Handler, and authenticated by data-plane keys the product
// issues (auth productKey). The API host is then <namespace>-api.ezghcloud.com.
type DataHost struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
}

// PKIHost is the PKI host (contracts/registration.md, "PKI endpoints"): exactly PKIHostname, a
// reserved hostname only PKINamespace declares, where Private CA serves CA certificates, CRLs and
// OCSP (plain HTTP and HTTPS) and ACME (HTTPS) from its own PKI servers, never Service.Handler. Port
// is the revocation server's; the Gateway may route the ACME part to another port of the product's.
// It doesn't move the API host.
type PKIHost struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
}

// S3Host is the S3 host (contracts/registration.md, "The S3 host"): Domain, s3.<region>.ezghapps.com,
// is S3's path-style endpoint, and <bucket>.<Domain> its virtual-hosted one, served by the product's
// own S3 gateway on Port, never Service.Handler. Only S3Namespace declares it. S3 chooses an
// operation by method, path, query and headers, so no manifest endpoint is on it. It's off
// ezghcloud.com on purpose, so it doesn't move the API host.
type S3Host struct {
	Domain string `json:"domain"`
	Port   int    `json:"port"`
}

// EdgeCookie is a product's own edge cookie (contracts/identity-and-authz.md, "Product cookies"):
// a host-only __Host- cookie holding an EZGH SSO resource token bound to the audience label
// https://<namespace>.ezghcloud.com/v1/organizations/<orgId>/<Audience>, which the kit sends as
// requestUrl (MCP Central's __Host-mcp_portal, audience portal).
type EdgeCookie struct {
	Name     string `json:"name"`
	Audience string `json:"audience"`
}

type ResourceType struct {
	Name        string `json:"name"`
	Prefix      string `json:"prefix"`
	Parent      string `json:"parent"`
	Regional    bool   `json:"regional"`
	Description string `json:"description"`
	Status      string `json:"status,omitempty"`
}

type ManagedPolicy struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      string `json:"status,omitempty"`
}

type Action struct {
	ID              string   `json:"id"`
	Description     string   `json:"description"`
	Resource        string   `json:"resource"`
	AccessLevel     string   `json:"accessLevel"`
	ManagedPolicies []string `json:"managedPolicies"`
	Status          string   `json:"status,omitempty"`
	// Ingest marks an ingest action: a project ingest key may hold it, an API key's owner may be
	// allowed it, and only ingest-host endpoints check it. Sent only when true, so registrations
	// without one keep their digests.
	Ingest bool `json:"ingest,omitempty"`
	// PlatformOnly marks a platform-only action: in no managed policy, and left out of org-api's
	// action listing outside the platform organizations. Sent only when true, like Ingest.
	PlatformOnly bool `json:"platformOnly,omitempty"`
}

// Endpoint is one route and its Trails catalog entry.
type Endpoint struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Auth is none (/health, /ready, /openapi.json), caller (a customer, authorized with Action),
	// operation (authorized from the operation's target), service (/internal), ingestKey (an
	// ingest-host endpoint, authorized by resolving a project ingest key or API key for Action), registry
	// (a registry-host endpoint, authorized by the registry server for Action), invocationToken
	// (a live-host endpoint under /v1/invocations/, given its bearer unchecked: Call.InvocationToken)
	// or receiptHandle (a live-host POST under /v1/receipts/ whose body carries product-signed
	// receipt handles; the kit reads no credential).
	Auth string `json:"auth"`
	// Host is api (the default, ""), ingest, edge, live or registry; Protocol is http (the
	// default, "") or grpc.
	Host string `json:"host,omitempty"`
	// Component is the Deployment that serves the endpoint: api (the default, ""), edge, or live
	// (live-host endpoints, and only they).
	// Service.Handler(component) serves, and drift-checks, only its own component's endpoints;
	// GET /health, /ready and /openapi.json are every component's and name none.
	Component string `json:"component,omitempty"`
	// Public marks an unauthenticated endpoint that isn't the kit's own (auth none, GET, trail
	// null; contracts/registration.md, "Public endpoints"): metadata (a protected-resource
	// metadata document, a JWKS, an OAuth client metadata document) or redirect (an OAuth callback,
	// a sign-in redirect).
	Public string `json:"public,omitempty"`
	// Credentials narrows what a caller endpoint (or a public redirect, through Call.Authenticate)
	// takes: bearer (only Authorization: Bearer; the edge host's default) or cookie (only the
	// product's edge cookie, edge-host endpoints only). "" is the platform's rule: bearer or the
	// session cookie on the API host, bearer on the edge host.
	Credentials string `json:"credentials,omitempty"`
	// TimeoutSeconds, when set (1-900), is how long the endpoint's requests may take, read and
	// answer, in place of the server's defaults (60 s to answer): MCP Central's tool calls run up
	// to 10 minutes behind a 15-minute route timeout.
	TimeoutSeconds         int             `json:"timeoutSeconds,omitempty"`
	Protocol               string          `json:"protocol,omitempty"`
	Action                 string          `json:"action,omitempty"`
	Async                  bool            `json:"async,omitempty"`
	IdempotencyException   string          `json:"idempotencyException,omitempty"`
	IdempotencyKey         bool            `json:"idempotencyKey,omitempty"`
	Quotas                 []EndpointQuota `json:"quotas,omitempty"`
	BlockedBySpendControls bool            `json:"blockedBySpendControls,omitempty"`
	// ErrorModel is how the endpoint answers errors: the platform's body ("", the default),
	// ErrorModelPrometheus, the Prometheus HTTP API's (contracts/api-conventions.md, "Prometheus
	// query surface"), or ErrorModelOpenAI, the OpenAI API's ("OpenAI-compatible surface"): the
	// kit's failures then write httpapi.WritePrometheusError or httpapi.WriteOpenAIError, and the
	// OpenAPI document describes PrometheusError or OpenAIError for the operation.
	ErrorModel string `json:"errorModel,omitempty"`
	// Trail is how the call is recorded, or nil: never recorded. Always written out, so null is
	// a decision, not an omission.
	Trail *Trail `json:"trail"`
}

// Trail is an endpoint's catalog entry: one entry, or (a JSON-RPC endpoint) an entry per JSON-RPC
// method (contracts/trails.md, "JSON-RPC endpoints").
type Trail struct {
	EventName string `json:"eventName,omitempty"`
	// EventCategory is Management ("", the default) or Data: a call that reads or acts on a
	// resource's content (MCP Central's CallTool).
	EventCategory string `json:"eventCategory,omitempty"`
	// Sensitive request fields are recorded as HIDDEN_DUE_TO_SECURITY_REASONS, at any depth.
	Sensitive []string `json:"sensitive,omitempty"`
	// ResponseElements lists top-level response fields recorded for a successful write, beyond
	// the default identifiers.
	ResponseElements []string `json:"responseElements,omitempty"`
	// Record is which calls are recorded: every one ("", the default) or RecordRefusals, only the
	// calls that fail after the caller is known (contracts/trails.md, "The run-operation volume
	// exception"): Workflows' StartRun and PollTasks (a live-host long poll), EZGH Inference's
	// CreateEmbeddings and Queues' SendMessages and ReceiveMessages (a live-host long poll), whose
	// successes would flood audit-api.
	Record string `json:"record,omitempty"`
	// ByRPCMethod, for a JSON-RPC endpoint, is the entry per JSON-RPC method (nil: not recorded),
	// and Otherwise the entry for every other method (nil: not recorded). Such a trail has no
	// entry of its own: written {"byRpcMethod": {…}, "otherwise": …}.
	ByRPCMethod map[string]*Trail `json:"-"`
	Otherwise   *Trail            `json:"-"`
}

// Event categories.
const (
	CategoryManagement = "Management"
	CategoryData       = "Data"
)

// RecordRefusals is Trail.Record for an entry that records only failures after authentication.
// The kit records the failures its handler returns; one the handler writes itself isn't recorded.
const RecordRefusals = "refusals"

// RPC reports whether the trail is per JSON-RPC method.
func (t *Trail) RPC() bool { return t != nil && t.ByRPCMethod != nil }

// Category is the entry's event category: CategoryManagement unless it says Data.
func (t *Trail) Category() string {
	if t.EventCategory == "" {
		return CategoryManagement
	}
	return t.EventCategory
}

// ForMethod is the entry a call of the JSON-RPC method records (nil: none); for a trail that isn't
// per method, the trail itself.
func (t *Trail) ForMethod(method string) *Trail {
	if t == nil || !t.RPC() {
		return t
	}
	if entry, listed := t.ByRPCMethod[method]; listed {
		return entry
	}
	return t.Otherwise
}

// Entries are the trail's entries that record something: itself, or its methods' (sorted) and
// then Otherwise.
func (t *Trail) Entries() []*Trail {
	if t == nil {
		return nil
	}
	if !t.RPC() {
		return []*Trail{t}
	}
	var out []*Trail
	for _, method := range slices.Sorted(maps.Keys(t.ByRPCMethod)) {
		if entry := t.ByRPCMethod[method]; entry != nil {
			out = append(out, entry)
		}
	}
	if t.Otherwise != nil {
		out = append(out, t.Otherwise)
	}
	return out
}

type trailEntry struct {
	EventName        string   `json:"eventName"`
	EventCategory    string   `json:"eventCategory,omitempty"`
	Sensitive        []string `json:"sensitive,omitempty"`
	ResponseElements []string `json:"responseElements,omitempty"`
	Record           string   `json:"record,omitempty"`
}

// UnmarshalJSON reads either form, strictly: an entry, or {"byRpcMethod", "otherwise"}, with
// "otherwise" written out (null is a decision, as for trail itself).
func (t *Trail) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	strict := func(raw []byte, into any) error {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		return dec.Decode(into)
	}
	if _, rpc := probe["byRpcMethod"]; !rpc {
		var e trailEntry
		if err := strict(data, &e); err != nil {
			return fmt.Errorf("trail: %w", err)
		}
		*t = Trail{EventName: e.EventName, EventCategory: e.EventCategory, Sensitive: e.Sensitive, ResponseElements: e.ResponseElements, Record: e.Record}
		return nil
	}
	var d struct {
		ByRPCMethod map[string]*trailEntry `json:"byRpcMethod"`
		Otherwise   json.RawMessage        `json:"otherwise"`
	}
	if err := strict(data, &d); err != nil {
		return fmt.Errorf("trail: %w", err)
	}
	if d.Otherwise == nil {
		return errors.New(`trail: a byRpcMethod trail writes "otherwise" out (null: not recorded)`)
	}
	out := Trail{ByRPCMethod: map[string]*Trail{}}
	for method, e := range d.ByRPCMethod {
		var entry *Trail
		if e != nil {
			entry = &Trail{EventName: e.EventName, EventCategory: e.EventCategory, Sensitive: e.Sensitive, ResponseElements: e.ResponseElements, Record: e.Record}
		}
		out.ByRPCMethod[method] = entry
	}
	if string(bytes.TrimSpace(d.Otherwise)) != "null" {
		var e trailEntry
		if err := strict(d.Otherwise, &e); err != nil {
			return fmt.Errorf("trail.otherwise: %w", err)
		}
		out.Otherwise = &Trail{EventName: e.EventName, EventCategory: e.EventCategory, Sensitive: e.Sensitive, ResponseElements: e.ResponseElements, Record: e.Record}
	}
	*t = out
	return nil
}

// MarshalJSON writes the form it was read in.
func (t Trail) MarshalJSON() ([]byte, error) {
	entry := func(e *Trail) *trailEntry {
		if e == nil {
			return nil
		}
		return &trailEntry{EventName: e.EventName, EventCategory: e.EventCategory, Sensitive: e.Sensitive, ResponseElements: e.ResponseElements, Record: e.Record}
	}
	if !t.RPC() {
		return json.Marshal(entry(&t))
	}
	methods := map[string]*trailEntry{}
	for method, e := range t.ByRPCMethod {
		methods[method] = entry(e)
	}
	return json.Marshal(struct {
		ByRPCMethod map[string]*trailEntry `json:"byRpcMethod"`
		Otherwise   *trailEntry            `json:"otherwise"`
	}{methods, entry(t.Otherwise)})
}

// EndpointQuota is one quota an endpoint's requests consume: written as the quota's ID (an
// amount of 1) or as { "id", "amount" } (contracts/quotas.md, "Defining quotas").
type EndpointQuota struct {
	ID string
	// Amount is what one request consumes.
	Amount int
}

// UnmarshalJSON reads either form.
func (q *EndpointQuota) UnmarshalJSON(data []byte) error {
	var id string
	if err := json.Unmarshal(data, &id); err == nil {
		*q = EndpointQuota{ID: id, Amount: 1}
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var obj struct {
		ID     string `json:"id"`
		Amount *int   `json:"amount"`
	}
	if err := dec.Decode(&obj); err != nil {
		return fmt.Errorf("an endpoint's quota is an ID or { id, amount }: %w", err)
	}
	*q = EndpointQuota{ID: obj.ID, Amount: 1}
	if obj.Amount != nil {
		q.Amount = *obj.Amount
	}
	return nil
}

// MarshalJSON writes the short form when the amount is 1.
func (q EndpointQuota) MarshalJSON() ([]byte, error) {
	if q.Amount == 1 {
		return json.Marshal(q.ID)
	}
	return json.Marshal(map[string]any{"id": q.ID, "amount": q.Amount})
}

// Hosts, protocols and the ingest auth (contracts/registration.md, "Ingest endpoints").
const (
	HostAPI       = "api"
	HostIngest    = "ingest"
	HostEdge      = "edge"
	HostLive      = "live"
	ProtocolHTTP  = "http"
	ProtocolGRPC  = "grpc"
	AuthIngestKey = "ingestKey"
)

// The invocation-token auth (contracts/registration.md, "Invocation-token endpoints"): a live-host
// endpoint authenticated by a bearer token the product issued for one unit of work and verifies
// itself (Workflows' invocation tokens). The kit reads the bearer and asks no one.
const (
	AuthInvocationToken  = "invocationToken"
	InvocationPathPrefix = "/v1/invocations/"
)

// InvocationToken reports whether the endpoint is authenticated by an invocation token.
func (e Endpoint) InvocationToken() bool { return e.Auth == AuthInvocationToken }

// The receipt-handle auth (contracts/registration.md, "Receipt-handle endpoints"): a live-host POST
// whose body carries receipt handles the product signed and verifies itself (Queues' acks and
// visibility changes). The handle is the capability: the kit reads no credential and asks no one.
const (
	AuthReceiptHandle = "receiptHandle"
	ReceiptPathPrefix = "/v1/receipts/"
)

// ReceiptHandle reports whether the endpoint is authorized by the receipt handles in its body.
func (e Endpoint) ReceiptHandle() bool { return e.Auth == AuthReceiptHandle }

// [being built] The delegated auth (contracts/registration.md, "Delegation"): an /internal/ endpoint
// on the API host serving another product's call made for a customer, with the caller's
// receiver-issued service credential and an X-Ezgh-Caller reference, authorized with its action
// through org-api's delegated check and recorded like the public call it stands in for.
const AuthDelegated = "delegated"

// Delegated reports whether the endpoint serves delegated calls.
func (e Endpoint) Delegated() bool { return e.Auth == AuthDelegated }

// The registry host and its auth (contracts/registration.md, "Registry endpoints").
const (
	HostRegistry = "registry"
	AuthRegistry = "registry"
)

// RegistryPaths are the paths a registry-host endpoint may have: the OCI distribution spec's,
// where <name> is a repository name (it spans path segments), <reference> a tag or digest,
// <digest> a digest and <session> an upload session.
var RegistryPaths = []string{"/v2/", "/v2/token", "/v2/<name>/manifests/<reference>", "/v2/<name>/blobs/<digest>", "/v2/<name>/blobs/uploads/", "/v2/<name>/blobs/uploads/<session>", "/v2/<name>/tags/list"}

// registryMethods are the methods each registry path takes.
var registryMethods = map[string][]string{
	"/v2/":                               {"GET"},
	"/v2/token":                          {"GET", "POST"},
	"/v2/<name>/manifests/<reference>":   {"GET", "HEAD", "PUT", "DELETE"},
	"/v2/<name>/blobs/<digest>":          {"GET", "HEAD", "DELETE"},
	"/v2/<name>/blobs/uploads/":          {"POST"},
	"/v2/<name>/blobs/uploads/<session>": {"GET", "PATCH", "PUT", "DELETE"},
	"/v2/<name>/tags/list":               {"GET"},
}

// registryUnauthenticated are the registry paths that may be auth none: the version check and the
// token endpoint, which check a credential's shape or validate it themselves.
var registryUnauthenticated = []string{"/v2/", "/v2/token"}

// The Git host and its auth (contracts/registration.md, "Git hosts").
const (
	HostGit = "git"
	AuthGit = "git"
)

// GitPaths are the paths a Git-host endpoint may have: Git's smart HTTP and Git LFS's, where <repo>
// is a repository's name and <oid> an LFS object's.
var GitPaths = []string{"/<repo>.git/info/refs", "/<repo>.git/git-upload-pack", "/<repo>.git/git-receive-pack", "/<repo>.git/info/lfs/objects/batch", "/<repo>.git/info/lfs/objects/<oid>"}

// gitMethods are the methods each Git path takes.
var gitMethods = map[string][]string{
	"/<repo>.git/info/refs":              {"GET"},
	"/<repo>.git/git-upload-pack":        {"POST"},
	"/<repo>.git/git-receive-pack":       {"POST"},
	"/<repo>.git/info/lfs/objects/batch": {"POST"},
	"/<repo>.git/info/lfs/objects/<oid>": {"GET", "PUT"},
}

// GitReads are the Git host's reads (a ref advertisement, a fetch, an LFS batch, an LFS download),
// which may record only their refusals (contracts/trails.md, "Git calls"); a push and an LFS upload
// may not.
var GitReads = []GitRoute{
	{"GET", "/<repo>.git/info/refs"},
	{"POST", "/<repo>.git/git-upload-pack"},
	{"POST", "/<repo>.git/info/lfs/objects/batch"},
	{"GET", "/<repo>.git/info/lfs/objects/<oid>"},
}

// gitRecorded are always recorded (a read may record refusals only): the ref advertisement, the
// first request of every clone and push, a fetch and a push. The LFS batch and download may be
// unrecorded.
var gitRecorded = []GitRoute{
	{"GET", "/<repo>.git/info/refs"},
	{"POST", "/<repo>.git/git-upload-pack"},
	{"POST", "/<repo>.git/git-receive-pack"},
}

// The data host and its auth (contracts/registration.md, "Data endpoints"): a product's own data
// plane, authenticated by a product data-plane key (Feature Flags' SDK keys, ezgh_fs_ and ezgh_fc_)
// that the product's data server resolves itself. Never recorded, no Idempotency-Key, no quotas.
const (
	HostData       = "data"
	AuthProductKey = "productKey"
)

// S3Namespace is the only namespace that declares the S3 host (contracts/registration.md, "The S3
// host"); s3Domain is the shape of that host's domain.
const S3Namespace = "objects"

var s3Domain = regexp.MustCompile(`^s3\.[a-z]{2}-[a-z]+-[0-9]+\.ezghapps\.com$`)

// The PKI host (contracts/registration.md, "PKI endpoints"): a reserved hostname, declared only by
// PKINamespace, whose endpoints are auth none, trail null, GET, HEAD or POST under one of its two
// parts' paths, and none of the API host's features.
const (
	HostPKI      = "pki"
	PKIHostname  = "pki.ezghcloud.com"
	PKINamespace = "certificates"
	// PKIPartACME is the ACME server's part of the host, under /acme/: HTTPS only.
	PKIPartACME = "acme"
	// PKIPartRevocation is the rest, under /ca/, /crl/ and /ocsp/: CA certificates, CRLs and OCSP,
	// served over plain HTTP too and never redirected.
	PKIPartRevocation = "revocation"
)

// pkiPathPrefixes are the PKI host's paths, by part.
var pkiPathPrefixes = []struct{ prefix, part string }{
	{"/acme/", PKIPartACME},
	{"/ca/", PKIPartRevocation},
	{"/crl/", PKIPartRevocation},
	{"/ocsp/", PKIPartRevocation},
}

// PKIPart is the part of the PKI host a path is in, PKIPartACME or PKIPartRevocation, or "" when
// it isn't a PKI path.
func PKIPart(path string) string {
	for _, p := range pkiPathPrefixes {
		if strings.HasPrefix(path, p.prefix) {
			return p.part
		}
	}
	return ""
}

// ErrorModelPrometheus is the Prometheus HTTP API's error body (Endpoint.ErrorModel).
const ErrorModelPrometheus = "prometheus"

// ErrorModelOpenAI is the OpenAI API's error body (Endpoint.ErrorModel; contracts/api-conventions.md,
// "OpenAI-compatible surface").
const ErrorModelOpenAI = "openai"

// RemoteWritePath is Prometheus remote write's ingest path, the one HTTP ingest path that isn't
// OTLP's /v1/<signal>: the metrics signal's, with its own wire format and error model.
const RemoteWritePath = "/api/v1/write"

// Computation reports whether the endpoint is a synchronous caller computation that keeps
// nothing (idempotencyException computation).
func (e Endpoint) Computation() bool { return e.IdempotencyException == "computation" }

// ReadOnly reports whether the endpoint's Trails events are readOnly (contracts/trails.md, "The
// event"): a GET, or a computation POST, unless it records per JSON-RPC method, whose methods may
// act (MCP Central's CallTool stays a write), or records refusals only, whose refusals are the only
// record of a high-volume call and are always kept, as writes are (EZGH Inference's
// CreateEmbeddings; [being built] a GET too, EZGH Git's content reads).
func (e Endpoint) ReadOnly() bool {
	if e.Trail != nil && e.Trail.Record == RecordRefusals {
		return false
	}
	if e.Method == "GET" {
		return true
	}
	return e.Method == "POST" && e.Computation() && !e.Trail.RPC()
}

// Credentials an endpoint takes (Endpoint.Credentials).
const (
	CredentialsBearer = "bearer"
	CredentialsCookie = "cookie"
)

// Public endpoint classes (contracts/registration.md, "Public endpoints").
const (
	PublicMetadata = "metadata"
	PublicRedirect = "redirect"
	// PublicCallback is a server-to-server notification that carries its own capability in the
	// path: a POST to CallbackPathPrefix + ":<param>" on the API host, never recorded, whose
	// handler authorizes from the path value itself (Workflows' POST /public/v1/callbacks/:token).
	PublicCallback = "callback"
)

// CallbackPathPrefix is where public callbacks are: /public/v1/callbacks/:<param>, one segment.
const CallbackPathPrefix = "/public/v1/callbacks/"

var callbackPath = regexp.MustCompile(`^/public/v1/callbacks/:[a-z][A-Za-z0-9]*$`)

// Components: the Deployments that serve a product's endpoints.
const (
	ComponentAPI  = "api"
	ComponentEdge = "edge"
	ComponentLive = "live"
)

// OnIngestHost reports whether the endpoint is served on the namespace's ingest host.
func (e Endpoint) OnIngestHost() bool { return e.Host == HostIngest }

// OnEdgeHost reports whether the endpoint is served on the product's edge host, where the
// organization comes from the host's alias, never the path.
func (e Endpoint) OnEdgeHost() bool { return e.Host == HostEdge }

// OnRegistryHost reports whether the endpoint is served on the product's registry host, by its
// own OCI server.
func (e Endpoint) OnRegistryHost() bool { return e.Host == HostRegistry }

// OnGitHost reports whether the endpoint is served on the product's Git host, by its own Git
// server.
func (e Endpoint) OnGitHost() bool { return e.Host == HostGit }

// OnDataHost reports whether the endpoint is served on the product's data host, by its own data
// server.
func (e Endpoint) OnDataHost() bool { return e.Host == HostData }

// OnPKIHost reports whether the endpoint is served on the PKI host, by the product's own PKI
// servers.
func (e Endpoint) OnPKIHost() bool { return e.Host == HostPKI }

// OnLiveHost reports whether the endpoint is served on the product's live host,
// <namespace>-live.ezghcloud.com, by its live component.
func (e Endpoint) OnLiveHost() bool { return e.Host == HostLive }

// ComponentName is the endpoint's component: ComponentAPI unless it names another.
func (e Endpoint) ComponentName() string {
	if e.Component == "" {
		return ComponentAPI
	}
	return e.Component
}

// Builtin reports whether the endpoint is one the kit serves in every component: GET /health,
// /ready or /openapi.json.
func (e Endpoint) Builtin() bool {
	return e.Method == "GET" && e.Auth == "none" && e.Public == "" && slices.Contains(Unauthenticated, e.Path)
}

// EdgeEndpoints are the endpoints served on the edge host, in manifest order.
func (m *Manifest) EdgeEndpoints() []Endpoint {
	var out []Endpoint
	for _, e := range m.Endpoints {
		if e.OnEdgeHost() {
			out = append(out, e)
		}
	}
	return out
}

// LiveEndpoints are the endpoints served on the live host, in manifest order.
func (m *Manifest) LiveEndpoints() []Endpoint {
	var out []Endpoint
	for _, e := range m.Endpoints {
		if e.OnLiveHost() {
			out = append(out, e)
		}
	}
	return out
}

// GRPC reports whether the endpoint is a gRPC method, whose Path is its full name.
func (e Endpoint) GRPC() bool { return e.Protocol == ProtocolGRPC }

// Transport is ProtocolGRPC for a gRPC endpoint, and ProtocolHTTP otherwise.
func (e Endpoint) Transport() string {
	if e.GRPC() {
		return ProtocolGRPC
	}
	return ProtocolHTTP
}

// Quota is a quota definition (contracts/schemas/quota-definition.schema.json).
type Quota struct {
	ID           string `json:"id"`
	DisplayName  string `json:"displayName"`
	Description  string `json:"description"`
	Kind         string `json:"kind"`
	Scope        string `json:"scope"`
	ResourceType string `json:"resourceType,omitempty"`
	PerRegion    bool   `json:"perRegion,omitempty"`
	Unit         string `json:"unit"`
	Default      int    `json:"default"`
	// TierDefaults is the value for organizations of a tier, instead of Default.
	TierDefaults map[string]int `json:"tierDefaults,omitempty"`
	Adjustable   bool           `json:"adjustable,omitempty"`
	// Max caps overrides; nil is no maximum.
	Max *int `json:"max,omitempty"`
	// Window is a rate quota's refill window: PT<n>S, PT<n>M or PT1H.
	Window string `json:"window,omitempty"`
	// Burst is a rate quota's bucket capacity at the default value; nil is the value.
	Burst *int `json:"burst,omitempty"`
	// LeaseTTL is how long a concurrency quota's lease lasts; "" is PT2M.
	LeaseTTL string `json:"leaseTtl,omitempty"`
	Status   string `json:"status,omitempty"`
}

// Quota kinds.
const (
	Allocation  = "allocation"
	Rate        = "rate"
	Usage       = "usage"
	Concurrency = "concurrency"
)

type Metering struct {
	Producer string   `json:"producer"`
	Meters   []string `json:"meters"`
}

type ServiceCredentials struct {
	IssuerPrefix string `json:"issuerPrefix"`
}

// Issue is one broken rule, at a dotted path into the manifest.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Parse reads a manifest strictly (unknown fields are an error, like the schema's
// additionalProperties: false) and checks its rules.
func Parse(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if issues := m.Check(); len(issues) > 0 {
		var lines []string
		for _, i := range issues {
			lines = append(lines, fmt.Sprintf("%s: %s", i.Path, i.Message))
		}
		return nil, fmt.Errorf("manifest breaks the registration rules:\n  %s", strings.Join(lines, "\n  "))
	}
	return &m, nil
}

// [being built] ProjectDeletionCheckPath is the route org-api calls, with GET, on a product whose
// manifest declares projectDeletionCheck (contracts/org-lifecycle.md, "Products that refuse").
const ProjectDeletionCheckPath = "/internal/v1/lifecycle/projects/:projectId/deletion-check"

// projectDeletionCheckIssues: a product that refuses project deletions declares it and serves the
// check, a service route on the API host taking the credential it issued org-api. Both ways: a
// route nobody declared would never be called.
func (m *Manifest) projectDeletionCheckIssues() []Issue {
	var issues []Issue
	declared := m.ProjectDeletionCheck != nil && *m.ProjectDeletionCheck
	check := slices.IndexFunc(m.Endpoints, func(e Endpoint) bool { return e.Method == "GET" && e.Path == ProjectDeletionCheckPath })
	if check != -1 {
		e := m.Endpoints[check]
		if !declared {
			issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.path", check), Message: "is the project deletion check: declare projectDeletionCheck: true"})
		}
		if e.Auth != "service" || (e.Host != "" && e.Host != "api") {
			issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.auth", check), Message: "the project deletion check is a service route on the API host"})
		}
	}
	if declared {
		if check == -1 {
			issues = append(issues, Issue{Path: "projectDeletionCheck", Message: "requires the endpoint GET " + ProjectDeletionCheckPath + " (auth service)"})
		}
		if m.ServiceCredentials == nil {
			issues = append(issues, Issue{Path: "serviceCredentials", Message: "is required: org-api calls the project deletion check with a credential the service issued"})
		}
	}
	return issues
}

// Registration is the IAM part of the manifest: the body of PUT /internal/v1/registrations/:ns.
type Registration struct {
	RegistrationVersion  int             `json:"registrationVersion"`
	Namespace            string          `json:"namespace"`
	DirectoryUsable      *bool           `json:"directoryUsable,omitempty"`
	ProjectDeletionCheck *bool           `json:"projectDeletionCheck,omitempty"`
	OrgAPI               *OrgAPI         `json:"orgApi,omitempty"`
	Scopes               []Scope         `json:"scopes,omitempty"`
	Delegation           *Delegation     `json:"delegation,omitempty"`
	DisplayName          string          `json:"displayName"`
	Description          string          `json:"description"`
	ResourceTypes        []ResourceType  `json:"resourceTypes"`
	ManagedPolicies      []ManagedPolicy `json:"managedPolicies"`
	Actions              []Action        `json:"actions"`
}

// Registration builds the registration the manifest sends.
func (m *Manifest) Registration() Registration {
	return Registration{
		RegistrationVersion: 1, Namespace: m.Namespace, DirectoryUsable: m.DirectoryUsable, ProjectDeletionCheck: m.ProjectDeletionCheck, OrgAPI: m.OrgAPI, Scopes: m.Scopes, Delegation: m.Delegation, DisplayName: m.DisplayName, Description: m.Description,
		ResourceTypes: nonNil(m.ResourceTypes), ManagedPolicies: nonNil(m.ManagedPolicies), Actions: nonNil(m.Actions),
	}
}

// QuotasRegistration is the body of PUT /internal/v1/registrations/:namespace on quotas-api
// (contracts/quotas.md, "Registering quotas"): the manifest's quotas, and where quotas-api reads
// the product's allocation usage.
type QuotasRegistration struct {
	RegistrationVersion int    `json:"registrationVersion"`
	Namespace           string `json:"namespace"`
	// DisplayName is the product's name for people, the manifest's (quotas 1.2).
	DisplayName string  `json:"displayName,omitempty"`
	InternalURL *string `json:"internalUrl"`
	Quotas      []Quota `json:"quotas"`
}

// QuotasRegistration builds the quotas registration, with the chart's own Service as internalURL
// ("" sends null: allowed only while the namespace has no active allocation quota).
func (m *Manifest) QuotasRegistration(internalURL string) QuotasRegistration {
	r := QuotasRegistration{RegistrationVersion: 1, Namespace: m.Namespace, DisplayName: m.DisplayName, Quotas: nonNil(m.Quotas)}
	if internalURL != "" {
		r.InternalURL = &internalURL
	}
	return r
}

// NeedsInternalURL reports whether the namespace has an active allocation quota, whose usage
// quotas-api reads from the product.
func (m *Manifest) NeedsInternalURL() bool {
	for _, q := range m.Quotas {
		if q.Kind == Allocation && q.Status != "deprecated" {
			return true
		}
	}
	return false
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// EventSource is the namespace's Trails eventSource.
func (m *Manifest) EventSource() string { return m.Namespace + ".ezghcloud.com" }

// Endpoint finds the entry for a route, by method and manifest path.
func (m *Manifest) Endpoint(method, path string) (Endpoint, bool) {
	for _, e := range m.Endpoints {
		if e.Method == method && e.Path == path {
			return e, true
		}
	}
	return Endpoint{}, false
}

// Action finds a declared action by ID.
func (m *Manifest) Action(id string) (Action, bool) {
	for _, a := range m.Actions {
		if a.ID == id {
			return a, true
		}
	}
	return Action{}, false
}

// IngestEndpoints are the endpoints served on the ingest host, in manifest order.
func (m *Manifest) IngestEndpoints() []Endpoint {
	var out []Endpoint
	for _, e := range m.Endpoints {
		if e.OnIngestHost() {
			out = append(out, e)
		}
	}
	return out
}

// IngestEndpoint finds an ingest-host endpoint by protocol (ProtocolHTTP or ProtocolGRPC) and path
// (the gRPC method's full name for gRPC).
func (m *Manifest) IngestEndpoint(protocol, path string) (Endpoint, bool) {
	for _, e := range m.IngestEndpoints() {
		if e.Transport() == protocol && e.Path == path {
			return e, true
		}
	}
	return Endpoint{}, false
}

// IngestRoute is a route an ingest server serves: POST of Path over HTTP, or the gRPC method whose
// full name is Path.
type IngestRoute struct {
	Protocol string // ProtocolHTTP or ProtocolGRPC
	Path     string
}

// CheckIngestRoutes compares what an ingest server serves with the manifest's ingest-host
// endpoints, as Service.Handler does for the API host: a route without an entry, an entry without
// a route, or a route served twice is an error, and the ingest server refuses to start. It compares
// both kinds of ingest-host endpoint: ingestKey endpoints and [being built] internal producer routes
// (auth service under /internal/). Health, readiness and metrics aren't ingest routes and aren't
// listed.
func (m *Manifest) CheckIngestRoutes(served []IngestRoute) error {
	want := map[IngestRoute]bool{}
	for _, e := range m.IngestEndpoints() {
		want[IngestRoute{Protocol: e.Transport(), Path: e.Path}] = true
	}
	var problems []string
	seen := map[IngestRoute]bool{}
	for _, r := range served {
		if seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served twice", r.Protocol, r.Path))
			continue
		}
		seen[r] = true
		if !want[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served but isn't an ingest endpoint in the manifest", r.Protocol, r.Path))
		}
	}
	for r := range want {
		if !seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is in the manifest but isn't served", r.Protocol, r.Path))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("the ingest routes and the manifest's ingest endpoints differ:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// GitEndpoints are the endpoints served on the Git host, in manifest order.
func (m *Manifest) GitEndpoints() []Endpoint {
	var out []Endpoint
	for _, e := range m.Endpoints {
		if e.OnGitHost() {
			out = append(out, e)
		}
	}
	return out
}

// GitRoute is a route a Git server serves: Method of the manifest's Path
// (/<repo>.git/git-upload-pack).
type GitRoute struct {
	Method string
	Path   string
}

// CheckGitRoutes compares what a Git server serves with the manifest's Git-host endpoints, as
// CheckRegistryRoutes does: a route without an entry, an entry without a route, or a route served
// twice is an error, and the Git server refuses to start. Health, readiness and metrics aren't Git
// routes and aren't listed.
func (m *Manifest) CheckGitRoutes(served []GitRoute) error {
	want := map[GitRoute]bool{}
	for _, e := range m.GitEndpoints() {
		want[GitRoute{Method: e.Method, Path: e.Path}] = true
	}
	var problems []string
	seen := map[GitRoute]bool{}
	for _, r := range served {
		if seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served twice", r.Method, r.Path))
			continue
		}
		seen[r] = true
		if !want[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served but isn't a Git endpoint in the manifest", r.Method, r.Path))
		}
	}
	for r := range want {
		if !seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is in the manifest but isn't served", r.Method, r.Path))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("the Git routes and the manifest's Git endpoints differ:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// RegistryEndpoints are the endpoints served on the registry host, in manifest order.
func (m *Manifest) RegistryEndpoints() []Endpoint {
	var out []Endpoint
	for _, e := range m.Endpoints {
		if e.OnRegistryHost() {
			out = append(out, e)
		}
	}
	return out
}

// RegistryRoute is a route a registry server serves: Method of the manifest's Path
// (/v2/<name>/manifests/<reference>).
type RegistryRoute struct {
	Method string
	Path   string
}

// CheckRegistryRoutes compares what a registry server serves with the manifest's registry-host
// endpoints, as Service.Handler does for the API host: a route without an entry, an entry without
// a route, or a route served twice is an error, and the registry server refuses to start. Health,
// readiness and metrics aren't registry routes and aren't listed.
func (m *Manifest) CheckRegistryRoutes(served []RegistryRoute) error {
	want := map[RegistryRoute]bool{}
	for _, e := range m.RegistryEndpoints() {
		want[RegistryRoute{Method: e.Method, Path: e.Path}] = true
	}
	var problems []string
	seen := map[RegistryRoute]bool{}
	for _, r := range served {
		if seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served twice", r.Method, r.Path))
			continue
		}
		seen[r] = true
		if !want[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served but isn't a registry endpoint in the manifest", r.Method, r.Path))
		}
	}
	for r := range want {
		if !seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is in the manifest but isn't served", r.Method, r.Path))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("the registry routes and the manifest's registry endpoints differ:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// DataEndpoints are the endpoints served on the data host, in manifest order.
func (m *Manifest) DataEndpoints() []Endpoint {
	var out []Endpoint
	for _, e := range m.Endpoints {
		if e.OnDataHost() {
			out = append(out, e)
		}
	}
	return out
}

// DataRoute is a route a data server serves: Method of the manifest's Path
// (/ofrep/v1/evaluate/flags/:key).
type DataRoute struct {
	Method string
	Path   string
}

// CheckDataRoutes compares what a data server serves with the manifest's data-host endpoints, as
// Service.Handler does for the API host: a route without an entry, an entry without a route, or a
// route served twice is an error, and the data server refuses to start. Health, readiness and
// metrics aren't data routes and aren't listed.
func (m *Manifest) CheckDataRoutes(served []DataRoute) error {
	want := map[DataRoute]bool{}
	for _, e := range m.DataEndpoints() {
		want[DataRoute{Method: e.Method, Path: e.Path}] = true
	}
	var problems []string
	seen := map[DataRoute]bool{}
	for _, r := range served {
		if seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served twice", r.Method, r.Path))
			continue
		}
		seen[r] = true
		if !want[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served but isn't a data endpoint in the manifest", r.Method, r.Path))
		}
	}
	for r := range want {
		if !seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is in the manifest but isn't served", r.Method, r.Path))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("the data routes and the manifest's data endpoints differ:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// PKIEndpoints are the endpoints served on the PKI host, in manifest order.
func (m *Manifest) PKIEndpoints() []Endpoint {
	var out []Endpoint
	for _, e := range m.Endpoints {
		if e.OnPKIHost() {
			out = append(out, e)
		}
	}
	return out
}

// PKIRoute is a route a PKI server serves: Method of the manifest's Path (/crl/:file).
type PKIRoute struct {
	Method string
	Path   string
}

// CheckPKIRoutes compares what one PKI server serves with the manifest's PKI-host endpoints, as
// Service.Handler does for the API host. The host has two parts (PKIPart), which a product may
// serve from one server or one each (Private CA's pca-ocsp and pca-acme), so a server serves every
// endpoint of each part it serves at least one route of. A route without an entry, a route served
// twice, an entry of a served part without a route, or no PKI route at all is an error, and the
// server refuses to start. Health, readiness and metrics aren't PKI routes and aren't listed.
func (m *Manifest) CheckPKIRoutes(served []PKIRoute) error {
	want := map[PKIRoute]bool{}
	for _, e := range m.PKIEndpoints() {
		want[PKIRoute{Method: e.Method, Path: e.Path}] = true
	}
	var problems []string
	seen := map[PKIRoute]bool{}
	parts := map[string]bool{}
	for _, r := range served {
		if seen[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served twice", r.Method, r.Path))
			continue
		}
		seen[r] = true
		if !want[r] {
			problems = append(problems, fmt.Sprintf("%s %s is served but isn't a PKI endpoint in the manifest", r.Method, r.Path))
			continue
		}
		parts[PKIPart(r.Path)] = true
	}
	if len(served) == 0 {
		problems = append(problems, "the server serves no PKI route")
	}
	for r := range want {
		if !seen[r] && parts[PKIPart(r.Path)] {
			problems = append(problems, fmt.Sprintf("%s %s is in the manifest but isn't served", r.Method, r.Path))
		}
	}
	if len(problems) > 0 {
		slices.Sort(problems)
		return fmt.Errorf("the PKI routes and the manifest's PKI endpoints differ:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// Quota finds a quota by ID.
func (m *Manifest) Quota(id string) (Quota, bool) {
	for _, q := range m.Quotas {
		if q.ID == id {
			return q, true
		}
	}
	return Quota{}, false
}

// ResourceTypeByPrefix finds a resource type by its slug prefix.
func (m *Manifest) ResourceTypeByPrefix(prefix string) (ResourceType, bool) {
	for _, t := range m.ResourceTypes {
		if t.Prefix == prefix {
			return t, true
		}
	}
	return ResourceType{}, false
}

// ---- The rules --------------------------------------------------------------------------------

// ReservedNamespaces are the platform's: public namespaces and hosts, org-api's own action
// prefixes, and platform words.
var ReservedNamespaces = []string{
	"organizations", "iam", "signin", "billing", "trails", "orgs", "auth", "login", "onboarding", "audit", "console", "argo",
	"projects", "users", "groups", "bots", "policies",
	"ezgh", "ezghcloud", "platform", "internal", "admin", "metering", "api", "www",
	// [being built] The reserved PKI host's name (PKIHostname): no namespace takes it.
	"pki",
	// [being built] org-api's folder actions (contracts/identity-and-authz.md, "Folder actions").
	"folders",
}

// ReservedOperations are org-api operations only the namespaces listed may declare, by operation:
// they act without a customer credential (contracts/registration.md, "org-api operations"). Any
// other namespace declaring one is an issue, as org-api's.
var ReservedOperations = map[string][]string{"permissions.principalBatch": {PKINamespace, S3Namespace, "git"}, "instanceBots.lookup": {"compute", S3Namespace}, "instanceBots.mint": {"compute"}, "organizations.rootContact": {"network", "kubernetes"}, "kubernetes.mint": {"kubernetes"}, "kubernetes.authenticate": {"kubernetes"}}

// ReservedPrefixes are slug prefixes the platform already uses ([being built] igk: org-api's
// project ingest keys; oip: its OIDC providers; fld: its folders).
var ReservedPrefixes = []string{"org", "prj", "bot", "pol", "grp", "sso", "bgt", "exp", "igk", "oip", "fld"}

// ClaimedPrefixes belong to a product namespace, which may register them.
var ClaimedPrefixes = map[string]string{"dir": "sso", "usr": "sso", "dgp": "sso", "app": "sso", "idp": "sso"}

// PlatformManagedPolicies are org-api's built-in managed policies.
var PlatformManagedPolicies = []string{"AdministratorAccess", "BasicAccess", "ReadOnlyAccess", "ProjectsFullAccess", "BillingFullAccess", "IAMFullAccess", "TrailsFullAccess", "KubernetesNodeAccess", "KubernetesClusterAccess"}

var (
	namespacePattern    = regexp.MustCompile(`^[a-z]{2,32}$`)
	actionPattern       = regexp.MustCompile(`^[a-z]{2,32}(\.[a-z][A-Za-z]*){1,2}$`)
	prefixPattern       = regexp.MustCompile(`^[a-z]{3}$`)
	typeNamePattern     = regexp.MustCompile(`^[a-z][A-Za-z0-9]{1,63}$`)
	policyPattern       = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{2,63}Access$`)
	eventNamePattern    = regexp.MustCompile(`^[A-Z][A-Za-z0-9]{2,63}$`)
	pathPattern         = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+|/:[a-z][A-Za-z0-9]*)+$`)
	paramPattern        = regexp.MustCompile(`:[A-Za-z0-9]+`)
	operationPath       = regexp.MustCompile(`^/v1/organizations/:orgId/operations/:operationId$`)
	quotaIDPattern      = regexp.MustCompile(`^[a-z]{2,32}\.[a-z][A-Za-z0-9]{2,63}(\.[a-z][A-Za-z0-9]{1,63})?$`)
	quotaUnitPattern    = regexp.MustCompile(`^[a-z][a-z-]*$`)
	quotaTypePattern    = regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	tierPattern         = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	durationPattern     = regexp.MustCompile(`^PT(?:([1-9][0-9]*)S|([1-9][0-9]*)M|(1)H)$`)
	ingestHTTPPath      = regexp.MustCompile(`^/v1/[a-z]+$`)
	prometheusPath      = regexp.MustCompile(`^/v1/organizations/:orgId(/[^/]+)*/prometheus/api/v1/[^/]`)
	openAIPath          = regexp.MustCompile(`^/v1/organizations/:orgId/projects/:projectId/openai/v1/[^/]`)
	grpcMethodPath      = regexp.MustCompile(`^/[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*\.[A-Z][A-Za-z0-9]*/[A-Z][A-Za-z0-9]*$`)
	operationPattern    = regexp.MustCompile(`^[a-z][A-Za-z]*\.[a-z][A-Za-z]*$`)
	edgeDomainPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}\.ezgh\.app$`)
	edgeCookiePattern   = regexp.MustCompile(`^__Host-[A-Za-z0-9_-]{1,64}$`)
	edgeAudiencePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
)

// MaxOrgAPIOperations is the most org-api operations a namespace declares.
const MaxOrgAPIOperations = 20

// DurationMS reads a rate window or a lease TTL (PT<n>S, PT<n>M or PT1H, from a second to an
// hour) as milliseconds; false for anything else.
func DurationMS(text string) (int64, bool) {
	m := durationPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	ms := int64(3_600_000)
	for i, unit := range []int64{1000, 60_000} {
		if m[i+1] != "" {
			n, err := strconv.ParseInt(m[i+1], 10, 64)
			if err != nil || n > 3600 {
				return 0, false
			}
			ms = n * unit
		}
	}
	return ms, ms >= 1000 && ms <= 3_600_000
}

func pascal(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// RegistrationIssues checks a registration (or a manifest's IAM part).
func RegistrationIssues(r Registration) []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	ns := r.Namespace
	if !namespacePattern.MatchString(ns) {
		add("namespace", "must be 2-32 lowercase letters")
		return issues
	}
	if slices.Contains(ReservedNamespaces, ns) {
		add("namespace", "%s is reserved", ns)
	}

	if ns == "sso" && r.DirectoryUsable != nil && *r.DirectoryUsable {
		add("directoryUsable", "SSO management is never directory usable")
	}

	if r.OrgAPI != nil {
		ops := r.OrgAPI.Operations
		if len(ops) < 1 || len(ops) > MaxOrgAPIOperations {
			add("orgApi.operations", "must list 1-%d org-api operations", MaxOrgAPIOperations)
		}
		for i, op := range ops {
			switch {
			case !operationPattern.MatchString(op):
				add(fmt.Sprintf("orgApi.operations.%d", i), "must be an org-api operation like permissions.batch: <group>.<operation>, letters only")
			case slices.Index(ops, op) != i:
				add(fmt.Sprintf("orgApi.operations.%d", i), "%s is declared twice", op)
			case ReservedOperations[op] != nil && !slices.Contains(ReservedOperations[op], ns):
				add(fmt.Sprintf("orgApi.operations.%d", i), "%s is reserved to namespace %s", op, strings.Join(ReservedOperations[op], " or "))
			}
		}
	}

	hasDirectory := slices.ContainsFunc(r.ResourceTypes, func(t ResourceType) bool { return t.Name == "directory" })
	typeNames := map[string]bool{}
	seenPrefixes := map[string]bool{}
	for i, t := range r.ResourceTypes {
		if !prefixPattern.MatchString(t.Prefix) {
			add(fmt.Sprintf("resourceTypes.%d.prefix", i), "must be 3 lowercase letters")
		}
		if slices.Contains(ReservedPrefixes, t.Prefix) {
			add(fmt.Sprintf("resourceTypes.%d.prefix", i), "%s is reserved", t.Prefix)
		}
		if owner := ClaimedPrefixes[t.Prefix]; owner != "" && owner != ns {
			add(fmt.Sprintf("resourceTypes.%d.prefix", i), "%s belongs to %s", t.Prefix, owner)
		}
		if t.Parent == "directory" && (!hasDirectory || t.Name == "directory") {
			add(fmt.Sprintf("resourceTypes.%d.parent", i), "directory parent requires a separate directory resource type in this namespace")
		}
		if seenPrefixes[t.Prefix] {
			add(fmt.Sprintf("resourceTypes.%d.prefix", i), "%s is declared twice", t.Prefix)
		}
		seenPrefixes[t.Prefix] = true
		if !typeNamePattern.MatchString(t.Name) {
			add(fmt.Sprintf("resourceTypes.%d.name", i), "must be camelCase")
		}
		if typeNames[t.Name] || t.Name == "organization" || t.Name == "project" {
			add(fmt.Sprintf("resourceTypes.%d.name", i), "%s is taken", t.Name)
		}
		typeNames[t.Name] = true
		if t.Parent != "organization" && t.Parent != "project" && t.Parent != "directory" {
			add(fmt.Sprintf("resourceTypes.%d.parent", i), "must be organization, project or directory")
		}
	}

	own := map[string]bool{}
	for i, p := range r.ManagedPolicies {
		path := fmt.Sprintf("managedPolicies.%d.name", i)
		switch {
		case !policyPattern.MatchString(p.Name):
			add(path, "must be PascalCase and end in Access")
		case slices.Contains(PlatformManagedPolicies, p.Name):
			add(path, "%s is a platform policy", p.Name)
		case !strings.HasPrefix(p.Name, pascal(ns)):
			add(path, "must start with %s", pascal(ns))
		}
		if own[p.Name] {
			add(path, "%s is declared twice", p.Name)
		}
		own[p.Name] = true
	}

	used := map[string]bool{}
	seenActions := map[string]bool{}
	for i, a := range r.Actions {
		at := func(field string) string { return fmt.Sprintf("actions.%d.%s", i, field) }
		if !actionPattern.MatchString(a.ID) {
			add(at("id"), "must be <namespace>.<resource>.<verb> or <namespace>.<verb>, letters only")
		}
		if !strings.HasPrefix(a.ID, ns+".") {
			add(at("id"), "must start with %s.", ns)
		}
		if seenActions[a.ID] {
			add(at("id"), "%s is declared twice", a.ID)
		}
		seenActions[a.ID] = true
		if a.Resource != "organization" && a.Resource != "project" && !typeNames[a.Resource] {
			add(at("resource"), "must be organization, project or one of the namespace's resource types")
		}
		// ReadOnlyAccess allows *.get and *.list by pattern, so names and levels must agree.
		verb := a.ID[strings.LastIndex(a.ID, ".")+1:]
		if verb == "list" && a.AccessLevel != "list" {
			add(at("accessLevel"), "a .list action is accessLevel list")
		}
		if verb == "get" && a.AccessLevel != "read" {
			add(at("accessLevel"), "a .get action is accessLevel read")
		}
		if !slices.Contains([]string{"list", "read", "write"}, a.AccessLevel) {
			add(at("accessLevel"), "must be list, read or write")
		}
		readOnly := slices.Contains(a.ManagedPolicies, "ReadOnlyAccess")
		if a.AccessLevel != "write" && !readOnly {
			add(at("managedPolicies"), "list and read actions are in ReadOnlyAccess")
		}
		if a.AccessLevel == "write" && readOnly {
			add(at("managedPolicies"), "write actions can't be in ReadOnlyAccess")
		}
		if a.Ingest && a.AccessLevel != "write" {
			add(at("ingest"), "an ingest action is accessLevel write")
		}
		if a.Ingest && a.Resource != "project" {
			add(at("ingest"), "an ingest action is checked against the project")
		}
		if a.PlatformOnly && len(a.ManagedPolicies) > 0 {
			add(at("platformOnly"), "a platform-only action is in no managed policy")
		}
		for j, name := range a.ManagedPolicies {
			if name == "ReadOnlyAccess" {
				continue
			}
			if !own[name] {
				add(fmt.Sprintf("actions.%d.managedPolicies.%d", i, j), "%s isn't one of the namespace's managed policies (AdministratorAccess is implied)", name)
			}
			used[name] = true
		}
	}
	for i, p := range r.ManagedPolicies {
		if !used[p.Name] {
			add(fmt.Sprintf("managedPolicies.%d", i), "%s includes no actions", p.Name)
		}
	}
	issues = append(issues, scopeIssues(r)...)
	issues = append(issues, delegationIssues(r)...)
	return issues
}

// delegationIssues checks a registration's delegation as org-api does, with the shape the schema
// says (contracts/registration.md, "Delegation"): 1 to 20 entries, each calling namespace once and
// never the namespace's own; 1 to 50 unique patterns of the namespace's own actions, each covering
// one; at most 5 unique origins, never the receiver or the entry's own namespace.
func delegationIssues(r Registration) []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if r.Delegation == nil {
		return nil
	}
	ns := r.Namespace
	callers := r.Delegation.Callers
	if len(callers) < 1 || len(callers) > MaxDelegationCallers {
		add("delegation.callers", "lists 1 to %d callers", MaxDelegationCallers)
	}
	seen := map[string]bool{}
	for i, entry := range callers {
		at := func(field string) string { return fmt.Sprintf("delegation.callers.%d.%s", i, field) }
		switch {
		case !namespacePattern.MatchString(entry.Namespace):
			add(at("namespace"), "must be a namespace: 2-32 lowercase letters")
		case entry.Namespace == ns:
			add(at("namespace"), "a namespace never delegates to itself")
		case seen[entry.Namespace]:
			add(at("namespace"), "%s is declared twice", entry.Namespace)
		}
		seen[entry.Namespace] = true
		if len(entry.Actions) < 1 || len(entry.Actions) > MaxDelegationActions {
			add(at("actions"), "lists 1 to %d action patterns", MaxDelegationActions)
		}
		for j, pattern := range entry.Actions {
			path := fmt.Sprintf("%s.%d", at("actions"), j)
			switch {
			case slices.Index(entry.Actions, pattern) != j:
				add(path, "%s is listed twice", pattern)
			case len(pattern) > 128 || !actionPatternSyntax.MatchString(pattern) || !strings.HasPrefix(pattern, ns+"."):
				add(path, "must be an action pattern starting with %s.", ns)
			case !slices.ContainsFunc(r.Actions, func(a Action) bool { return MatchesActionPattern(pattern, a.ID) }):
				add(path, "%q doesn't match any of the namespace's actions", pattern)
			}
		}
		if len(entry.Origins) > MaxDelegationOrigins {
			add(at("origins"), "lists at most %d origins", MaxDelegationOrigins)
		}
		for j, origin := range entry.Origins {
			path := fmt.Sprintf("%s.%d", at("origins"), j)
			switch {
			case !namespacePattern.MatchString(origin):
				add(path, "must be a namespace: 2-32 lowercase letters")
			case slices.Index(entry.Origins, origin) != j:
				add(path, "%s is listed twice", origin)
			case origin == ns || origin == entry.Namespace:
				add(path, "an origin is never the receiver or the entry's own namespace")
			}
		}
	}
	return issues
}

var (
	scopeNamePattern    = regexp.MustCompile(`^[a-z]{2,32}:[a-z]{2,32}$`)
	actionPatternSyntax = regexp.MustCompile(`^[A-Za-z*]+(\.[A-Za-z*]+)*$`)
)

// scopeIssues checks a registration's scopes as org-api does (registration/rules.ts), with the
// shape the schema says: at most 20, each <namespace>:<word> once, with 1-50 unique patterns that
// start with the namespace and cover at least one of its actions, and only in a directory-usable
// namespace.
func scopeIssues(r Registration) []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	ns := r.Namespace
	if len(r.Scopes) > 0 && (r.DirectoryUsable == nil || !*r.DirectoryUsable) {
		add("scopes", "only a directory-usable namespace declares scopes")
	}
	if len(r.Scopes) > MaxScopes {
		add("scopes", "at most %d scopes", MaxScopes)
	}
	names := map[string]bool{}
	for i, scope := range r.Scopes {
		at := func(field string) string { return fmt.Sprintf("scopes.%d.%s", i, field) }
		switch {
		case !scopeNamePattern.MatchString(scope.Name):
			add(at("name"), "must be <namespace>:<word>, lowercase letters")
		case !strings.HasPrefix(scope.Name, ns+":"):
			add(at("name"), "must start with %s:", ns)
		case names[scope.Name]:
			add(at("name"), "%s is declared twice", scope.Name)
		}
		names[scope.Name] = true
		if scope.Status != "" && scope.Status != "active" && scope.Status != "deprecated" {
			add(at("status"), "must be active or deprecated")
		}
		if len(scope.Actions) < 1 || len(scope.Actions) > MaxScopeActions {
			add(at("actions"), "must list 1-%d action patterns", MaxScopeActions)
		}
		for j, pattern := range scope.Actions {
			path := fmt.Sprintf("scopes.%d.actions.%d", i, j)
			switch {
			case slices.Index(scope.Actions, pattern) != j:
				add(path, "%s is listed twice", pattern)
			case len(pattern) > 128 || !actionPatternSyntax.MatchString(pattern) || !strings.HasPrefix(pattern, ns+"."):
				add(path, "must be an action pattern starting with %s.", ns)
			case !slices.ContainsFunc(r.Actions, func(a Action) bool { return MatchesActionPattern(pattern, a.ID) }):
				add(path, "%q doesn't match any of the namespace's actions", pattern)
			}
		}
	}
	return issues
}

// MatchesActionPattern is org-api's action pattern match: `*` stands for any run of characters
// (authz/glob.ts), so mcpcentral.connections.* covers mcpcentral.connections.connect.
func MatchesActionPattern(pattern, action string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == action
	}
	pieces := strings.Split(pattern, "*")
	first, last := pieces[0], pieces[len(pieces)-1]
	if !strings.HasPrefix(action, first) || !strings.HasSuffix(action, last) || len(action) < len(first)+len(last) {
		return false
	}
	at, end := len(first), len(action)-len(last)
	for _, piece := range pieces[1 : len(pieces)-1] {
		found := strings.Index(action[at:], piece)
		if found == -1 || at+found+len(piece) > end {
			return false
		}
		at += found + len(piece)
	}
	return true
}

// Check applies every rule: the registration's, then the endpoints', quotas' and meters'.
// Unauthenticated are the only paths with auth none, all GET: liveness, readiness and the
// OpenAPI document.
var Unauthenticated = []string{"/health", "/ready", "/openapi.json"}

func (m *Manifest) Check() []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if m.ManifestVersion != 1 {
		add("manifestVersion", "must be 1")
	}
	issues = append(issues, RegistrationIssues(m.Registration())...)
	ns := m.Namespace
	if want := m.apiHostnames(); !slices.Contains(want, m.API.Hostname) {
		if len(want) == 1 {
			add("api.hostname", "must be %s: the registry, data or Git host takes %s.ezghcloud.com", want[0], ns)
		} else {
			add("api.hostname", "must be %s or %s", want[0], want[1])
		}
	}
	if m.API.Port < 1024 || m.API.Port > 65535 {
		add("api.port", "must be 1024-65535")
	}
	issues = append(issues, m.ingestHostIssues()...)
	issues = append(issues, m.edgeHostIssues()...)
	issues = append(issues, m.liveHostIssues()...)
	issues = append(issues, m.registryHostIssues()...)
	issues = append(issues, m.gitHostIssues()...)
	issues = append(issues, m.dataHostIssues()...)
	issues = append(issues, m.pkiHostIssues()...)
	issues = append(issues, m.s3HostIssues()...)

	actions := map[string]Action{}
	for _, a := range m.Actions {
		if _, seen := actions[a.ID]; !seen {
			actions[a.ID] = a
		}
	}
	typeNames := map[string]bool{}
	for _, t := range m.ResourceTypes {
		typeNames[t.Name] = true
	}
	quotas := map[string]Quota{}
	for _, q := range m.Quotas {
		if _, seen := quotas[q.ID]; !seen {
			quotas[q.ID] = q
		}
	}
	routes := map[string]bool{}
	servesOpenAPI := false
	eventNames := map[string]int{}
	issues = append(issues, m.crossHostRouteIssues()...)
	for i, e := range m.Endpoints {
		at := func(field string) string { return fmt.Sprintf("endpoints.%d.%s", i, field) }
		switch {
		case e.Method == "HEAD" && !e.OnRegistryHost() && !e.OnPKIHost():
			add(at("method"), "HEAD is only on registry-host and PKI-host endpoints")
		case e.Method != "HEAD" && !slices.Contains([]string{"GET", "POST", "PUT", "PATCH", "DELETE"}, e.Method):
			add(at("method"), "must be GET, POST, PUT, PATCH or DELETE")
		}
		// A registry path is one of the OCI distribution spec's (registryEndpointIssues).
		if !e.OnRegistryHost() && !e.OnGitHost() && !pathPattern.MatchString(e.Path) {
			add(at("path"), "must be /segments and /:params")
		}
		// Routes overlap within one server: the ingest server, or one component's API server (every
		// component serves the kit's built-ins).
		shape := e.Method + " " + paramPattern.ReplaceAllString(e.Path, ":")
		var keys []string
		switch {
		case e.OnIngestHost():
			keys = []string{HostIngest + " " + shape}
		case e.OnRegistryHost():
			keys = []string{HostRegistry + " " + e.Method + " " + e.Path}
		case e.OnGitHost():
			keys = []string{HostGit + " " + e.Method + " " + e.Path}
		case e.OnDataHost():
			keys = []string{HostData + " " + shape}
		case e.OnPKIHost():
			keys = []string{HostPKI + " " + shape}
		case e.Builtin() && e.Component == "":
			keys = []string{ComponentAPI + " " + shape, ComponentEdge + " " + shape, ComponentLive + " " + shape}
		case e.ComponentName() == ComponentAPI || e.ComponentName() == ComponentLive:
			// The api and live components are one route space: the OpenAPI document and the kit's
			// routes key endpoints by method and path, whichever host serves them.
			keys = []string{ComponentAPI + " " + shape, ComponentLive + " " + shape}
		default:
			keys = []string{e.ComponentName() + " " + shape}
		}
		if slices.ContainsFunc(keys, func(k string) bool { return routes[k] }) {
			add(at("path"), "%s %s overlaps another endpoint", e.Method, e.Path)
		}
		for _, k := range keys {
			routes[k] = true
		}
		if e.Builtin() && e.Path == "/openapi.json" {
			servesOpenAPI = true
		}
		if e.Host != "" && e.Host != HostAPI && e.Host != HostIngest && e.Host != HostEdge && e.Host != HostLive && e.Host != HostRegistry && e.Host != HostData && e.Host != HostPKI && e.Host != HostGit {
			add(at("host"), "must be api, ingest, edge, live, registry, data, pki or git")
		}
		if e.OnRegistryHost() {
			issues = append(issues, registryEndpointIssues(e, i)...)
		}
		if e.OnGitHost() {
			issues = append(issues, gitEndpointIssues(e, i)...)
		}
		if e.OnDataHost() != (e.Auth == AuthProductKey) {
			add(at("host"), "data-host endpoints are auth productKey, and only they are")
		}
		if e.OnDataHost() {
			issues = append(issues, dataEndpointIssues(e, i)...)
		}
		if e.OnPKIHost() {
			issues = append(issues, pkiEndpointIssues(e, i)...)
		}
		if e.Component != "" && e.Component != ComponentAPI && e.Component != ComponentEdge && e.Component != ComponentLive {
			add(at("component"), "must be api, edge or live")
		}
		if e.Component != "" && e.OnIngestHost() {
			add(at("component"), "ingest-host endpoints are the ingest server's: they name no component")
		}
		if e.Component == ComponentEdge && !e.OnEdgeHost() {
			add(at("component"), "the edge component serves only edge-host endpoints")
		}
		if e.Component == ComponentLive && !e.OnLiveHost() {
			add(at("component"), "the live component serves only live-host endpoints")
		}
		if e.OnLiveHost() && !e.Builtin() && !e.InvocationToken() && !e.ReceiptHandle() {
			issues = append(issues, liveEndpointIssues(e, i)...)
		}
		if e.InvocationToken() {
			issues = append(issues, invocationEndpointIssues(e, i)...)
		}
		if e.ReceiptHandle() {
			issues = append(issues, receiptEndpointIssues(e, i)...)
		}
		if e.Public != "" && e.Public != PublicMetadata && e.Public != PublicRedirect && e.Public != PublicCallback {
			add(at("public"), "must be metadata, redirect or callback")
		}
		if e.Public != "" && e.Auth != "none" {
			add(at("public"), "only an unauthenticated endpoint (auth none) is public")
		}
		switch {
		case e.Credentials != "" && e.Credentials != CredentialsBearer && e.Credentials != CredentialsCookie:
			add(at("credentials"), "must be bearer or cookie")
		case e.Credentials != "" && e.Auth != "caller" && e.Public != PublicRedirect:
			add(at("credentials"), "only caller endpoints and public redirects take credentials")
		case e.Credentials == CredentialsCookie && !e.OnEdgeHost():
			add(at("credentials"), "the product's cookie is taken only on the edge host")
		}
		switch {
		case e.TimeoutSeconds < 0 || e.TimeoutSeconds > MaxTimeoutSeconds:
			add(at("timeoutSeconds"), "must be 1-%d", MaxTimeoutSeconds)
		case e.TimeoutSeconds != 0 && (e.Builtin() || e.OnIngestHost()):
			add(at("timeoutSeconds"), "the kit's own endpoints and ingest endpoints keep the server's timeouts")
		}
		if e.Builtin() && (e.Component != "" || (e.Host != "" && e.Host != HostAPI)) {
			add(at("component"), "GET /health, /ready and /openapi.json are every component's: they name no component or host")
		}
		if e.Protocol != "" && e.Protocol != ProtocolHTTP && e.Protocol != ProtocolGRPC {
			add(at("protocol"), "must be http or grpc")
		}
		if e.GRPC() && !e.OnIngestHost() {
			add(at("protocol"), "gRPC endpoints are on the ingest host")
		}
		switch {
		case e.OnIngestHost() && e.Auth == "service":
			// [being built] An internal producer route (docs/observability/otlp-ingest.md,
			// "Internal producers"): the ingest server's, for other services only.
			issues = append(issues, internalIngestEndpointIssues(e, i)...)
		case e.OnIngestHost() != (e.Auth == AuthIngestKey):
			add(at("host"), "ingest-host endpoints are auth ingestKey, or service under /internal/, and only they are auth ingestKey")
		}
		switch e.Auth {
		case "none":
			switch {
			case e.OnRegistryHost():
				// Only /v2/ and /v2/token (registryEndpointIssues).
			case e.OnPKIHost():
				// pkiEndpointIssues.
			case e.Public == "":
				if e.Method != "GET" || !slices.Contains(Unauthenticated, e.Path) {
					add(at("auth"), "only GET /health, /ready and /openapi.json are unauthenticated")
				}
			case e.Public == PublicCallback:
				switch {
				case e.Method != "POST":
					add(at("method"), "a public callback is a POST")
				case !callbackPath.MatchString(e.Path):
					add(at("path"), "a public callback is "+CallbackPathPrefix+":<param>")
				case (e.Host != "" && e.Host != HostAPI) || e.Component != "":
					add(at("host"), "a public callback is on the API host")
				case e.IdempotencyKey || e.IdempotencyException != "" || e.Async || len(e.Quotas) > 0 || e.ErrorModel != "":
					add(at("public"), "a public callback has no Idempotency-Key, computation, async, quotas or error model")
				}
			case e.Method != "GET":
				add(at("method"), "a public endpoint is a GET")
			case slices.Contains(Unauthenticated, e.Path) || strings.HasPrefix(e.Path, "/internal/"):
				add(at("path"), "a public endpoint isn't one of the kit's own, or under /internal/")
			}
		case "service":
			if !strings.HasPrefix(e.Path, "/internal/") {
				add(at("path"), "service endpoints are under /internal/")
			}
			if e.OnEdgeHost() {
				add(at("host"), "service endpoints are never on the edge host")
			}
		case "caller", "operation":
			switch {
			case e.OnEdgeHost() && e.Auth == "operation":
				add(at("host"), "operation endpoints are on the API host")
			case e.OnEdgeHost():
				// The host's alias names the organization (contracts/registration.md, "Edge endpoints").
				if strings.Contains(e.Path, "/:orgId") || strings.HasPrefix(e.Path, "/internal/") {
					add(at("path"), "an edge-host endpoint's organization is its host's: no :orgId, and never /internal/")
				}
			case !strings.HasPrefix(e.Path, "/v1/organizations/:orgId"):
				add(at("path"), "public endpoints are under /v1/organizations/:orgId")
			}
		case AuthIngestKey:
			issues = append(issues, ingestEndpointIssues(e, i)...)
		case AuthRegistry:
			if !e.OnRegistryHost() {
				add(at("auth"), "auth registry is only for registry-host endpoints")
			}
		case AuthGit:
			if !e.OnGitHost() {
				add(at("auth"), "auth git is only for Git-host endpoints")
			}
		case AuthInvocationToken:
			// invocationEndpointIssues.
		case AuthReceiptHandle:
			// receiptEndpointIssues.
		case AuthProductKey:
			// dataEndpointIssues.
		case AuthDelegated:
			// [being built] /internal/ on the API host, the api component's, with an action some
			// delegation entry covers; a write always has a catalog entry.
			if !strings.HasPrefix(e.Path, "/internal/") {
				add(at("path"), "delegated endpoints are under /internal/")
			}
			// The organization is the caller reference's, which org-api vouched for: a path's
			// :orgId would name one it didn't.
			if strings.Contains(e.Path, "/:orgId") {
				add(at("path"), "a delegated endpoint's organization is its caller reference's: no :orgId")
			}
			if (e.Host != "" && e.Host != HostAPI) || (e.Component != "" && e.Component != ComponentAPI) {
				add(at("host"), "delegated endpoints are on the API host, the api component's")
			}
			if _, declared := actions[e.Action]; declared && !m.Delegation.covers(e.Action) {
				add(at("action"), "no delegation entry covers %s", e.Action)
			}
			if e.Trail == nil && e.Method != "GET" {
				add(at("trail"), "a delegated write is always recorded")
			}
		default:
			add(at("auth"), "must be none, caller, operation, service, ingestKey, registry, git, invocationToken, receiptHandle, productKey or delegated")
		}
		if strings.HasPrefix(e.Path, InvocationPathPrefix) && !e.InvocationToken() {
			add(at("path"), "%s is for invocation-token endpoints", InvocationPathPrefix)
		}
		if strings.HasPrefix(e.Path, ReceiptPathPrefix) && !e.ReceiptHandle() {
			add(at("path"), "%s is for receipt-handle endpoints", ReceiptPathPrefix)
		}
		issues = append(issues, errorModelIssues(e, i)...)
		if e.Auth == "operation" && !operationPath.MatchString(e.Path) {
			add(at("path"), "operation endpoints are /v1/organizations/:orgId/operations/:operationId")
		}
		if (e.Auth == "caller" || e.Auth == AuthIngestKey || e.Auth == AuthRegistry || e.Auth == AuthGit || e.Auth == AuthDelegated) != (e.Action != "") {
			add(at("action"), "caller, ingestKey, registry, git and delegated endpoints name their action, and only they do")
		}
		if a, declared := actions[e.Action]; e.Action != "" && !declared {
			add(at("action"), "%s isn't a declared action", e.Action)
		} else if declared && a.Ingest != (e.Auth == AuthIngestKey) {
			add(at("action"), "ingest actions are for ingest endpoints, and only they are")
		}
		// A computation POST of a read-level action (read or list) keeps nothing and acts on
		// nothing, so it's a read: like a GET, a product may choose not to record it (obs's content
		// reads, contracts/trails.md "The catalog"). A computation of a write-level action acts for
		// the caller (MCP Central's CallTool), so it's always recorded, as is every other write.
		if e.Trail == nil && e.Auth == "caller" && e.Method != "GET" && !(e.Computation() && readLevel(actions, e.Action)) {
			add(at("trail"), "a public write is always recorded: only a computation of a read or list action may be unrecorded")
		}
		if e.Trail == nil && e.Auth == "operation" && e.Method != "GET" {
			add(at("trail"), "cancelling an operation is recorded")
		}
		if e.Trail != nil && (e.Auth == "none" || e.Auth == "service") {
			add(at("trail"), "health checks, public endpoints and service-to-service calls are never recorded")
		}
		if e.Trail != nil && e.Auth == AuthIngestKey {
			add(at("trail"), "ingest calls aren't recorded (the data-plane ingest exception)")
		}
		if e.Trail != nil && e.Auth == AuthProductKey {
			add(at("trail"), "data-host calls aren't recorded (the data-plane exception)")
		}
		if e.Trail != nil {
			issues = append(issues, rpcTrailIssues(e, i)...)
			for _, entry := range trailEntries(e.Trail) {
				where := at("trail" + entry.path)
				if !eventNamePattern.MatchString(entry.trail.EventName) {
					add(where+".eventName", "must be PascalCase")
				}
				if c := entry.trail.EventCategory; c != "" && c != CategoryManagement && c != CategoryData {
					add(where+".eventCategory", "must be Management or Data")
				}
				switch r := entry.trail.Record; {
				case r != "" && r != RecordRefusals:
					add(where+".record", "must be refusals, or omitted")
				case r == RecordRefusals && e.Auth != "caller" && !e.OnGitHost():
					// A write, or a computation POST (a declared high-volume data-plane call:
					// EZGH Inference's CreateEmbeddings, or a live-host long poll: Workflows'
					// PollTasks), or [being built] a caller GET that is a Data event, or a Git-host
					// read (the Git content-read exception: EZGH Git's GetContents,
					// FetchRepository, DownloadLfsObject; gitEndpointIssues limits it to the reads).
					add(where+".record", "only a caller POST, PUT, PATCH or DELETE, a caller GET that is a Data event, or a Git-host read may record refusals only")
				case r == RecordRefusals && e.Method == "GET" && entry.trail.EventCategory != CategoryData:
					add(where+".record", "a GET records refusals only as a Data event (the Git content-read exception)")
				case r == RecordRefusals && e.Trail.RPC():
					add(where+".record", "a JSON-RPC endpoint records per method, never refusals only")
				case r == RecordRefusals && e.Method != "GET" && !e.OnGitHost() && entry.trail.EventCategory == CategoryData:
					add(where+".record", "a refusals-only write or computation is a Management event")
				}
				name := entry.trail.EventName
				if prior, exists := eventNames[name]; exists && !sharesEventName(m.Endpoints[prior], e, prior == i) {
					add(where+".eventName", "%s is used twice", name)
				}
				if _, exists := eventNames[name]; !exists {
					eventNames[name] = i
				}
			}
		}
		if e.Async && e.Method == "GET" {
			add(at("async"), "reads are never asynchronous")
		}
		if e.IdempotencyException != "" && (e.IdempotencyException != "computation" || e.Method != "POST" || e.Auth != "caller" || e.Async || e.IdempotencyKey) {
			add(at("idempotencyException"), "computation requires a synchronous caller POST without persisted idempotency")
		}
		if e.Method == "POST" && e.Auth == "caller" && !e.IdempotencyKey && e.IdempotencyException != "computation" {
			add(at("idempotencyKey"), "every public POST honors Idempotency-Key")
		}
		issues = append(issues, endpointQuotaIssues(e, i, quotas, typeNames)...)
	}
	// Every product serves its OpenAPI document (contracts/api-conventions.md, "OpenAPI").
	if !servesOpenAPI {
		add("endpoints", "must include GET /openapi.json (auth none, trail null)")
	}
	// [being built] A receiver of delegated calls declares who may call (delegation), the
	// credentials it issues them (serviceCredentials) and org-api's delegated check.
	if slices.ContainsFunc(m.Endpoints, Endpoint.Delegated) {
		if m.Delegation == nil {
			add("delegation", "is required: the manifest has delegated endpoints")
		}
		if m.ServiceCredentials == nil {
			add("serviceCredentials", "is required: delegated endpoints take a credential the service issued")
		}
		if m.OrgAPI == nil || !slices.Contains(m.OrgAPI.Operations, "permissions.delegated") {
			add("orgApi.operations", "must include permissions.delegated: the manifest has delegated endpoints")
		}
	}
	issues = append(issues, m.projectDeletionCheckIssues()...)
	issues = append(issues, QuotaIssues(ns, m.Quotas, typeNames)...)
	if m.Metering != nil {
		for i, meter := range m.Metering.Meters {
			if !strings.HasPrefix(meter, ns+".") {
				add(fmt.Sprintf("metering.meters.%d", i), "must start with %s.", ns)
			}
		}
	}
	return issues
}

// ingestHostIssues checks the manifest's ingest host: present exactly when an endpoint is on it,
// <namespace>-ingest.ezghcloud.com, its own ports, and a gRPC port exactly when it serves gRPC.
func (m *Manifest) ingestHostIssues() []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	hasIngest := slices.ContainsFunc(m.Endpoints, Endpoint.OnIngestHost)
	hasGRPC := slices.ContainsFunc(m.Endpoints, Endpoint.GRPC)
	h := m.Ingest
	switch {
	case h == nil && hasIngest:
		add("ingest", "is required: the manifest has ingest-host endpoints")
	case h != nil && !hasIngest:
		add("ingest", "only a manifest with ingest-host endpoints declares an ingest host")
	case h != nil:
		if h.Hostname != m.Namespace+"-ingest.ezghcloud.com" {
			add("ingest.hostname", "must be %s-ingest.ezghcloud.com", m.Namespace)
		}
		if h.Port < 1024 || h.Port > 65535 || h.Port == m.API.Port {
			add("ingest.port", "must be 1024-65535, not api.port")
		}
		if hasGRPC != (h.GRPCPort != 0) {
			add("ingest.grpcPort", "is set exactly when the manifest has gRPC endpoints")
		}
		if h.GRPCPort != 0 && (h.GRPCPort < 1024 || h.GRPCPort > 65535 || h.GRPCPort == m.API.Port || h.GRPCPort == h.Port) {
			add("ingest.grpcPort", "must be 1024-65535, not api.port or ingest.port")
		}
	}
	// Remote write is the metrics signal's other door: it names POST /v1/metrics's action.
	for i, e := range m.Endpoints {
		if !e.OnIngestHost() || e.GRPC() || e.Path != RemoteWritePath {
			continue
		}
		metrics, ok := m.Endpoint("POST", "/v1/metrics")
		if !ok || !metrics.OnIngestHost() || metrics.GRPC() || metrics.Action != e.Action {
			add(fmt.Sprintf("endpoints.%d.action", i), "Prometheus remote write is the metrics signal's: it names POST /v1/metrics's action")
		}
		break
	}
	return issues
}

// gitHostIssues checks the manifest's Git host: present exactly when an endpoint is on it, exactly
// <namespace>.ezghcloud.com (never beside a registry or data host, which take the same name), and a
// port of its own.
func (m *Manifest) gitHostIssues() []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	hasGit := slices.ContainsFunc(m.Endpoints, Endpoint.OnGitHost)
	h := m.Git
	switch {
	case h == nil && hasGit:
		add("git", "is required: the manifest has Git-host endpoints")
	case h != nil && !hasGit:
		add("git", "only a manifest with Git-host endpoints declares a Git host")
	case h != nil:
		if want := m.Namespace + ".ezghcloud.com"; h.Domain != want {
			add("git.domain", "must be %s: a product's Git host is its own namespace's", want)
		}
		if m.Registry != nil || m.Data != nil {
			add("git", "a registry or data host already takes %s.ezghcloud.com", m.Namespace)
		}
		taken := []int{m.API.Port}
		if m.Ingest != nil {
			taken = append(taken, m.Ingest.Port, m.Ingest.GRPCPort)
		}
		if m.Edge != nil {
			taken = append(taken, m.Edge.Port)
		}
		if m.Live != nil {
			taken = append(taken, m.Live.Port)
		}
		if m.Registry != nil {
			taken = append(taken, m.Registry.Port)
		}
		if m.Data != nil {
			taken = append(taken, m.Data.Port)
		}
		if m.PKI != nil {
			taken = append(taken, m.PKI.Port)
		}
		if m.S3 != nil {
			taken = append(taken, m.S3.Port)
		}
		if h.Port < 1024 || h.Port > 65535 || slices.Contains(taken, h.Port) {
			add("git.port", "must be 1024-65535, not api.port or another host's port")
		}
	}
	return issues
}

// gitEndpointIssues are a Git-host endpoint's own rules (contracts/registration.md, "Git hosts"):
// a Git or Git LFS path and one of its methods, auth git, its trail a Data event (refusals only for
// the reads, and a push always recorded), and none of the platform's API-host features.
func gitEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	route := GitRoute{e.Method, e.Path}
	methods, known := gitMethods[e.Path]
	switch {
	case !known:
		add("path", "a Git path is one of "+strings.Join(GitPaths, ", "))
	case !slices.Contains(methods, e.Method):
		add("method", e.Path+" takes "+strings.Join(methods, ", "))
	}
	if e.Auth != AuthGit {
		add("auth", "Git-host endpoints are auth git")
	}
	if e.Trail != nil && e.Trail.EventCategory != CategoryData {
		add("trail.eventCategory", "Git-host calls are Data events")
	}
	if e.Trail != nil && e.Trail.Record == RecordRefusals && !slices.Contains(GitReads, route) {
		add("trail.record", "only the Git host's reads record refusals only: a push or an LFS upload is always recorded")
	}
	if slices.Contains(gitRecorded, route) && e.Trail == nil {
		add("trail", "the ref advertisement, git-upload-pack and git-receive-pack are always recorded (a read may record refusals only)")
	}
	if e.Component != "" {
		add("component", "Git-host endpoints are the Git server's: they name no component")
	}
	if e.IdempotencyKey {
		add("idempotencyKey", "Git-host endpoints take no Idempotency-Key: a ref update is a compare-and-swap, an LFS object is content-addressed")
	}
	if e.IdempotencyException != "" {
		add("idempotencyException", "Git-host endpoints are no computations")
	}
	if e.Async {
		add("async", "Git-host endpoints are never asynchronous")
	}
	if len(e.Quotas) > 0 {
		add("quotas", "Git-host endpoints consume no quotas: their limits are the product's own")
	}
	if e.TimeoutSeconds != 0 {
		add("timeoutSeconds", "Git-host endpoints keep the Git server's timeouts")
	}
	if e.Public != "" {
		add("public", "Git-host endpoints aren't public endpoints")
	}
	if e.Credentials != "" {
		add("credentials", "the Git server takes its own credentials (an API key, as Basic or bearer)")
	}
	if e.ErrorModel != "" {
		add("errorModel", "Git-host endpoints answer as Git and Git LFS clients expect")
	}
	return issues
}

// apiHostnames are the API hosts a manifest may name: <namespace>.ezghcloud.com, or
// <namespace>-api.ezghcloud.com, which any product may choose (workflows-api.ezghcloud.com) and
// a product with a registry or data host must use, since the registry domain and the data host are always
// <namespace>.ezghcloud.com (registry-stack: registry-api.ezghcloud.com beside
// *.registry.ezghcloud.com).
func (m *Manifest) apiHostnames() []string {
	if m.Registry != nil || m.Data != nil || m.Git != nil {
		return []string{m.Namespace + "-api.ezghcloud.com"}
	}
	return []string{m.Namespace + ".ezghcloud.com", m.Namespace + "-api.ezghcloud.com"}
}

// registryHostIssues checks the manifest's registry host: present exactly when an endpoint is on
// it, exactly <namespace>.ezghcloud.com, and a port of its own.
func (m *Manifest) registryHostIssues() []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	hasRegistry := slices.ContainsFunc(m.Endpoints, Endpoint.OnRegistryHost)
	h := m.Registry
	switch {
	case h == nil && hasRegistry:
		add("registry", "is required: the manifest has registry-host endpoints")
	case h != nil && !hasRegistry:
		add("registry", "only a manifest with registry-host endpoints declares a registry host")
	case h != nil:
		if want := m.Namespace + ".ezghcloud.com"; h.Domain != want {
			add("registry.domain", "must be %s: a product's registry host is its own namespace's", want)
		}
		taken := []int{m.API.Port}
		if m.Ingest != nil {
			taken = append(taken, m.Ingest.Port, m.Ingest.GRPCPort)
		}
		if m.Edge != nil {
			taken = append(taken, m.Edge.Port)
		}
		if m.Live != nil {
			taken = append(taken, m.Live.Port)
		}
		if h.Port < 1024 || h.Port > 65535 || slices.Contains(taken, h.Port) {
			add("registry.port", "must be 1024-65535, not api.port or another host's port")
		}
	}
	return issues
}

// crossHostRouteIssues refuses a data-host, ingest-host or PKI-host endpoint whose method and path are
// another host's endpoint's too: each host has its own server, but Service.Handler keys every
// manifest endpoint by method and path, so it would refuse to start. (Registry paths use <…>
// placeholders no other host's path can have, so they never collide.)
func (m *Manifest) crossHostRouteIssues() []Issue {
	var issues []Issue
	for i, e := range m.Endpoints {
		if !e.OnDataHost() && !e.OnIngestHost() && !e.OnPKIHost() {
			continue
		}
		for j, other := range m.Endpoints {
			if j != i && other.Host != e.Host && other.Method == e.Method && other.Path == e.Path {
				issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.path", i), Message: fmt.Sprintf("%s %s is endpoints.%d's too, on another host: the kit's API server keys every endpoint by method and path", e.Method, e.Path, j)})
				break
			}
		}
	}
	return issues
}

// dataHostIssues checks the manifest's data host: present exactly when an endpoint is on it,
// exactly <namespace>.ezghcloud.com (never beside a registry host, which takes the same name), and
// a port of its own.
func (m *Manifest) dataHostIssues() []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	hasData := slices.ContainsFunc(m.Endpoints, Endpoint.OnDataHost)
	h := m.Data
	switch {
	case h == nil && hasData:
		add("data", "is required: the manifest has data-host endpoints")
	case h != nil && !hasData:
		add("data", "only a manifest with data-host endpoints declares a data host")
	case h != nil:
		if want := m.Namespace + ".ezghcloud.com"; h.Hostname != want {
			add("data.hostname", "must be %s: a product's data host is its own namespace's", want)
		}
		if m.Registry != nil {
			add("data", "a registry host already takes %s.ezghcloud.com", m.Namespace)
		}
		taken := []int{m.API.Port}
		if m.Ingest != nil {
			taken = append(taken, m.Ingest.Port, m.Ingest.GRPCPort)
		}
		if m.Edge != nil {
			taken = append(taken, m.Edge.Port)
		}
		if m.Live != nil {
			taken = append(taken, m.Live.Port)
		}
		if m.Registry != nil {
			taken = append(taken, m.Registry.Port)
		}
		if h.Port < 1024 || h.Port > 65535 || slices.Contains(taken, h.Port) {
			add("data.port", "must be 1024-65535, not api.port or another host's port")
		}
	}
	return issues
}

// s3HostIssues checks the manifest's S3 host: only namespace objects', s3.<region>.ezghapps.com, and
// a port of its own.
func (m *Manifest) s3HostIssues() []Issue {
	h := m.S3
	if h == nil {
		return nil
	}
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	if m.Namespace != S3Namespace {
		add("s3", "the S3 host is reserved to namespace %s", S3Namespace)
	}
	if !s3Domain.MatchString(h.Domain) {
		add("s3.domain", "must be s3.<region>.ezghapps.com")
	}
	taken := []int{m.API.Port}
	if m.Ingest != nil {
		taken = append(taken, m.Ingest.Port, m.Ingest.GRPCPort)
	}
	if m.Edge != nil {
		taken = append(taken, m.Edge.Port)
	}
	if m.Live != nil {
		taken = append(taken, m.Live.Port)
	}
	if m.Registry != nil {
		taken = append(taken, m.Registry.Port)
	}
	if m.Data != nil {
		taken = append(taken, m.Data.Port)
	}
	if m.PKI != nil {
		taken = append(taken, m.PKI.Port)
	}
	if h.Port < 1024 || h.Port > 65535 || slices.Contains(taken, h.Port) {
		add("s3.port", "must be 1024-65535, not api.port or another host's port")
	}
	return issues
}

// pkiHostIssues checks the manifest's PKI host: present exactly when an endpoint is on it, only
// namespace certificates', exactly pki.ezghcloud.com, and a port of its own.
func (m *Manifest) pkiHostIssues() []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	hasPKI := slices.ContainsFunc(m.Endpoints, Endpoint.OnPKIHost)
	h := m.PKI
	switch {
	case h == nil && hasPKI:
		add("pki", "is required: the manifest has PKI-host endpoints")
	case h != nil && !hasPKI:
		add("pki", "only a manifest with PKI-host endpoints declares the PKI host")
	case h != nil:
		if m.Namespace != PKINamespace {
			add("pki", "the PKI host is reserved to namespace %s", PKINamespace)
		}
		if h.Hostname != PKIHostname {
			add("pki.hostname", "must be %s", PKIHostname)
		}
		taken := []int{m.API.Port}
		if m.Ingest != nil {
			taken = append(taken, m.Ingest.Port, m.Ingest.GRPCPort)
		}
		if m.Edge != nil {
			taken = append(taken, m.Edge.Port)
		}
		if m.Live != nil {
			taken = append(taken, m.Live.Port)
		}
		if m.Registry != nil {
			taken = append(taken, m.Registry.Port)
		}
		if m.Data != nil {
			taken = append(taken, m.Data.Port)
		}
		if h.Port < 1024 || h.Port > 65535 || slices.Contains(taken, h.Port) {
			add("pki.port", "must be 1024-65535, not api.port or another host's port")
		}
	}
	return issues
}

// pkiEndpointIssues are a PKI-host endpoint's own rules (contracts/registration.md, "PKI
// endpoints"): an unauthenticated GET, HEAD or POST under /acme/, /ca/, /crl/ or /ocsp/, never
// recorded by the kit, and none of the platform's API-host features. (Its protocol and
// timeoutSeconds are checked with the others'.)
func pkiEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if e.Method != "GET" && e.Method != "HEAD" && e.Method != "POST" {
		add("method", "PKI-host endpoints are GET, HEAD or POST")
	}
	switch {
	case strings.HasPrefix(e.Path, "/v1/organizations/") || e.Path == "/v1/organizations":
		add("path", "PKI-host endpoints are never under /v1/organizations/")
	case strings.HasPrefix(e.Path, "/internal/"):
		add("path", "PKI-host endpoints are never under /internal/")
	case slices.Contains(Unauthenticated, e.Path):
		add("path", "/health, /ready and /openapi.json are the kit's own, not PKI-host endpoints")
	case PKIPart(e.Path) == "":
		add("path", "a PKI-host path is under /acme/, /ca/, /crl/ or /ocsp/")
	}
	if e.Auth != "none" {
		add("auth", "PKI-host endpoints are auth none: relying parties are anonymous, and ACME authenticates with its own account keys")
	}
	if e.Trail != nil {
		add("trail", "PKI-host endpoints have trail null: the ACME server records issuance and revocation itself")
	}
	if e.Action != "" {
		add("action", "a PKI-host endpoint names no action")
	}
	if e.IdempotencyKey {
		add("idempotencyKey", "PKI-host endpoints take no Idempotency-Key")
	}
	if e.IdempotencyException != "" {
		add("idempotencyException", "a PKI-host endpoint is no computation: it takes no Idempotency-Key at all")
	}
	if e.Async {
		add("async", "PKI-host endpoints are never asynchronous")
	}
	if len(e.Quotas) > 0 {
		add("quotas", "PKI-host endpoints consume no quotas: their limits are the product's own")
	}
	if e.Component != "" {
		add("component", "PKI-host endpoints are the product's PKI servers': they name no component")
	}
	if e.Credentials != "" {
		add("credentials", "a PKI-host endpoint takes no platform credential")
	}
	if e.Public != "" {
		add("public", "PKI-host endpoints aren't public endpoints")
	}
	if e.ErrorModel != "" {
		add("errorModel", "PKI-host endpoints answer their protocols' errors (RFC 8555, RFC 6960)")
	}
	if e.BlockedBySpendControls {
		add("blockedBySpendControls", "spend controls apply to the API's creates, not the PKI host")
	}
	return issues
}

// dataEndpointIssues are a data-host endpoint's own rules (contracts/registration.md, "Data
// endpoints"): a GET or POST outside the organization path, /internal/ and the kit's own paths,
// never recorded, and none of the platform's API-host features. (Its auth, action, protocol and
// timeoutSeconds are checked with the others'.)
func dataEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if e.Method != "GET" && e.Method != "POST" {
		add("method", "data-host endpoints are GET or POST")
	}
	switch {
	case strings.HasPrefix(e.Path, "/v1/organizations/") || e.Path == "/v1/organizations":
		add("path", "a data-host endpoint's key names the organization: never under /v1/organizations/")
	case strings.HasPrefix(e.Path, "/internal/"):
		add("path", "data-host endpoints are never under /internal/")
	case slices.Contains(Unauthenticated, e.Path):
		add("path", "/health, /ready and /openapi.json are the kit's own, not data-host endpoints")
	}
	if e.Trail != nil {
		add("trail", "data-host calls aren't recorded (the data-plane exception): trail null")
	}
	if e.Action != "" {
		add("action", "a data-host endpoint names no action: the product's key authorizes it")
	}
	if e.IdempotencyKey {
		add("idempotencyKey", "data-host endpoints take no Idempotency-Key")
	}
	if e.IdempotencyException != "" {
		add("idempotencyException", "a data-host endpoint is no computation: it takes no Idempotency-Key at all")
	}
	if e.Async {
		add("async", "data-host endpoints are never asynchronous")
	}
	if len(e.Quotas) > 0 {
		add("quotas", "data-host endpoints consume no quotas: their limits are the product's own")
	}
	if e.Component != "" {
		add("component", "data-host endpoints are the data server's: they name no component")
	}
	if e.Credentials != "" {
		add("credentials", "a data-host endpoint takes only the product's data-plane key")
	}
	if e.Public != "" {
		add("public", "data-host endpoints aren't public endpoints")
	}
	if e.ErrorModel != "" {
		add("errorModel", "data-host endpoints answer the platform's errors, or their public protocol's (OFREP)")
	}
	return issues
}

// registryEndpointIssues are a registry-host endpoint's own rules (contracts/registration.md,
// "Registry endpoints"): an OCI path and one of its methods, auth registry (or none for /v2/ and
// /v2/token), and none of the platform's API-host features. (Its action and trail are checked
// with the others'.)
func registryEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	methods, known := registryMethods[e.Path]
	switch {
	case !known:
		add("path", "a registry path is one of "+strings.Join(RegistryPaths, ", "))
	case !slices.Contains(methods, e.Method):
		add("method", e.Path+" takes "+strings.Join(methods, ", "))
	}
	switch {
	case e.Auth != AuthRegistry && e.Auth != "none":
		add("auth", "registry-host endpoints are auth registry, or none for /v2/ and /v2/token")
	case e.Auth == "none" && known && !slices.Contains(registryUnauthenticated, e.Path):
		add("auth", "only /v2/ and /v2/token are unauthenticated on the registry host")
	}
	if e.Component != "" {
		add("component", "registry-host endpoints are the registry server's: they name no component")
	}
	if e.IdempotencyKey {
		add("idempotencyKey", "OCI writes are idempotent by digest: no Idempotency-Key")
	}
	if e.Async {
		add("async", "registry endpoints are never asynchronous")
	}
	if len(e.Quotas) > 0 {
		add("quotas", "registry endpoints consume no quotas: their limits are the product's own")
	}
	if e.TimeoutSeconds != 0 {
		add("timeoutSeconds", "registry endpoints keep the registry server's timeouts")
	}
	if e.Public != "" {
		add("public", "registry endpoints aren't public endpoints")
	}
	if e.Credentials != "" {
		add("credentials", "the registry server takes its own credentials (an API key, or none)")
	}
	return issues
}

// errorModelIssues are an endpoint's error-model rules (contracts/api-conventions.md, "Prometheus
// query surface" and "OpenAI-compatible surface"): caller endpoints on the API host. Prometheus:
// a GET or a computation POST, under a Prometheus API base inside the organization. OpenAI: a GET
// or a POST under the project's OpenAI base, taking bearer credentials only.
func errorModelIssues(e Endpoint, i int) []Issue {
	if e.ErrorModel == "" {
		return nil
	}
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if e.ErrorModel != ErrorModelPrometheus && e.ErrorModel != ErrorModelOpenAI {
		add("errorModel", "must be prometheus or openai")
		return issues
	}
	if e.Auth != "caller" || (e.Host != "" && e.Host != HostAPI) {
		add("errorModel", "only caller endpoints on the API host use another error model")
	}
	if e.ErrorModel == ErrorModelOpenAI {
		if e.Method != "GET" && e.Method != "POST" {
			add("errorModel", "the OpenAI error model is for GET and POST")
		}
		if !openAIPath.MatchString(e.Path) {
			add("path", "an OpenAI endpoint is under /v1/organizations/:orgId/projects/:projectId/openai/v1/")
		}
		if e.Credentials != CredentialsBearer {
			add("credentials", "an OpenAI endpoint takes bearer credentials only")
		}
		return issues
	}
	if e.Method != "GET" && !(e.Method == "POST" && e.Computation() && !e.Trail.RPC()) {
		add("errorModel", "the Prometheus error model is for reads: a GET or a computation POST")
	}
	if !prometheusPath.MatchString(e.Path) {
		add("path", "a Prometheus endpoint is under /v1/organizations/:orgId/…/prometheus/api/v1/")
	}
	return issues
}

type pathedTrail struct {
	path  string
	trail *Trail
}

// trailEntries are a trail's entries with their paths under "trail": itself (""), or each
// recorded method's (".byRpcMethod.<method>", sorted) and then ".otherwise".
func trailEntries(t *Trail) []pathedTrail {
	if !t.RPC() {
		return []pathedTrail{{"", t}}
	}
	var out []pathedTrail
	for _, method := range slices.Sorted(maps.Keys(t.ByRPCMethod)) {
		if entry := t.ByRPCMethod[method]; entry != nil {
			out = append(out, pathedTrail{".byRpcMethod." + method, entry})
		}
	}
	if t.Otherwise != nil {
		out = append(out, pathedTrail{".otherwise", t.Otherwise})
	}
	return out
}

// sharesEventName reports whether two endpoints may record the same event name: GET aliases of
// one action, [being built] a GET and a computation POST of the same path and action (one read
// offered both ways, as the Prometheus HTTP API's GET and form POST), or two JSON-RPC endpoints of
// one action (two MCP endpoints of one product could both record CallTool). Never within one
// endpoint.
// [being built] A delegated endpoint may share its event name with another endpoint of the same
// action: it stands in for the public call (images' ResolveImage, blockstore's CreateVolume).
func sharesEventName(prior, e Endpoint, same bool) bool {
	if same || e.Action == "" || prior.Action != e.Action {
		return false
	}
	// [being built] Two Git-host endpoints of one action may share one too: a fetch is a ref
	// advertisement and an upload-pack POST (FetchRepository), an LFS download a batch and a GET.
	return (prior.Method == "GET" && e.Method == "GET") || ReadPair(prior, e) || (prior.Trail.RPC() && e.Trail.RPC()) || prior.Delegated() || e.Delegated() ||
		(prior.OnGitHost() && e.OnGitHost())
}

// ReadPair reports whether a and b are one read offered both ways: a GET and a computation POST
// (not per JSON-RPC method) of the same path. They may share an event name.
func ReadPair(a, b Endpoint) bool {
	post := func(e Endpoint) bool { return e.Method == "POST" && e.Computation() && !e.Trail.RPC() }
	return a.Path == b.Path && ((a.Method == "GET" && post(b)) || (b.Method == "GET" && post(a)))
}

var rpcMethodPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*([./][A-Za-z][A-Za-z0-9_]*)*$`)

// rpcTrailIssues are a JSON-RPC trail's own rules: on a caller POST, method names, and at least one
// entry that records something.
func rpcTrailIssues(e Endpoint, i int) []Issue {
	if !e.Trail.RPC() {
		return nil
	}
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if e.Method != "POST" || e.Auth != "caller" {
		add("trail.byRpcMethod", "a trail per JSON-RPC method is for a caller POST")
	}
	for _, method := range slices.Sorted(maps.Keys(e.Trail.ByRPCMethod)) {
		if len(method) > 64 || !rpcMethodPattern.MatchString(method) {
			add("trail.byRpcMethod."+method, "must be a JSON-RPC method name, such as tools/call")
		}
	}
	if len(e.Trail.ByRPCMethod) == 0 {
		add("trail.byRpcMethod", "lists at least one JSON-RPC method")
	} else if len(e.Trail.Entries()) == 0 {
		add("trail.byRpcMethod", "records at least one method (a public write is always recorded)")
	}
	return issues
}

// edgeHostIssues checks the manifest's edge host: present exactly when an endpoint is on it, a
// domain of its own under ezgh.app, and the edge component's port exactly when an endpoint names
// that component.
func (m *Manifest) edgeHostIssues() []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	hasEdge := slices.ContainsFunc(m.Endpoints, Endpoint.OnEdgeHost)
	hasComponent := slices.ContainsFunc(m.Endpoints, func(e Endpoint) bool { return e.Component == ComponentEdge })
	h := m.Edge
	switch {
	case h == nil && hasEdge:
		add("edge", "is required: the manifest has edge-host endpoints")
	case h != nil && !hasEdge:
		add("edge", "only a manifest with edge-host endpoints declares an edge host")
	case h != nil:
		if !edgeDomainPattern.MatchString(h.Domain) || slices.Contains(ReservedEdgeDomains, h.Domain) {
			add("edge.domain", "must be <name>.ezgh.app, and not the platform's own (sso.ezgh.app)")
		}
		hasCookie := slices.ContainsFunc(m.Endpoints, func(e Endpoint) bool { return e.Credentials == CredentialsCookie })
		switch {
		case hasCookie != (h.Cookie != nil):
			add("edge.cookie", "is declared exactly when an endpoint takes it (credentials cookie)")
		case h.Cookie != nil:
			if !edgeCookiePattern.MatchString(h.Cookie.Name) {
				add("edge.cookie.name", "must be a host-only cookie: __Host- and 1-64 letters, digits, _ or -")
			}
			if !edgeAudiencePattern.MatchString(h.Cookie.Audience) {
				add("edge.cookie.audience", "must be one lowercase path segment, such as portal")
			}
		}
		if hasComponent != (h.Port != 0) {
			add("edge.port", "is set exactly when an endpoint names component edge")
		}
		if h.Port != 0 {
			taken := []int{m.API.Port}
			if m.Ingest != nil {
				taken = append(taken, m.Ingest.Port, m.Ingest.GRPCPort)
			}
			if h.Port < 1024 || h.Port > 65535 || slices.Contains(taken, h.Port) {
				add("edge.port", "must be 1024-65535, not api.port or an ingest port")
			}
		}
	}
	return issues
}

// liveHostIssues checks the manifest's live host: present exactly when an endpoint is on it,
// <namespace>-live.ezghcloud.com, and the live component's own port.
func (m *Manifest) liveHostIssues() []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	hasLive := slices.ContainsFunc(m.Endpoints, Endpoint.OnLiveHost)
	h := m.Live
	switch {
	case h == nil && hasLive:
		add("live", "is required: the manifest has live-host endpoints")
	case h != nil && !hasLive:
		add("live", "only a manifest with live-host endpoints declares a live host")
	case h != nil:
		if h.Hostname != m.Namespace+"-live.ezghcloud.com" {
			add("live.hostname", "must be %s-live.ezghcloud.com", m.Namespace)
		}
		taken := []int{m.API.Port}
		if m.Ingest != nil {
			taken = append(taken, m.Ingest.Port, m.Ingest.GRPCPort)
		}
		if m.Edge != nil {
			taken = append(taken, m.Edge.Port)
		}
		if h.Port < 1024 || h.Port > 65535 || slices.Contains(taken, h.Port) {
			add("live.port", "must be 1024-65535, not api.port or another host's port")
		}
	}
	return issues
}

// liveEndpointIssues are a live-host endpoint's own rules (contracts/registration.md, "Live
// endpoints"): a caller POST stream, the live component's, a computation with its own timeout,
// recorded once when it opens, not at all (trail null: a read the product chooses not to record,
// obs's live tail), or [being built] only when refused (record: "refusals", checked
// with the other trail rules: a long poll such as Workflows' PollTasks, whose successes come at
// data-plane rates). (Its path, action and timeout's range are checked with the others'.)
func liveEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if e.Component != ComponentLive {
		add("component", "live-host endpoints are the live component's")
	}
	if e.Method != "POST" {
		add("method", "a live-host endpoint (a stream) is a POST")
	}
	if e.Auth != "caller" {
		add("auth", "live-host endpoints are auth caller")
	}
	if !strings.HasPrefix(e.Path, "/v1/organizations/:orgId/") {
		add("path", "a live-host endpoint is under /v1/organizations/:orgId/")
	}
	if e.IdempotencyException != "computation" {
		add("idempotencyException", "a stream is a computation: idempotencyException computation")
	}
	if e.TimeoutSeconds == 0 {
		add("timeoutSeconds", "a live-host endpoint declares its timeoutSeconds")
	}
	// A null trail (a stream of a read-level action) is checked with the other caller writes'.
	if e.Trail.RPC() {
		add("trail", "a stream is recorded once, when it opens (a trail entry with an eventName), or not at all (null)")
	}
	return issues
}

// readLevel reports whether id is a declared action of accessLevel read or list.
func readLevel(actions map[string]Action, id string) bool {
	a, ok := actions[id]
	return ok && (a.AccessLevel == "read" || a.AccessLevel == "list")
}

// invocationEndpointIssues are an invocation-token endpoint's rules (contracts/registration.md,
// "Invocation-token endpoints"): the live component's, a GET or POST under /v1/invocations/,
// never recorded, and nothing a caller endpoint has. (Its action is refused with the others'.)
func invocationEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if !e.OnLiveHost() {
		add("host", "invocation-token endpoints are on the live host")
	}
	if e.Component != ComponentLive {
		add("component", "invocation-token endpoints are the live component's")
	}
	if e.Method != "GET" && e.Method != "POST" {
		add("method", "an invocation-token endpoint is a GET or a POST")
	}
	if !strings.HasPrefix(e.Path, InvocationPathPrefix) {
		add("path", "an invocation-token endpoint is under "+InvocationPathPrefix)
	}
	if e.Trail != nil {
		add("trail", "invocation-token calls aren't recorded: the product's own history holds them")
	}
	if e.IdempotencyKey {
		add("idempotencyKey", "an invocation-token endpoint takes no Idempotency-Key: the token names one unit of work")
	}
	if e.IdempotencyException != "" {
		add("idempotencyException", "an invocation-token endpoint is no computation")
	}
	if e.Async {
		add("async", "an invocation-token endpoint is synchronous")
	}
	if len(e.Quotas) > 0 {
		add("quotas", "invocation-token endpoints check no quotas")
	}
	if e.Credentials != "" {
		add("credentials", "an invocation-token endpoint takes only its bearer token")
	}
	if e.Public != "" {
		add("public", "an invocation-token endpoint isn't public")
	}
	if e.ErrorModel != "" {
		add("errorModel", "an invocation-token endpoint answers the platform's errors")
	}
	if e.BlockedBySpendControls {
		add("blockedBySpendControls", "spend controls apply where the work starts, not to its invocations")
	}
	return issues
}

// receiptEndpointIssues are a receipt-handle endpoint's rules (contracts/registration.md,
// "Receipt-handle endpoints"): the live component's, a POST under /v1/receipts/, never recorded,
// and nothing a caller endpoint has. (Its action is refused with the others'.)
func receiptEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if !e.OnLiveHost() {
		add("host", "receipt-handle endpoints are on the live host")
	}
	if e.Component != ComponentLive {
		add("component", "receipt-handle endpoints are the live component's")
	}
	if e.Method != "POST" {
		add("method", "a receipt-handle endpoint is a POST")
	}
	if !strings.HasPrefix(e.Path, ReceiptPathPrefix) {
		add("path", "a receipt-handle endpoint is under "+ReceiptPathPrefix)
	}
	if e.Trail != nil {
		add("trail", "receipt-handle calls aren't recorded: the handle is the capability, like an invocation token")
	}
	if e.IdempotencyKey {
		add("idempotencyKey", "a receipt-handle endpoint takes no Idempotency-Key: each handle names one delivery")
	}
	if e.IdempotencyException != "" {
		add("idempotencyException", "a receipt-handle endpoint is no computation")
	}
	if e.Async {
		add("async", "a receipt-handle endpoint is synchronous")
	}
	if len(e.Quotas) > 0 {
		add("quotas", "receipt-handle endpoints check no quotas")
	}
	if e.Credentials != "" {
		add("credentials", "a receipt-handle endpoint takes no credential: the handles in its body are the capability")
	}
	if e.Public != "" {
		add("public", "a receipt-handle endpoint isn't public")
	}
	if e.ErrorModel != "" {
		add("errorModel", "a receipt-handle endpoint answers the platform's errors")
	}
	if e.BlockedBySpendControls {
		add("blockedBySpendControls", "spend controls apply where messages are sent, not to their receipts")
	}
	return issues
}

// ReservedEdgeDomains are ezgh.app domains no product's edge may use: EZGH SSO's.
var ReservedEdgeDomains = []string{"sso.ezgh.app"}

// ingestEndpointIssues are an ingest-host endpoint's own rules: a POST of an OTLP-style path, at
// least once, never asynchronous. (Its trail, action and quotas are checked with the others'.)
func ingestEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if e.Method != "POST" {
		add("method", "ingest endpoints are POST")
	}
	if e.GRPC() && !grpcMethodPath.MatchString(e.Path) {
		add("path", "a gRPC endpoint's path is /<package>.<Service>/<Method>")
	}
	if !e.GRPC() && !ingestHTTPPath.MatchString(e.Path) && e.Path != RemoteWritePath {
		add("path", "an HTTP ingest path is /v1/<signal>, or /api/v1/write (Prometheus remote write)")
	}
	if e.IdempotencyKey {
		add("idempotencyKey", "ingest is at least once: no Idempotency-Key")
	}
	if e.Async {
		add("async", "ingest is never asynchronous")
	}
	return issues
}

// internalIngestEndpointIssues are an internal producer route's own rules: a service endpoint on
// the ingest host is an HTTP POST, at least once, never asynchronous, like the ingest endpoints it
// joins. (Its /internal/ path, trail and action are checked with every service endpoint's.)
func internalIngestEndpointIssues(e Endpoint, i int) []Issue {
	var issues []Issue
	add := func(field, message string) {
		issues = append(issues, Issue{Path: fmt.Sprintf("endpoints.%d.%s", i, field), Message: message})
	}
	if e.Method != "POST" {
		add("method", "an internal producer route is a POST")
	}
	if e.GRPC() {
		add("protocol", "an internal producer route is HTTP")
	}
	if e.IdempotencyKey {
		add("idempotencyKey", "ingest is at least once: no Idempotency-Key")
	}
	if e.Async {
		add("async", "ingest is never asynchronous")
	}
	return issues
}

// MaxQuotaValue is the largest value a quota definition sets (default, tierDefaults, max, burst):
// 10^12 (contracts/schemas/quota-definition.schema.json, $defs.value).
const MaxQuotaValue = 1_000_000_000_000

// QuotaIssues checks a namespace's quota definitions: what the schema says, and the rules it
// can't (scripts/lib/quotas.mjs). quotas-api applies the same ones to a registration, so a
// manifest that passes registers.
func QuotaIssues(ns string, quotas []Quota, typeNames map[string]bool) []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	seen := map[string]bool{}
	for i, q := range quotas {
		at := func(field string) string { return fmt.Sprintf("quotas.%d.%s", i, field) }
		if !quotaIDPattern.MatchString(q.ID) || len(q.ID) > 96 {
			add(at("id"), "must be <namespace>.<name> or <namespace>.<group>.<name>, camelCase, at most 96 characters")
		}
		if !strings.HasPrefix(q.ID, ns+".") {
			add(at("id"), "must start with %s.", ns)
		}
		if seen[q.ID] {
			add(at("id"), "%s is declared twice", q.ID)
		}
		seen[q.ID] = true
		if n := utf8.RuneCountInString(q.DisplayName); n < 1 || n > 100 {
			add(at("displayName"), "%s displayName must be 1-100 characters, not %d", q.ID, n)
		}
		if n := utf8.RuneCountInString(q.Description); n < 1 || n > 500 {
			add(at("description"), "%s description must be 1-500 characters, not %d", q.ID, n)
		}
		if !quotaUnitPattern.MatchString(q.Unit) {
			add(at("unit"), "%s unit %q must be lowercase letters and dashes, singular: widget, api-request", q.ID, q.Unit)
		}
		if !slices.Contains([]string{Allocation, Rate, Usage, Concurrency}, q.Kind) {
			add(at("kind"), "must be allocation, rate, usage or concurrency")
		}
		if !slices.Contains([]string{"organization", "project", "resource"}, q.Scope) {
			add(at("scope"), "must be organization, project or resource")
		}
		if (q.Scope == "resource") != (q.ResourceType != "") {
			add(at("resourceType"), "resource-scoped quotas name their resource type, and only they do")
		}
		if q.ResourceType != "" && !quotaTypePattern.MatchString(q.ResourceType) {
			add(at("resourceType"), "%s resourceType %q must be camelCase", q.ID, q.ResourceType)
		}
		if q.ResourceType != "" && typeNames != nil && !typeNames[q.ResourceType] {
			add(at("resourceType"), "%s isn't a declared resource type", q.ResourceType)
		}
		value := func(field string, v int) {
			if v < 0 || v > MaxQuotaValue {
				add(at(field), "%s %s must be an integer from 0 to 10^12, not %d", q.ID, field, v)
			}
		}
		value("default", q.Default)
		if q.Max != nil {
			value("max", *q.Max)
		}
		if q.Burst != nil {
			value("burst", *q.Burst)
		}
		if q.Kind == Rate {
			if _, ok := DurationMS(q.Window); !ok {
				add(at("window"), "must be PT<n>S, PT<n>M or PT1H, at most an hour")
			}
		} else if q.Window != "" || q.Burst != nil {
			add(at("window"), "only rate quotas have a window and a burst")
		}
		if q.Burst != nil && *q.Burst < q.Default {
			add(at("burst"), "must be at least default")
		}
		if q.LeaseTTL != "" {
			if q.Kind != Concurrency {
				add(at("leaseTtl"), "only concurrency quotas have a lease TTL")
			} else if _, ok := DurationMS(q.LeaseTTL); !ok {
				add(at("leaseTtl"), "must be PT<n>S, PT<n>M or PT1H, at most an hour")
			}
		}
		for _, tier := range slices.Sorted(maps.Keys(q.TierDefaults)) {
			if !tierPattern.MatchString(tier) {
				add(at("tierDefaults."+tier), "%s tierDefaults %q isn't a tier name: 2-32 lowercase letters, digits and dashes, starting with a letter", q.ID, tier)
			}
			value("tierDefaults."+tier, q.TierDefaults[tier])
			if q.Max != nil && q.TierDefaults[tier] > *q.Max {
				add(at("tierDefaults."+tier), "must be at most max")
			}
		}
		if q.Max != nil && q.Default > *q.Max {
			add(at("default"), "must be at most max")
		}
		if q.Status != "" && q.Status != "active" && q.Status != "deprecated" {
			add(at("status"), "must be active or deprecated")
		}
	}
	return issues
}

func endpointQuotaIssues(e Endpoint, i int, quotas map[string]Quota, typeNames map[string]bool) []Issue {
	var issues []Issue
	add := func(path, format string, args ...any) {
		issues = append(issues, Issue{Path: path, Message: fmt.Sprintf(format, args...)})
	}
	seen := map[string]bool{}
	for j, ref := range e.Quotas {
		at := fmt.Sprintf("endpoints.%d.quotas.%d", i, j)
		q, ok := quotas[ref.ID]
		if !ok {
			add(at, "%s isn't a declared quota", ref.ID)
			continue
		}
		if seen[ref.ID] {
			add(at, "%s is listed twice", ref.ID)
		}
		seen[ref.ID] = true
		if e.Auth != "caller" {
			add(at, "only caller endpoints consume quotas: they're checked after authorization")
		}
		if q.Status == "deprecated" {
			add(at, "%s is deprecated", ref.ID)
		}
		if q.Scope == "project" && !strings.Contains(e.Path, "/:projectId") {
			add(at, "%s is per project, and the path has no :projectId", ref.ID)
		}
		if q.Scope == "resource" && typeNames[q.ResourceType] && !strings.Contains(e.Path, "/:"+q.ResourceType+"Id") {
			add(at, "%s is per %s, and the path has no :%sId", ref.ID, q.ResourceType, q.ResourceType)
		}
		if ref.Amount < 1 {
			add(at+".amount", "must be at least 1")
		} else if q.Kind == Concurrency && ref.Amount != 1 {
			add(at+".amount", "a concurrency quota takes one lease per request")
		}
	}
	return issues
}
