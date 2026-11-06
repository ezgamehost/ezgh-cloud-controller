package manifest_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// The contracts' widgets manifest follows every rule, and its registration is exactly
// examples/widgets.registration-request.json.
func TestWidgetsExample(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := canonical.Marshal(m.Registration())
	var want any
	want, _ = canonical.Decode(vectors.File(t, "examples/widgets.registration-request.json"))
	wantBytes, _ := canonical.Marshal(want)
	if string(got) != string(wantBytes) {
		t.Errorf("registration\n got %s\nwant %s", got, wantBytes)
	}
	if m.EventSource() != "widgets.ezghcloud.com" {
		t.Error(m.EventSource())
	}
}

// The broken registration scripts/check.mjs refuses, refused with the same paths.
func TestRegistrationRulesRefuse(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := m.Registration()
	r.Actions = append(slices.Clone(r.Actions),
		manifest.Action{ID: "widgets.widgets.describe", Description: "x", Resource: "widget", AccessLevel: "read", ManagedPolicies: []string{"WidgetsFullAccess"}},
		manifest.Action{ID: "billing.widgets.list", Description: "x", Resource: "gadget", AccessLevel: "write", ManagedPolicies: []string{"ReadOnlyAccess", "BillingFullAccess"}},
	)
	r.ResourceTypes = append(slices.Clone(r.ResourceTypes), manifest.ResourceType{Name: "bucket", Prefix: "prj", Parent: "project", Regional: true, Description: "x"})
	var paths []string
	for _, i := range manifest.RegistrationIssues(r) {
		paths = append(paths, i.Path)
	}
	slices.Sort(paths)
	want := []string{"actions.8.managedPolicies", "actions.9.accessLevel", "actions.9.id", "actions.9.managedPolicies", "actions.9.managedPolicies.1", "actions.9.resource", "resourceTypes.1.prefix"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths %v\nwant %v", paths, want)
	}
}

func TestEndpointRules(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	n := len(m.Endpoints)
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "POST", Path: "/v1/organizations/:orgId/projects/:projectId/gadgets", Auth: "caller", Action: "widgets.widgets.create", Trail: nil},
		manifest.Endpoint{Method: "GET", Path: "/metrics", Auth: "none", Trail: nil},
	)
	var paths []string
	for _, i := range m.Check() {
		paths = append(paths, i.Path)
	}
	slices.Sort(paths)
	want := []string{fmt.Sprintf("endpoints.%d.auth", n+1), fmt.Sprintf("endpoints.%d.idempotencyKey", n), fmt.Sprintf("endpoints.%d.trail", n)}
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Errorf("paths %v, want %v", paths, want)
	}
}

// The quota rules scripts/check.mjs exercises, refused with the same paths.
func TestQuotaRules(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	three, five, two := 3, 5, 2
	m.Quotas = append(m.Quotas,
		manifest.Quota{ID: "gadgets.things", DisplayName: "x", Description: "x", Kind: "rate", Scope: "organization", Default: 5, Unit: "thing", Window: "PT2H", Burst: &two, Max: &three},
		manifest.Quota{ID: "widgets.widgetsPerProject", DisplayName: "x", Description: "x", Kind: "concurrency", Scope: "resource", ResourceType: "gadget", Default: 1, Unit: "thing", TierDefaults: map[string]int{"trial": 9}, Max: &five, LeaseTTL: "PT90M"},
	)
	for i, e := range m.Endpoints {
		if e.Method == "GET" && e.Path == "/v1/organizations/:orgId/operations" {
			m.Endpoints[i].Quotas = []manifest.EndpointQuota{{ID: "widgets.widgetsPerProject", Amount: 1}, {ID: "widgets.api.requestsPerPeriod", Amount: 1}, {ID: "widgets.api.requestsPerPeriod", Amount: 1}, {ID: "widgets.nope", Amount: 1}, {ID: "widgets.api.concurrentRequests", Amount: 2}}
		}
		if e.Path == "/health" {
			m.Endpoints[i].Quotas = []manifest.EndpointQuota{{ID: "widgets.api.requestsPerPeriod", Amount: 1}}
		}
	}
	var paths []string
	for _, i := range m.Check() {
		paths = append(paths, i.Path)
	}
	slices.Sort(paths)
	want := []string{
		"endpoints.0.quotas.0",
		"endpoints.10.quotas.0", "endpoints.10.quotas.2", "endpoints.10.quotas.3", "endpoints.10.quotas.4.amount",
		"quotas.5.burst", "quotas.5.default", "quotas.5.id", "quotas.5.window",
		"quotas.6.id", "quotas.6.leaseTtl", "quotas.6.resourceType", "quotas.6.tierDefaults.trial",
	}
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Errorf("paths %v\nwant %v", paths, want)
	}
	for text, ms := range map[string]int64{"PT1S": 1000, "PT90S": 90_000, "PT60M": 3_600_000, "PT1H": 3_600_000, "PT61M": 0, "PT0S": 0, "P1D": 0, "PT2H": 0} {
		if got, ok := manifest.DurationMS(text); (ok && got != ms) || (!ok && ms != 0) {
			t.Errorf("DurationMS(%s) = %d %v", text, got, ok)
		}
	}
	var q manifest.EndpointQuota
	if err := q.UnmarshalJSON([]byte(`{"id":"widgets.a","amount":3}`)); err != nil || q.Amount != 3 {
		t.Error(q, err)
	}
	if err := q.UnmarshalJSON([]byte(`{"id":"widgets.a","nope":3}`)); err == nil {
		t.Error("an unknown field in an endpoint quota")
	}
}

// Each rule quotas-api applies to a quota definition at registration (and the schema states) is
// refused by the manifest check, at the field's path, with a message naming the quota and field.
func TestQuotaDefinitionRules(t *testing.T) {
	big, neg, hundred := manifest.MaxQuotaValue+1, -1, 100
	valid := func() manifest.Quota {
		return manifest.Quota{ID: "widgets.gadgetsPerProject", DisplayName: "Gadgets per project", Description: "How many gadgets a project holds.", Kind: "allocation", Scope: "project", Unit: "gadget", Default: 10, Max: &hundred, TierDefaults: map[string]int{"trial": 5}}
	}
	cases := []struct {
		name, path, message string
		edit                func(*manifest.Quota)
	}{
		{"a unit with a space", "unit", `widgets.gadgetsPerProject unit "alert rule" must be lowercase letters and dashes`, func(q *manifest.Quota) { q.Unit = "alert rule" }},
		{"an uppercase unit", "unit", `unit "SLO"`, func(q *manifest.Quota) { q.Unit = "SLO" }},
		{"an empty unit", "unit", `unit ""`, func(q *manifest.Quota) { q.Unit = "" }},
		{"an empty displayName", "displayName", "widgets.gadgetsPerProject displayName must be 1-100 characters, not 0", func(q *manifest.Quota) { q.DisplayName = "" }},
		{"a 101-character displayName", "displayName", "not 101", func(q *manifest.Quota) { q.DisplayName = strings.Repeat("x", 101) }},
		{"an empty description", "description", "widgets.gadgetsPerProject description must be 1-500 characters, not 0", func(q *manifest.Quota) { q.Description = "" }},
		{"a 501-character description", "description", "not 501", func(q *manifest.Quota) { q.Description = strings.Repeat("é", 501) }},
		{"a PascalCase resourceType", "resourceType", `widgets.gadgetsPerProject resourceType "Gadget" must be camelCase`, func(q *manifest.Quota) { q.Scope, q.ResourceType = "resource", "Gadget" }},
		{"a default above 10^12", "default", "widgets.gadgetsPerProject default must be an integer from 0 to 10^12", func(q *manifest.Quota) { q.Default, q.Max = big, nil }},
		{"a negative default", "default", "not -1", func(q *manifest.Quota) { q.Default = neg }},
		{"a max above 10^12", "max", "widgets.gadgetsPerProject max must be", func(q *manifest.Quota) { q.Max = &big }},
		// default above max is its own rule, so a negative max breaks two.
		{"a negative max", "max", "max must be", func(q *manifest.Quota) { q.Default, q.TierDefaults, q.Max = 0, nil, &neg }},
		{"a burst above 10^12", "burst", "widgets.gadgetsPerProject burst must be", func(q *manifest.Quota) { q.Kind, q.Window, q.Max, q.Burst = "rate", "PT1M", nil, &big }},
		{"a tier default above 10^12", "tierDefaults.trial", "widgets.gadgetsPerProject tierDefaults.trial must be", func(q *manifest.Quota) { q.Max, q.TierDefaults = nil, map[string]int{"trial": big} }},
		{"a negative tier default", "tierDefaults.trial", "not -1", func(q *manifest.Quota) { q.TierDefaults = map[string]int{"trial": neg} }},
		{"an uppercase tier name", "tierDefaults.Trial", `widgets.gadgetsPerProject tierDefaults "Trial" isn't a tier name`, func(q *manifest.Quota) { q.TierDefaults = map[string]int{"Trial": 1} }},
		{"a one-letter tier name", "tierDefaults.t", "isn't a tier name", func(q *manifest.Quota) { q.TierDefaults = map[string]int{"t": 1} }},
		{"an ID over 96 characters", "id", "at most 96 characters", func(q *manifest.Quota) { q.ID = "widgets." + strings.Repeat("a", 60) + "." + strings.Repeat("b", 30) }},
	}
	ok := valid()
	if issues := manifest.QuotaIssues("widgets", []manifest.Quota{ok}, nil); len(issues) != 0 {
		t.Fatalf("the valid quota: %v", issues)
	}
	// The edges are allowed: 100 and 500 characters (not bytes), 10^12, a two-letter tier, a dashed unit.
	ok.DisplayName, ok.Description, ok.Unit = strings.Repeat("é", 100), strings.Repeat("é", 500), "api-request"
	ok.Default, ok.Max, ok.TierDefaults = manifest.MaxQuotaValue, nil, map[string]int{"t2": manifest.MaxQuotaValue, "verified-plus": 0}
	if issues := manifest.QuotaIssues("widgets", []manifest.Quota{ok}, nil); len(issues) != 0 {
		t.Fatalf("the edges: %v", issues)
	}
	for _, c := range cases {
		q := valid()
		c.edit(&q)
		issues := manifest.QuotaIssues("widgets", []manifest.Quota{q}, nil)
		var got []string
		for _, i := range issues {
			got = append(got, i.Path+": "+i.Message)
		}
		want := 1
		if c.name == "a negative max" {
			want = 2
		}
		if len(issues) != want || issues[0].Path != "quotas.0."+c.path || !strings.Contains(issues[0].Message, c.message) {
			t.Errorf("%s: got %q, want one issue at quotas.0.%s containing %q", c.name, got, c.path, c.message)
		}
	}
}

// Every manifest declares its OpenAPI document, and nothing else is unauthenticated (the rule
// scripts/check.mjs exercises too).
func TestOpenAPIRules(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	m.Endpoints = slices.DeleteFunc(m.Endpoints, func(e manifest.Endpoint) bool { return e.Path == "/openapi.json" })
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "GET", Path: "/docs", Auth: "none"})
	var got []string
	for _, i := range m.Check() {
		got = append(got, i.Path+": "+i.Message)
	}
	slices.Sort(got)
	want := []string{
		"endpoints: must include GET /openapi.json (auth none, trail null)",
		fmt.Sprintf("endpoints.%d.auth: only GET /health, /ready and /openapi.json are unauthenticated", len(m.Endpoints)-1),
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

func TestComputationExceptionVectors(t *testing.T) {
	var cases []struct {
		Name     string
		Endpoint map[string]any
		Valid    bool
	}
	vectors.Load(t, "manifest-computation.json", &cases)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal(vectors.File(t, "examples/widgets.service-manifest.json"), &raw); err != nil {
				t.Fatal(err)
			}
			for _, value := range raw["endpoints"].([]any) {
				e := value.(map[string]any)
				if e["method"] == "POST" {
					e["async"] = false
					for k, v := range c.Endpoint {
						e[k] = v
					}
					break
				}
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			_, err = manifest.Parse(data)
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%t got %v", c.Valid, err)
			}
		})
	}
}

func TestReadAliasVectors(t *testing.T) {
	var cases []struct {
		Name   string
		Method string
		Action string
		Valid  bool
	}
	vectors.Load(t, "manifest-read-aliases.json", &cases)
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range m.Endpoints {
				if e.Method == "GET" && e.Action == "widgets.widgets.get" {
					e.Path += "/download"
					e.Method = tc.Method
					e.Action = tc.Action
					e.IdempotencyKey = e.Method == "POST"
					m.Endpoints = append(m.Endpoints, e)
					break
				}
			}
			b, _ := json.Marshal(m)
			_, err = manifest.Parse(b)
			if (err == nil) != tc.Valid {
				t.Fatalf("valid=%v: %v", tc.Valid, err)
			}
		})
	}
}

func TestDirectoryRegistrationCompatibility(t *testing.T) {
	var r manifest.Registration
	if err := json.Unmarshal(vectors.File(t, "examples/sso.registration-request.json"), &r); err != nil {
		t.Fatal(err)
	}
	if issues := manifest.RegistrationIssues(r); len(issues) != 0 {
		t.Fatal(issues)
	}
	encoded, err := canonical.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if flag, ok := document["directoryUsable"]; !ok || flag != false {
		t.Fatal(string(encoded))
	}
	r.ResourceTypes[0].Parent = "directory"
	if issues := manifest.RegistrationIssues(r); !slices.ContainsFunc(issues, func(i manifest.Issue) bool { return i.Path == "resourceTypes.0.parent" }) {
		t.Fatal(issues)
	}
	r.ResourceTypes = r.ResourceTypes[1:]
	if issues := manifest.RegistrationIssues(r); !slices.ContainsFunc(issues, func(i manifest.Issue) bool { return i.Path == "resourceTypes.0.parent" }) {
		t.Fatal(issues)
	}
	r.Namespace = "widgets"
	if issues := manifest.RegistrationIssues(r); !slices.ContainsFunc(issues, func(i manifest.Issue) bool { return i.Path == "resourceTypes.0.prefix" }) {
		t.Fatal(issues)
	}
}

func TestDirectoryUsableManifestPreservesOmission(t *testing.T) {
	data := vectors.File(t, "examples/widgets.service-manifest.json")
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []bool{false, true} {
		document["directoryUsable"] = flag
		encoded, _ := json.Marshal(document)
		m, err := manifest.Parse(encoded)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err = canonical.Marshal(m.Registration())
		if err != nil {
			t.Fatal(err)
		}
		var registration map[string]any
		if err := json.Unmarshal(encoded, &registration); err != nil {
			t.Fatal(err)
		}
		if registration["directoryUsable"] != flag {
			t.Fatal(string(encoded))
		}
	}
	var r manifest.Registration
	if err := json.Unmarshal(vectors.File(t, "examples/sso.registration-request.json"), &r); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(r)
	var registration map[string]any
	if err := json.Unmarshal(encoded, &registration); err != nil {
		t.Fatal(err)
	}
	registration["directoryUsable"] = true
	encoded, _ = json.Marshal(registration)
	if err := json.Unmarshal(encoded, &r); err != nil {
		t.Fatal(err)
	}
	if issues := manifest.RegistrationIssues(r); !slices.ContainsFunc(issues, func(i manifest.Issue) bool { return i.Path == "directoryUsable" }) {
		t.Fatal(issues)
	}
}

// The contracts' obs example follows every rule; its registration is exactly
// examples/obs.registration-request.json, ingest flag included; and its ingest endpoints are found
// by protocol and path.
func TestObsExample(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := canonical.Marshal(m.Registration())
	want, _ := canonical.Decode(vectors.File(t, "examples/obs.registration-request.json"))
	wantBytes, _ := canonical.Marshal(want)
	if string(got) != string(wantBytes) {
		t.Errorf("registration\n got %s\nwant %s", got, wantBytes)
	}
	if a, ok := m.Action("obs.logs.ingest"); !ok || !a.Ingest {
		t.Errorf("obs.logs.ingest: %+v %v", a, ok)
	}
	if a, ok := m.Action("obs.datasets.create"); !ok || a.Ingest {
		t.Errorf("obs.datasets.create: %+v %v", a, ok)
	}
	if m.Ingest == nil || m.Ingest.Hostname != "obs-ingest.ezghcloud.com" || m.Ingest.Port != 5400 || m.Ingest.GRPCPort != 5401 {
		t.Errorf("ingest host %+v", m.Ingest)
	}
	if n := len(m.IngestEndpoints()); n != 4 {
		t.Errorf("%d ingest endpoints", n)
	}
	if e, ok := m.IngestEndpoint(manifest.ProtocolHTTP, manifest.RemoteWritePath); !ok || e.Action != "obs.metrics.ingest" {
		t.Errorf("remote write %+v %v", e, ok)
	}
	e, ok := m.IngestEndpoint(manifest.ProtocolGRPC, "/opentelemetry.proto.collector.logs.v1.LogsService/Export")
	if !ok || e.Action != "obs.logs.ingest" || !e.GRPC() || !e.OnIngestHost() || e.Transport() != manifest.ProtocolGRPC {
		t.Errorf("gRPC endpoint %+v %v", e, ok)
	}
	if e, ok := m.IngestEndpoint(manifest.ProtocolHTTP, "/v1/logs"); !ok || e.GRPC() || e.Transport() != manifest.ProtocolHTTP {
		t.Errorf("HTTP endpoint %+v %v", e, ok)
	}
	if _, ok := m.IngestEndpoint(manifest.ProtocolHTTP, "/v1/organizations/:orgId/projects/:projectId/obs/datasets"); ok {
		t.Error("an API-host endpoint was found as an ingest endpoint")
	}
}

// contracts/vectors/manifest-ingest.json: each case changes the obs example; Parse accepts it
// exactly when it's valid, as scripts/check.mjs says.
func TestIngestVectors(t *testing.T) {
	var cases []struct {
		Name           string         `json:"name"`
		Manifest       map[string]any `json:"manifest"`
		Endpoint       map[string]any `json:"endpoint"`
		CallerEndpoint map[string]any `json:"callerEndpoint"`
		Action         map[string]any `json:"action"`
		Valid          bool           `json:"valid"`
	}
	vectors.Load(t, "manifest-ingest.json", &cases)
	if len(cases) < 20 {
		t.Fatalf("only %d cases", len(cases))
	}
	apply := func(target, patch map[string]any) {
		for k, v := range patch {
			if v == nil {
				delete(target, k)
			} else {
				target[k] = v
			}
		}
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal(vectors.File(t, "examples/obs.service-manifest.json"), &raw); err != nil {
				t.Fatal(err)
			}
			apply(raw, c.Manifest)
			for _, v := range raw["endpoints"].([]any) {
				e := v.(map[string]any)
				switch {
				case e["method"] == "POST" && e["path"] == "/v1/logs":
					apply(e, c.Endpoint)
				case e["method"] == "POST" && e["path"] == "/v1/organizations/:orgId/projects/:projectId/obs/datasets":
					apply(e, c.CallerEndpoint)
				}
			}
			for _, v := range raw["actions"].([]any) {
				if a := v.(map[string]any); a["id"] == "obs.logs.ingest" {
					apply(a, c.Action)
				}
			}
			data, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manifest.Parse(data); (err == nil) != c.Valid {
				t.Fatalf("valid=%t: %v", c.Valid, err)
			}
		})
	}
}

// An ingest server's routes must be exactly the manifest's ingest endpoints, as Service.Handler's
// must be the API host's.
func TestCheckIngestRoutes(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	logsHTTP := manifest.IngestRoute{Protocol: manifest.ProtocolHTTP, Path: "/v1/logs"}
	logsGRPC := manifest.IngestRoute{Protocol: manifest.ProtocolGRPC, Path: "/opentelemetry.proto.collector.logs.v1.LogsService/Export"}
	metricsHTTP := manifest.IngestRoute{Protocol: manifest.ProtocolHTTP, Path: "/v1/metrics"}
	remoteWrite := manifest.IngestRoute{Protocol: manifest.ProtocolHTTP, Path: manifest.RemoteWritePath}
	if err := m.CheckIngestRoutes([]manifest.IngestRoute{logsGRPC, logsHTTP, metricsHTTP, remoteWrite}); err != nil {
		t.Fatalf("exact routes: %v", err)
	}
	err = m.CheckIngestRoutes([]manifest.IngestRoute{logsHTTP, logsHTTP, metricsHTTP, remoteWrite, {Protocol: manifest.ProtocolHTTP, Path: "/v1/traces"}})
	for _, want := range []string{
		"grpc /opentelemetry.proto.collector.logs.v1.LogsService/Export is in the manifest but isn't served",
		"http /v1/traces is served but isn't an ingest endpoint in the manifest",
		"http /v1/logs is served twice",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
}

// An ingest server's routes include its internal producer routes (auth service under /internal/
// on the ingest host): CheckIngestRoutes compares both kinds with the manifest.
func TestCheckIngestRoutesInternalProducers(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal(vectors.File(t, "examples/obs.service-manifest.json"), &raw); err != nil {
		t.Fatal(err)
	}
	raw["endpoints"] = append(raw["endpoints"].([]any), map[string]any{"method": "POST", "path": "/internal/v1/otlp/v1/traces", "host": "ingest", "auth": "service", "trail": nil})
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	internal := manifest.IngestRoute{Protocol: manifest.ProtocolHTTP, Path: "/internal/v1/otlp/v1/traces"}
	if e, ok := m.IngestEndpoint(manifest.ProtocolHTTP, internal.Path); !ok || e.Auth != "service" {
		t.Fatalf("internal producer route %+v %v", e, ok)
	}
	public := []manifest.IngestRoute{
		{Protocol: manifest.ProtocolHTTP, Path: "/v1/logs"},
		{Protocol: manifest.ProtocolGRPC, Path: "/opentelemetry.proto.collector.logs.v1.LogsService/Export"},
		{Protocol: manifest.ProtocolHTTP, Path: "/v1/metrics"},
		{Protocol: manifest.ProtocolHTTP, Path: manifest.RemoteWritePath},
	}
	if err := m.CheckIngestRoutes(append(slices.Clone(public), internal)); err != nil {
		t.Fatalf("exact routes: %v", err)
	}
	if err := m.CheckIngestRoutes(public); err == nil || !strings.Contains(err.Error(), "http /internal/v1/otlp/v1/traces is in the manifest but isn't served") {
		t.Errorf("unserved internal route: %v", err)
	}
	other := manifest.IngestRoute{Protocol: manifest.ProtocolHTTP, Path: "/internal/v1/otlp/v1/metrics"}
	if err := m.CheckIngestRoutes(append(slices.Clone(public), internal, other)); err == nil || !strings.Contains(err.Error(), "http /internal/v1/otlp/v1/metrics is served but isn't an ingest endpoint in the manifest") {
		t.Errorf("undeclared internal route: %v", err)
	}
}

// igk is org-api's project ingest keys' prefix: no product registers it (as scripts/check.mjs).
func TestIngestKeyPrefixIsReserved(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := m.Registration()
	r.ResourceTypes = append(slices.Clone(r.ResourceTypes), manifest.ResourceType{Name: "gadget", Prefix: "igk", Parent: "project", Regional: true, Description: "x"})
	var got []string
	for _, i := range manifest.RegistrationIssues(r) {
		got = append(got, i.Path+": "+i.Message)
	}
	if want := []string{"resourceTypes.1.prefix: igk is reserved"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// [being built] fld is org-api's folders' prefix and folders its folder actions' namespace
// (contracts/identity-and-authz.md, "Folder actions"): no product registers either.
func TestFolderPrefixAndNamespaceAreReserved(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	issues := func(r manifest.Registration) (got []string) {
		for _, i := range manifest.RegistrationIssues(r) {
			got = append(got, i.Path+": "+i.Message)
		}
		return got
	}
	r := m.Registration()
	r.ResourceTypes = append(slices.Clone(r.ResourceTypes), manifest.ResourceType{Name: "gadget", Prefix: "fld", Parent: "project", Regional: true, Description: "x"})
	if got, want := issues(r), []string{"resourceTypes.1.prefix: fld is reserved"}; !slices.Equal(got, want) {
		t.Errorf("fld: got %v, want %v", got, want)
	}
	r = m.Registration()
	r.Namespace = "folders"
	if got := issues(r); !slices.Contains(got, "namespace: folders is reserved") {
		t.Errorf("folders namespace: got %v", got)
	}
}

// contracts/vectors/manifest-platform-only.json: each case sets one widgets action's platformOnly
// and managedPolicies (its changes are org-api's, which keeps registered state); the kit's issues are the rules' kit column, and the registration sends
// "platformOnly": true exactly when the registrations' stored column has it (never false, so the
// digest of a registration without it doesn't change). A false the schema refuses can't be sent.
func TestPlatformOnlyVectors(t *testing.T) {
	type platformOnlyCase struct {
		Name            string   `json:"name"`
		Action          string   `json:"action"`
		PlatformOnly    *bool    `json:"platformOnly"`
		ManagedPolicies []string `json:"managedPolicies"`
		Stored          *bool    `json:"stored"`
		Kit             []string `json:"kit"`
	}
	var file struct {
		Action        string             `json:"action"`
		Registrations []platformOnlyCase `json:"registrations"`
		Rules         []platformOnlyCase `json:"rules"`
	}
	vectors.Load(t, "manifest-platform-only.json", &file)
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	withCase := func(c platformOnlyCase) manifest.Registration {
		r := m.Registration()
		r.Actions = slices.Clone(r.Actions)
		id := cmp.Or(c.Action, file.Action)
		i := slices.IndexFunc(r.Actions, func(a manifest.Action) bool { return a.ID == id })
		r.Actions[i].ManagedPolicies = c.ManagedPolicies
		r.Actions[i].PlatformOnly = c.PlatformOnly != nil && *c.PlatformOnly
		return r
	}
	for _, c := range file.Rules {
		if c.Kit == nil {
			continue
		}
		got := []string{}
		for _, issue := range manifest.RegistrationIssues(withCase(c)) {
			got = append(got, issue.Path)
		}
		if !slices.Equal(got, c.Kit) {
			t.Errorf("%s: issues %v, want %v", c.Name, got, c.Kit)
		}
	}
	for _, c := range file.Registrations {
		body, err := json.Marshal(withCase(c))
		if err != nil {
			t.Fatal(err)
		}
		if sent := strings.Contains(string(body), `"platformOnly":true`); sent != (c.Stored != nil) || strings.Count(string(body), `"platformOnly"`) > 1 {
			t.Errorf("%s: sent %s", c.Name, body)
		}
	}
}

// contracts/vectors/manifest-org-api.json rules: each case sets the widgets registration's orgApi;
// the kit's issues are the vectors' kit column (shape and uniqueness only: a name org-api doesn't
// have is org-api's to refuse). A manifest's orgApi is registered as is, and none sends no field.
func TestOrgAPIVectors(t *testing.T) {
	var file struct {
		Rules []struct {
			Name   string           `json:"name"`
			OrgAPI *manifest.OrgAPI `json:"orgApi"`
			Kit    []string         `json:"kit"`
		} `json:"rules"`
	}
	vectors.Load(t, "manifest-org-api.json", &file)
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range file.Rules {
		r := m.Registration()
		r.OrgAPI = c.OrgAPI
		got := []string{}
		for _, issue := range manifest.RegistrationIssues(r) {
			got = append(got, issue.Path)
		}
		if !slices.Equal(got, c.Kit) {
			t.Errorf("%s: issues %v, want %v", c.Name, got, c.Kit)
		}
	}

	// [being built] Operations reserved to one namespace: the vectors' table is the kit's, and that
	// namespace (only) may declare each.
	var reserved struct {
		ReservedOperations map[string][]string `json:"reservedOperations"`
	}
	vectors.Load(t, "manifest-org-api.json", &reserved)
	if !maps.EqualFunc(reserved.ReservedOperations, manifest.ReservedOperations, slices.Equal[[]string]) {
		t.Errorf("reserved operations %v, the vectors' %v", manifest.ReservedOperations, reserved.ReservedOperations)
	}
	certificates := manifest.Registration{
		RegistrationVersion: 1, Namespace: "certificates", DisplayName: "Web Certificates", Description: "Private CA.",
		OrgAPI:          &manifest.OrgAPI{Operations: []string{"permissions.batch", "permissions.principalBatch"}},
		ResourceTypes:   []manifest.ResourceType{},
		ManagedPolicies: []manifest.ManagedPolicy{},
		Actions:         []manifest.Action{{ID: "certificates.certificateAuthorities.list", Description: "x", Resource: "project", AccessLevel: "list", ManagedPolicies: []string{"ReadOnlyAccess"}}},
	}
	if issues := manifest.RegistrationIssues(certificates); len(issues) != 0 {
		t.Errorf("certificates declaring permissions.principalBatch: %v", issues)
	}

	compute := certificates
	compute.Namespace = "compute"
	compute.Actions = []manifest.Action{{ID: "compute.instances.list", Description: "x", Resource: "project", AccessLevel: "list", ManagedPolicies: []string{"ReadOnlyAccess"}}}
	compute.OrgAPI = &manifest.OrgAPI{Operations: []string{"instanceBots.lookup", "instanceBots.mint"}}
	if issues := manifest.RegistrationIssues(compute); len(issues) != 0 {
		t.Errorf("compute declaring instanceBots: %v", issues)
	}
	certificates.OrgAPI = &manifest.OrgAPI{Operations: []string{"instanceBots.mint"}}
	if issues := manifest.RegistrationIssues(certificates); len(issues) != 1 {
		t.Errorf("certificates declaring instanceBots.mint: %v", issues)
	}

	network := certificates
	network.Namespace = "network"
	network.Actions = []manifest.Action{{ID: "network.interfaces.list", Description: "x", Resource: "project", AccessLevel: "list", ManagedPolicies: []string{"ReadOnlyAccess"}}}
	network.OrgAPI = &manifest.OrgAPI{Operations: []string{"organizations.rootContact"}}
	if issues := manifest.RegistrationIssues(network); len(issues) != 0 {
		t.Errorf("network declaring organizations.rootContact: %v", issues)
	}
	certificates.OrgAPI = &manifest.OrgAPI{Operations: []string{"organizations.rootContact"}}
	if issues := manifest.RegistrationIssues(certificates); len(issues) != 1 {
		t.Errorf("certificates declaring organizations.rootContact: %v", issues)
	}

	kubernetes := certificates
	kubernetes.Namespace = "kubernetes"
	kubernetes.Actions = []manifest.Action{{ID: "kubernetes.clusters.list", Description: "x", Resource: "project", AccessLevel: "list", ManagedPolicies: []string{"ReadOnlyAccess"}}}
	kubernetes.OrgAPI = &manifest.OrgAPI{Operations: []string{"kubernetes.authenticate", "kubernetes.mint", "organizations.rootContact"}}
	if issues := manifest.RegistrationIssues(kubernetes); len(issues) != 0 {
		t.Errorf("kubernetes declaring kubernetes.mint: %v", issues)
	}
	compute.OrgAPI = &manifest.OrgAPI{Operations: []string{"kubernetes.mint", "kubernetes.authenticate"}}
	if issues := manifest.RegistrationIssues(compute); len(issues) != 2 {
		t.Errorf("compute declaring kubernetes.mint and kubernetes.authenticate: %v", issues)
	}

	body, _ := json.Marshal(m.Registration())
	if strings.Contains(string(body), `"orgApi"`) {
		t.Errorf("widgets sent orgApi: %s", body)
	}
	var document map[string]any
	if err := json.Unmarshal(vectors.File(t, "examples/widgets.service-manifest.json"), &document); err != nil {
		t.Fatal(err)
	}
	document["orgApi"] = map[string]any{"operations": []string{"permissions.batch", "some.futureOperation"}}
	encoded, _ := json.Marshal(document)
	declaring, err := manifest.Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if r := declaring.Registration(); r.OrgAPI == nil || !slices.Equal(r.OrgAPI.Operations, []string{"permissions.batch", "some.futureOperation"}) {
		t.Errorf("registration %+v", r.OrgAPI)
	}
	document["orgApi"] = map[string]any{"operations": []string{"permissions.batch"}, "approved": true}
	encoded, _ = json.Marshal(document)
	if _, err := manifest.Parse(encoded); err == nil {
		t.Error("an unknown field in orgApi was accepted")
	}
}

// manifestCase is a case of the manifest vectors that change an example (scripts/lib/
// manifest-cases.mjs): manifest patches top-level fields, endpoints patches endpoints by
// "METHOD path", remove drops endpoints, add appends them, actions patches actions by ID. A null
// deletes a field, except an endpoint's trail, where null is a value.
type manifestCase struct {
	Name      string                    `json:"name"`
	Manifest  map[string]any            `json:"manifest"`
	Endpoints map[string]map[string]any `json:"endpoints"`
	Remove    []string                  `json:"remove"`
	Add       []map[string]any          `json:"add"`
	Actions   map[string]map[string]any `json:"actions"`
	Valid     bool                      `json:"valid"`
}

func patchFields(target, patch map[string]any) {
	for k, v := range patch {
		if v == nil && k != "trail" {
			delete(target, k)
		} else {
			target[k] = v
		}
	}
}

// apply is the case's manifest, as JSON.
func (c manifestCase) apply(t *testing.T, example string) []byte {
	t.Helper()
	return c.applyTo(t, vectors.File(t, example))
}

// applyTo is the case's manifest, as JSON, changing base.
func (c manifestCase) applyTo(t *testing.T, base []byte) []byte {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(base, &raw); err != nil {
		t.Fatal(err)
	}
	patchFields(raw, c.Manifest)
	found := map[string]bool{}
	var kept []any
	for _, v := range raw["endpoints"].([]any) {
		e := v.(map[string]any)
		key := fmt.Sprint(e["method"], " ", e["path"])
		if p, ok := c.Endpoints[key]; ok {
			patchFields(e, p)
			found[key] = true
		}
		if !slices.Contains(c.Remove, key) {
			kept = append(kept, e)
		}
	}
	for key := range c.Endpoints {
		if !found[key] {
			t.Fatalf("no endpoint %s", key)
		}
	}
	for _, e := range c.Add {
		kept = append(kept, e)
	}
	raw["endpoints"] = kept
	for _, v := range raw["actions"].([]any) {
		a := v.(map[string]any)
		if p, ok := c.Actions[a["id"].(string)]; ok {
			patchFields(a, p)
		}
	}
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// runManifestCases checks that Parse accepts each case exactly when it's valid, as
// scripts/check.mjs says.
func runManifestCases(t *testing.T, file, example string, atLeast int) {
	var cases []manifestCase
	vectors.Load(t, file, &cases)
	if len(cases) < atLeast {
		t.Fatalf("only %d cases", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if _, err := manifest.Parse(c.apply(t, example)); (err == nil) != c.Valid {
				t.Fatalf("valid=%t: %v", c.Valid, err)
			}
		})
	}
}

// contracts/vectors/manifest-edge.json, on the MCP Central excerpt.
func TestEdgeVectors(t *testing.T) {
	runManifestCases(t, "manifest-edge.json", "examples/mcpcentral.service-manifest.json", 20)
}

func TestEdgeHelpers(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/mcpcentral.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var edge []string
	for _, e := range m.EdgeEndpoints() {
		edge = append(edge, e.ComponentName()+" "+e.Method+" "+e.Path)
	}
	want := []string{
		"api POST /central", "edge GET /v1/me", "edge POST /v1/servers/:serverId/connection",
		"api GET /.well-known/oauth-protected-resource", "api GET /.well-known/oauth-protected-resource/central",
		"edge GET /sign-in", "edge GET /callback", "edge GET /oauth/callback",
	}
	if !slices.Equal(edge, want) {
		t.Errorf("%v", edge)
	}
	if e, _ := m.Endpoint("GET", "/health"); !e.Builtin() || e.ComponentName() != manifest.ComponentAPI {
		t.Errorf("%+v", e)
	}
	if m.Edge == nil || m.Edge.Domain != "mcp.ezgh.app" || m.Edge.Port != 5001 {
		t.Errorf("%+v", m.Edge)
	}
}

// contracts/vectors/manifest-public.json, on the MCP Central excerpt.
func TestPublicVectors(t *testing.T) {
	runManifestCases(t, "manifest-public.json", "examples/mcpcentral.service-manifest.json", 10)
	m, err := manifest.Parse(vectors.File(t, "examples/mcpcentral.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := m.Endpoint("GET", "/v1/oauth/callback"); e.Public != manifest.PublicRedirect || e.Builtin() {
		t.Errorf("%+v", e)
	}
}

// contracts/vectors/manifest-prometheus.json, on the obs example: errorModel, a GET and a
// computation POST sharing one event, and remote write's ingest path.
func TestPrometheusVectors(t *testing.T) {
	runManifestCases(t, "manifest-prometheus.json", "examples/obs.service-manifest.json", 20)
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	const path = "/v1/organizations/:orgId/projects/:projectId/obs/prometheus/api/v1/query"
	get, _ := m.Endpoint("GET", path)
	post, _ := m.Endpoint("POST", path)
	if get.ErrorModel != manifest.ErrorModelPrometheus || post.ErrorModel != manifest.ErrorModelPrometheus || !manifest.ReadPair(get, post) || !manifest.ReadPair(post, get) {
		t.Errorf("%+v %+v", get, post)
	}
	if ds, _ := m.Endpoint("GET", "/v1/organizations/:orgId/projects/:projectId/obs/datasets"); manifest.ReadPair(get, ds) {
		t.Error("endpoints of different paths are a read pair")
	}
}

// contracts/vectors/manifest-openai.json, on the inference example: errorModel openai on caller
// GETs and POSTs under the project's OpenAI base, with bearer credentials only.
func TestOpenAIVectors(t *testing.T) {
	runManifestCases(t, "manifest-openai.json", "examples/inference.service-manifest.json", 14)
	m, err := manifest.Parse(vectors.File(t, "examples/inference.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.ServiceCredentials == nil || m.ServiceCredentials.IssuerPrefix != "ifk" {
		t.Errorf("serviceCredentials %+v", m.ServiceCredentials)
	}
	for _, e := range []struct{ method, path string }{
		{"POST", "/v1/organizations/:orgId/projects/:projectId/openai/v1/embeddings"},
		{"GET", "/v1/organizations/:orgId/projects/:projectId/openai/v1/models"},
		{"GET", "/v1/organizations/:orgId/projects/:projectId/openai/v1/models/:model"},
	} {
		if got, _ := m.Endpoint(e.method, e.path); got.ErrorModel != manifest.ErrorModelOpenAI || got.Credentials != manifest.CredentialsBearer {
			t.Errorf("%+v", got)
		}
	}
}

// A computation POST may record refusals only (contracts/trails.md, "The run-operation volume
// exception"): EZGH Inference's CreateEmbeddings, in the inference example. A GET may only as a Data
// event (the Git content-read exception).
func TestRefusalsOnlyAllowedOnComputationPost(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/inference.service-manifest.json"))
	if err != nil {
		t.Fatalf("a computation POST may record refusals only: %v", err)
	}
	e, _ := m.Endpoint("POST", "/v1/organizations/:orgId/projects/:projectId/openai/v1/embeddings")
	// Its refusals are writes, so audit-api always keeps them (the only record of the call).
	if !e.Computation() || e.Trail.Record != manifest.RecordRefusals || e.ReadOnly() {
		t.Fatalf("%+v", e)
	}
	get := manifestCase{Endpoints: map[string]map[string]any{"GET /v1/organizations/:orgId/projects/:projectId/openai/v1/models": {"trail": map[string]any{"eventName": "ListModels", "record": "refusals"}}}}
	if _, err := manifest.Parse(get.apply(t, "examples/inference.service-manifest.json")); err == nil || !strings.Contains(err.Error(), "a GET records refusals only as a Data event") {
		t.Fatalf("a GET recording refusals only: %v", err)
	}
}

// Each OpenAI error-model rule names what's wrong.
func TestOpenAIErrorModelMessages(t *testing.T) {
	const embeddings = "POST /v1/organizations/:orgId/projects/:projectId/openai/v1/embeddings"
	cases := []struct {
		c    manifestCase
		want string
	}{
		{manifestCase{Add: []map[string]any{{"method": "POST", "path": "/v1/organizations/:orgId/openai/v1/embeddings", "auth": "caller", "action": "inference.models.invoke", "credentials": "bearer", "errorModel": "openai", "idempotencyException": "computation", "trail": map[string]any{"eventName": "CreateOrganizationEmbeddings"}}}},
			"an OpenAI endpoint is under /v1/organizations/:orgId/projects/:projectId/openai/v1/"},
		{manifestCase{Endpoints: map[string]map[string]any{"GET /internal/v1/catalog/models": {"errorModel": "openai"}}},
			"only caller endpoints on the API host use another error model"},
		{manifestCase{Add: []map[string]any{{"method": "DELETE", "path": "/v1/organizations/:orgId/projects/:projectId/openai/v1/models/:model", "auth": "caller", "action": "inference.models.invoke", "credentials": "bearer", "errorModel": "openai", "idempotencyKey": true, "trail": map[string]any{"eventName": "DeleteModel"}}}},
			"the OpenAI error model is for GET and POST"},
		{manifestCase{Endpoints: map[string]map[string]any{embeddings: {"credentials": nil}}},
			"an OpenAI endpoint takes bearer credentials only"},
		{manifestCase{Endpoints: map[string]map[string]any{embeddings: {"errorModel": "graphql"}}},
			"must be prometheus or openai"},
	}
	for _, tc := range cases {
		_, err := manifest.Parse(tc.c.apply(t, "examples/inference.service-manifest.json"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}
}

// contracts/vectors/trails-recording.json readOnly: which endpoints' events are readOnly.
func TestReadOnlyVectors(t *testing.T) {
	var file struct {
		ReadOnly []struct {
			Name     string            `json:"name"`
			Endpoint manifest.Endpoint `json:"endpoint"`
			ReadOnly bool              `json:"readOnly"`
		} `json:"readOnly"`
	}
	vectors.Load(t, "trails-recording.json", &file)
	if len(file.ReadOnly) < 5 {
		t.Fatalf("only %d cases", len(file.ReadOnly))
	}
	for _, c := range file.ReadOnly {
		if got := c.Endpoint.ReadOnly(); got != c.ReadOnly {
			t.Errorf("%s: readOnly %t", c.Name, got)
		}
	}
}

// contracts/vectors/manifest-rpc-trails.json, on the MCP Central excerpt.
func TestRPCTrailVectors(t *testing.T) {
	runManifestCases(t, "manifest-rpc-trails.json", "examples/mcpcentral.service-manifest.json", 12)
}

// contracts/vectors/trails-rpc.json: which entry a call records.
func TestTrailForMethod(t *testing.T) {
	var file struct {
		Trail *manifest.Trail `json:"trail"`
		Cases []struct {
			Name     string          `json:"name"`
			Method   *string         `json:"method"`
			Expected *manifest.Trail `json:"expected"`
		} `json:"cases"`
		Plain struct {
			Trail    *manifest.Trail `json:"trail"`
			Method   string          `json:"method"`
			Expected *manifest.Trail `json:"expected"`
		} `json:"plain"`
	}
	vectors.Load(t, "trails-rpc.json", &file)
	same := func(a, b *manifest.Trail) bool {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return string(x) == string(y)
	}
	for _, c := range file.Cases {
		method := ""
		if c.Method != nil {
			method = *c.Method
		}
		if got := file.Trail.ForMethod(method); !same(got, c.Expected) {
			t.Errorf("%s: %+v, want %+v", c.Name, got, c.Expected)
		}
	}
	if got := file.Plain.Trail.ForMethod(file.Plain.Method); got != file.Plain.Trail || !same(got, file.Plain.Expected) {
		t.Errorf("plain: %+v", got)
	}
	if !file.Trail.RPC() || file.Plain.Trail.RPC() || file.Trail.ForMethod("tools/call").Category() != manifest.CategoryData || file.Plain.Trail.Category() != manifest.CategoryManagement {
		t.Error("RPC or Category")
	}
}

// A trail reads and writes back in the form it was written in, and refuses mixtures.
func TestTrailJSON(t *testing.T) {
	for _, text := range []string{
		`{"eventName":"ConnectServer"}`,
		`{"eventName":"CallTool","eventCategory":"Data","sensitive":["arguments"],"responseElements":["id"]}`,
		`{"byRpcMethod":{"initialize":null,"tools/call":{"eventName":"CallTool","eventCategory":"Data"}},"otherwise":null}`,
		`{"byRpcMethod":{"tools/call":{"eventName":"CallTool"}},"otherwise":{"eventName":"UseGateway"}}`,
	} {
		var tr manifest.Trail
		if err := json.Unmarshal([]byte(text), &tr); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		out, _ := json.Marshal(tr)
		a, _ := canonical.Decode([]byte(text))
		b, _ := canonical.Decode(out)
		x, _ := canonical.Marshal(a)
		y, _ := canonical.Marshal(b)
		if string(x) != string(y) {
			t.Errorf("%s came back %s", text, out)
		}
	}
	for _, text := range []string{
		`{"byRpcMethod":{"tools/call":{"eventName":"CallTool"}}}`,
		`{"eventName":"X","byRpcMethod":{},"otherwise":null}`,
		`{"byRpcMethod":{"tools/call":{"eventName":"CallTool","extra":1}},"otherwise":null}`,
		`{"eventName":"X","extra":1}`,
		`{"byRpcMethod":{},"otherwise":{"eventName":"X","nested":{}}}`,
	} {
		var tr manifest.Trail
		if err := json.Unmarshal([]byte(text), &tr); err == nil {
			t.Errorf("%s was accepted", text)
		}
	}
}

// contracts/vectors/manifest-credentials.json, on the MCP Central excerpt.
func TestCredentialsVectors(t *testing.T) {
	runManifestCases(t, "manifest-credentials.json", "examples/mcpcentral.service-manifest.json", 10)
	m, err := manifest.Parse(vectors.File(t, "examples/mcpcentral.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c := m.Edge.Cookie; c == nil || c.Name != "__Host-mcp_portal" || c.Audience != "portal" {
		t.Errorf("%+v", c)
	}
}

// contracts/vectors/manifest-scopes.json: the kits apply org-api's scope rules (and, having no
// schema, refuse what the schema does); the MCP Central excerpt registers its scopes as is.
func TestScopeVectors(t *testing.T) {
	var file struct {
		Rules []struct {
			Name   string          `json:"name"`
			Scopes json.RawMessage `json:"scopes"`
			Schema bool            `json:"schema"`
			Issues []string        `json:"issues"`
		} `json:"rules"`
		NotDirectoryUsable struct {
			Issues []string `json:"issues"`
		} `json:"notDirectoryUsable"`
	}
	vectors.Load(t, "manifest-scopes.json", &file)
	m, err := manifest.Parse(vectors.File(t, "examples/mcpcentral.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := canonical.Marshal(m.Registration())
	want, _ := canonical.Decode(vectors.File(t, "examples/mcpcentral.registration-request.json"))
	wantBytes, _ := canonical.Marshal(want)
	if string(got) != string(wantBytes) {
		t.Errorf("registration\n got %s\nwant %s", got, wantBytes)
	}
	if len(file.Rules) < 15 {
		t.Fatalf("only %d rules", len(file.Rules))
	}
	for _, c := range file.Rules {
		// Strictly, as Parse reads a manifest: an unknown field is refused.
		var scopes []manifest.Scope
		dec := json.NewDecoder(strings.NewReader(string(c.Scopes)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&scopes); err != nil {
			if c.Schema {
				t.Errorf("%s: %v", c.Name, err)
			}
			continue
		}
		r := m.Registration()
		r.Scopes = scopes
		paths := []string{}
		for _, issue := range manifest.RegistrationIssues(r) {
			paths = append(paths, issue.Path)
		}
		switch {
		case c.Schema && !slices.Equal(paths, c.Issues):
			t.Errorf("%s: issues %v, want %v", c.Name, paths, c.Issues)
		case !c.Schema && len(paths) == 0:
			t.Errorf("%s: accepted", c.Name)
		}
	}
	r := m.Registration()
	r.DirectoryUsable = nil
	var paths []string
	for _, issue := range manifest.RegistrationIssues(r) {
		paths = append(paths, issue.Path)
	}
	if !slices.Equal(paths, file.NotDirectoryUsable.Issues) {
		t.Errorf("not directory-usable: %v", paths)
	}
	widgets, _ := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if body, _ := json.Marshal(widgets.Registration()); strings.Contains(string(body), `"scopes"`) {
		t.Errorf("widgets sent scopes: %s", body)
	}
}

func TestMatchesActionPattern(t *testing.T) {
	for _, c := range []struct {
		pattern, action string
		want            bool
	}{
		{"mcpcentral.tools.call", "mcpcentral.tools.call", true},
		{"mcpcentral.tools.call", "mcpcentral.tools.callx", false},
		{"mcpcentral.*", "mcpcentral.tools.call", true},
		{"mcpcentral.connections.*", "mcpcentral.connections.connect", true},
		{"mcpcentral.connections.*", "mcpcentral.connection", false},
		{"*.list", "mcpcentral.servers.list", true},
		{"mcpcentral.*.get", "mcpcentral.servers.get", true},
		{"mcpcentral.*.get", "mcpcentral.get", false},
		{"a*b*c", "abc", true},
		{"a*b*c", "ac", false},
		{"a*bb*b", "abbb", true},
		{"a*bb*b", "abb", false},
		{"*", "anything", true},
	} {
		if got := manifest.MatchesActionPattern(c.pattern, c.action); got != c.want {
			t.Errorf("%s ~ %s: %t", c.pattern, c.action, got)
		}
	}
}

// contracts/vectors/manifest-timeouts.json, on the MCP Central excerpt.
func TestTimeoutVectors(t *testing.T) {
	runManifestCases(t, "manifest-timeouts.json", "examples/mcpcentral.service-manifest.json", 6)
	m, err := manifest.Parse(vectors.File(t, "examples/mcpcentral.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := m.Endpoint("POST", "/central"); e.TimeoutSeconds != 900 {
		t.Errorf("%+v", e)
	}
}

// contracts/vectors/manifest-live.json, on the obs example: live hosts and live-host endpoints.
func TestLiveVectors(t *testing.T) {
	runManifestCases(t, "manifest-live.json", "examples/obs.service-manifest.json", 30)
}

// The obs example's live tail is the live component's only endpoint, on obs-live.ezghcloud.com.
func TestLiveHelpers(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Live == nil || m.Live.Hostname != "obs-live.ezghcloud.com" || m.Live.Port != 5550 {
		t.Errorf("%+v", m.Live)
	}
	live := m.LiveEndpoints()
	if len(live) != 1 {
		t.Fatalf("%d live endpoints", len(live))
	}
	e := live[0]
	if !e.OnLiveHost() || e.ComponentName() != manifest.ComponentLive || e.TimeoutSeconds != 900 || e.Trail.EventName != "TailLogs" {
		t.Errorf("%+v", e)
	}
	if e, _ := m.Endpoint("GET", "/v1/organizations/:orgId/projects/:projectId/obs/datasets"); e.OnLiveHost() || e.ComponentName() != manifest.ComponentAPI {
		t.Errorf("%+v", e)
	}
}

// registryVectors is contracts/vectors/manifest-registry.json: a valid registry excerpt and the
// cases that change it.
type registryVectors struct {
	Manifest json.RawMessage `json:"manifest"`
	Cases    []struct {
		manifestCase
		Issue string `json:"issue"`
	} `json:"cases"`
}

// registryManifest is the vectors' registry excerpt, parsed.
func registryManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	var v registryVectors
	vectors.Load(t, "manifest-registry.json", &v)
	m, err := manifest.Parse(v.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// registryEndpoint is the excerpt's endpoint for method and path, to change in place.
func registryEndpoint(t *testing.T, m *manifest.Manifest, method, path string) *manifest.Endpoint {
	t.Helper()
	for i := range m.Endpoints {
		if m.Endpoints[i].Method == method && m.Endpoints[i].Path == path {
			return &m.Endpoints[i]
		}
	}
	t.Fatalf("no endpoint %s %s", method, path)
	return nil
}

func TestRegistryHostValid(t *testing.T) {
	m := registryManifest(t)
	if issues := m.Check(); len(issues) != 0 {
		t.Fatalf("issues: %v", issues)
	}
	if m.Registry == nil || m.Registry.Domain != "registry.ezghcloud.com" || m.Registry.Port != 5801 {
		t.Fatalf("%+v", m.Registry)
	}
	// The API moves to <namespace>-api; the Trails eventSource stays <namespace>.ezghcloud.com.
	if m.API.Hostname != "registry-api.ezghcloud.com" || m.EventSource() != "registry.ezghcloud.com" {
		t.Fatalf("%s %s", m.API.Hostname, m.EventSource())
	}
}

func TestRegistryHostRules(t *testing.T) {
	const manifests = "/v2/<name>/manifests/<reference>"
	cases := map[string]func(t *testing.T, m *manifest.Manifest){
		"host without endpoints": func(t *testing.T, m *manifest.Manifest) {
			m.Endpoints = slices.DeleteFunc(m.Endpoints, manifest.Endpoint.OnRegistryHost)
		},
		"endpoints without host":   func(t *testing.T, m *manifest.Manifest) { m.Registry = nil },
		"domain outside ezghcloud": func(t *testing.T, m *manifest.Manifest) { m.Registry.Domain = "registry.ezgh.app" },
		"domain is the API host":   func(t *testing.T, m *manifest.Manifest) { m.Registry.Domain = m.API.Hostname },
		"another product's domain": func(t *testing.T, m *manifest.Manifest) { m.Registry.Domain = "cr.ezghcloud.com" },
		"API not moved to -api":    func(t *testing.T, m *manifest.Manifest) { m.API.Hostname = "registry.ezghcloud.com" },
		"the bare API host beside a registry host": func(t *testing.T, m *manifest.Manifest) {
			m.API.Hostname = m.Namespace + ".ezghcloud.com"
		},
		"port equals api port": func(t *testing.T, m *manifest.Manifest) { m.Registry.Port = m.API.Port },
		"HEAD on the API host": func(t *testing.T, m *manifest.Manifest) {
			m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "HEAD", Path: "/v1/organizations/:orgId/projects/:projectId/repositories", Auth: "caller", Action: "registry.repositories.list"})
		},
		"path outside the OCI set": func(t *testing.T, m *manifest.Manifest) {
			registryEndpoint(t, m, "GET", manifests).Path = "/v2/_catalog"
		},
		"method not allowed": func(t *testing.T, m *manifest.Manifest) {
			registryEndpoint(t, m, "GET", "/v2/<name>/tags/list").Method = "POST"
		},
		"auth none on manifests": func(t *testing.T, m *manifest.Manifest) {
			e := registryEndpoint(t, m, "GET", manifests)
			e.Auth, e.Action, e.Trail = "none", "", nil
		},
		"registry auth no action": func(t *testing.T, m *manifest.Manifest) { registryEndpoint(t, m, "GET", manifests).Action = "" },
		"registry auth on the API host": func(t *testing.T, m *manifest.Manifest) {
			m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "GET", Path: "/v1/organizations/:orgId/projects/:projectId/images", Auth: manifest.AuthRegistry, Action: "registry.images.list"})
		},
		"idempotency key": func(t *testing.T, m *manifest.Manifest) {
			registryEndpoint(t, m, "PUT", manifests).IdempotencyKey = true
		},
		"component named": func(t *testing.T, m *manifest.Manifest) {
			registryEndpoint(t, m, "GET", manifests).Component = manifest.ComponentAPI
		},
		"async": func(t *testing.T, m *manifest.Manifest) { registryEndpoint(t, m, "PUT", manifests).Async = true },
		"timeout": func(t *testing.T, m *manifest.Manifest) {
			registryEndpoint(t, m, "PUT", manifests).TimeoutSeconds = 900
		},
		"credentials": func(t *testing.T, m *manifest.Manifest) {
			registryEndpoint(t, m, "GET", manifests).Credentials = manifest.CredentialsBearer
		},
		"public": func(t *testing.T, m *manifest.Manifest) {
			registryEndpoint(t, m, "GET", "/v2/token").Public = manifest.PublicMetadata
		},
		"served twice": func(t *testing.T, m *manifest.Manifest) {
			m.Endpoints = append(m.Endpoints, *registryEndpoint(t, m, "GET", "/v2/<name>/tags/list"))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := registryManifest(t)
			mutate(t, m)
			if len(m.Check()) == 0 {
				t.Fatal("expected issues")
			}
		})
	}
}

// contracts/vectors/manifest-registry.json: Parse accepts each case exactly when it's valid, and
// an invalid case's issue is among Check's.
func TestRegistryVectors(t *testing.T) {
	var v registryVectors
	vectors.Load(t, "manifest-registry.json", &v)
	if len(v.Cases) < 30 {
		t.Fatalf("only %d cases", len(v.Cases))
	}
	for _, c := range v.Cases {
		t.Run(c.Name, func(t *testing.T) {
			data := c.applyTo(t, v.Manifest)
			if _, err := manifest.Parse(data); (err == nil) != c.Valid {
				t.Fatalf("valid=%t: %v", c.Valid, err)
			}
			if c.Valid {
				return
			}
			var m manifest.Manifest
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatal(err)
			}
			var paths []string
			for _, i := range m.Check() {
				paths = append(paths, i.Path)
			}
			if !slices.Contains(paths, c.Issue) {
				t.Fatalf("issue %s not among %v", c.Issue, paths)
			}
		})
	}
}

func TestRegistryEndpoints(t *testing.T) {
	m := registryManifest(t)
	var got []string
	for _, e := range m.RegistryEndpoints() {
		if !e.OnRegistryHost() || e.OnIngestHost() || e.OnEdgeHost() || e.OnLiveHost() {
			t.Errorf("%+v", e)
		}
		got = append(got, e.Method+" "+e.Path)
	}
	if len(got) != 16 || got[0] != "GET /v2/" || got[15] != "GET /v2/<name>/tags/list" {
		t.Fatalf("%v", got)
	}
	for _, e := range m.RegistryEndpoints() {
		if !slices.Contains(manifest.RegistryPaths, e.Path) {
			t.Errorf("%s isn't in RegistryPaths", e.Path)
		}
	}
	if e, _ := m.Endpoint("GET", "/health"); e.OnRegistryHost() {
		t.Error("/health is on the registry host")
	}
}

func TestCheckRegistryRoutes(t *testing.T) {
	m := registryManifest(t)
	var served []manifest.RegistryRoute
	for _, e := range m.RegistryEndpoints() {
		served = append(served, manifest.RegistryRoute{Method: e.Method, Path: e.Path})
	}
	if err := m.CheckRegistryRoutes(served); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckRegistryRoutes(served[1:]); err == nil || !strings.Contains(err.Error(), "GET /v2/ is in the manifest but isn't served") {
		t.Fatalf("missing route: %v", err)
	}
	if err := m.CheckRegistryRoutes(append(slices.Clone(served), manifest.RegistryRoute{Method: "GET", Path: "/v2/_catalog"})); err == nil || !strings.Contains(err.Error(), "GET /v2/_catalog is served but isn't a registry endpoint in the manifest") {
		t.Fatalf("extra route: %v", err)
	}
	if err := m.CheckRegistryRoutes(append(slices.Clone(served), served[0])); err == nil || !strings.Contains(err.Error(), "GET /v2/ is served twice") {
		t.Fatalf("duplicate: %v", err)
	}
	// An API-host endpoint isn't a registry route.
	if err := m.CheckRegistryRoutes(append(slices.Clone(served), manifest.RegistryRoute{Method: "GET", Path: "/health"})); err == nil || !strings.Contains(err.Error(), "isn't a registry endpoint") {
		t.Fatalf("API route: %v", err)
	}
}

// apiHostIssue reports whether Check finds an api.hostname issue on m.
func apiHostIssue(m *manifest.Manifest) bool {
	for _, i := range m.Check() {
		if i.Path == "api.hostname" {
			return true
		}
	}
	return false
}

// api.hostname: <namespace>.ezghcloud.com, or <namespace>-api.ezghcloud.com, which any product
// may choose and a product with a registry host must use (contracts/registration.md).
func TestAPIHostnameMayUseTheAPIForm(t *testing.T) {
	for _, tc := range []struct {
		host  string
		reg   bool
		valid bool
	}{
		{"workflows.ezghcloud.com", false, true},
		{"workflows-api.ezghcloud.com", false, true},
		{"workflows-x.ezghcloud.com", false, false},
		{"registry-api.ezghcloud.com", true, true},
		{"registry.ezghcloud.com", true, false},
	} {
		ns := strings.SplitN(strings.TrimSuffix(tc.host, ".ezghcloud.com"), "-", 2)[0]
		m := &manifest.Manifest{Namespace: ns, API: manifest.API{Hostname: tc.host, Port: 6100}}
		if tc.reg {
			m.Registry = &manifest.RegistryHost{Domain: ns + ".ezghcloud.com", Port: 5801}
		}
		if got := !apiHostIssue(m); got != tc.valid {
			t.Errorf("%s (registry %v): allowed %v, want %v", tc.host, tc.reg, got, tc.valid)
		}
	}
}

// contracts/vectors/manifest-api-host.json, through manifest.Check (scripts/check.mjs runs the
// same cases through scripts/lib/manifest.mjs).
func TestAPIHostVectors(t *testing.T) {
	var file struct {
		Cases []struct {
			Name, Namespace, Hostname string
			Registry, Valid           bool
		} `json:"cases"`
	}
	vectors.Load(t, "manifest-api-host.json", &file)
	if len(file.Cases) < 6 {
		t.Fatalf("only %d cases", len(file.Cases))
	}
	for _, c := range file.Cases {
		m := &manifest.Manifest{Namespace: c.Namespace, API: manifest.API{Hostname: c.Hostname, Port: 6100}}
		if c.Registry {
			m.Registry = &manifest.RegistryHost{Domain: c.Namespace + ".ezghcloud.com", Port: 5801}
		}
		if got := !apiHostIssue(m); got != c.Valid {
			t.Errorf("%s: allowed %v, want %v", c.Name, got, c.Valid)
		}
	}
}

// contracts/vectors/manifest-invocation-token.json, on the Workflows excerpt: invocation-token
// endpoints on the live host.
func TestInvocationTokenVectors(t *testing.T) {
	runManifestCases(t, "manifest-invocation-token.json", "examples/workflows.service-manifest.json", 20)
}

func TestInvocationTokenHelpers(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/workflows.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, ok := m.Endpoint("POST", "/v1/invocations/checkpoint")
	if !ok || !e.InvocationToken() || e.ComponentName() != manifest.ComponentLive || !strings.HasPrefix(e.Path, manifest.InvocationPathPrefix) {
		t.Fatalf("%+v", e)
	}
	if e, _ := m.Endpoint("GET", "/v1/organizations/:orgId/projects/:projectId/apps"); e.InvocationToken() {
		t.Fatalf("%+v", e)
	}
}

// contracts/vectors/manifest-receipt-handle.json, on the Queues excerpt: receipt-handle
// endpoints on the live host.
func TestReceiptHandleVectors(t *testing.T) {
	runManifestCases(t, "manifest-receipt-handle.json", "examples/queues.service-manifest.json", 20)
}

func TestReceiptHandleHelpers(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/queues.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/receipts/ack", "/v1/receipts/visibility"} {
		e, ok := m.Endpoint("POST", path)
		if !ok || !e.ReceiptHandle() || e.InvocationToken() || e.Auth != manifest.AuthReceiptHandle || e.ComponentName() != manifest.ComponentLive || !strings.HasPrefix(e.Path, manifest.ReceiptPathPrefix) {
			t.Fatalf("%+v", e)
		}
	}
	if e, _ := m.Endpoint("POST", "/v1/organizations/:orgId/projects/:projectId/queues/:queueId/messages/receive"); e.ReceiptHandle() {
		t.Fatalf("%+v", e)
	}
	// The live endpoints are the receive poll and the two receipt endpoints.
	if got := len(m.LiveEndpoints()); got != 3 {
		t.Fatalf("%d live endpoints", got)
	}
}

// contracts/vectors/manifest-public-callback.json, on the Workflows excerpt.
func TestPublicCallbackVectors(t *testing.T) {
	runManifestCases(t, "manifest-public-callback.json", "examples/workflows.service-manifest.json", 15)
}

// contracts/vectors/manifest-trail-record.json, on the Workflows excerpt.
func TestTrailRecordVectors(t *testing.T) {
	runManifestCases(t, "manifest-trail-record.json", "examples/workflows.service-manifest.json", 12)
}

// Queues' message operations are in the run-operation volume exception (contracts/trails.md):
// SendMessages, an API-host write with an Idempotency-Key, and ReceiveMessages, a live-host long
// poll, both record refusals only; PeekMessages is a recorded Data event.
func TestQueuesRecordsRefusals(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/queues.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	send, _ := m.Endpoint("POST", "/v1/organizations/:orgId/projects/:projectId/queues/:queueId/messages")
	receive, _ := m.Endpoint("POST", "/v1/organizations/:orgId/projects/:projectId/queues/:queueId/messages/receive")
	peek, _ := m.Endpoint("GET", "/v1/organizations/:orgId/projects/:projectId/queues/:queueId/messages")
	if send.Trail == nil || send.Trail.EventName != "SendMessages" || send.Trail.Record != manifest.RecordRefusals || !send.IdempotencyKey || send.OnLiveHost() {
		t.Fatalf("send: %+v %+v", send, send.Trail)
	}
	if receive.Trail == nil || receive.Trail.EventName != "ReceiveMessages" || receive.Trail.Record != manifest.RecordRefusals || !receive.OnLiveHost() || !receive.Computation() {
		t.Fatalf("receive: %+v %+v", receive, receive.Trail)
	}
	if peek.Trail == nil || peek.Trail.EventName != "PeekMessages" || peek.Trail.EventCategory != manifest.CategoryData || peek.Trail.Record != "" {
		t.Fatalf("peek: %+v %+v", peek, peek.Trail)
	}
}

// A trail's record survives a JSON round trip, in both forms.
func TestTrailRecordRoundTrip(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/workflows.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := m.Endpoint("POST", "/v1/organizations/:orgId/projects/:projectId/runs")
	if e.Trail == nil || e.Trail.Record != manifest.RecordRefusals {
		t.Fatalf("%+v", e.Trail)
	}
	b, err := json.Marshal(e.Trail)
	if err != nil || !strings.Contains(string(b), `"record":"refusals"`) {
		t.Fatalf("%s %v", b, err)
	}
	if c, _ := m.Endpoint("POST", "/v1/organizations/:orgId/projects/:projectId/runs/:runId/cancel"); c.Trail.Record != "" {
		t.Fatalf("%+v", c.Trail)
	}
}

// oip is org-api's OIDC providers' prefix (contracts/resources.md): no product registers it.
func TestOIDCProviderPrefixIsReserved(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	r := m.Registration()
	r.ResourceTypes = append(slices.Clone(r.ResourceTypes), manifest.ResourceType{Name: "gadget", Prefix: "oip", Parent: "project", Regional: true, Description: "x"})
	var got []string
	for _, i := range manifest.RegistrationIssues(r) {
		got = append(got, i.Path+": "+i.Message)
	}
	if want := []string{"resourceTypes.1.prefix: oip is reserved"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// contracts/vectors/manifest-delegation.json cases, on the blockstore excerpt: Parse accepts each
// case exactly when it's valid.
func TestDelegationVectors(t *testing.T) {
	var file struct {
		Cases []manifestCase `json:"cases"`
	}
	vectors.Load(t, "manifest-delegation.json", &file)
	if len(file.Cases) < 25 {
		t.Fatalf("only %d cases", len(file.Cases))
	}
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if _, err := manifest.Parse(c.apply(t, "examples/blockstore.service-manifest.json")); (err == nil) != c.Valid {
				t.Fatalf("valid=%t: %v", c.Valid, err)
			}
		})
	}
}

// contracts/vectors/manifest-delegation.json registrations: the registration carries delegation
// exactly as the manifest has it, and only when present (org-api stores and digests it).
func TestDelegationRegistration(t *testing.T) {
	var file struct {
		Registrations []struct {
			Name       string          `json:"name"`
			Delegation json.RawMessage `json:"delegation"`
		} `json:"registrations"`
	}
	vectors.Load(t, "manifest-delegation.json", &file)
	base := vectors.File(t, "examples/blockstore.service-manifest.json")
	m, err := manifest.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := m.Delegation.Entry("compute"); !ok || !e.Covers("blockstore.volumes.attach") || e.Covers("blockstore.volumes.get") {
		t.Errorf("Entry(compute) = %+v, %t", e, ok)
	}
	if _, ok := m.Delegation.Entry("network"); ok {
		t.Error("Entry(network) found")
	}
	if !m.Endpoints[4].Delegated() || m.Endpoints[3].Delegated() {
		t.Error("Delegated()")
	}
	for _, c := range file.Registrations {
		t.Run(c.Name, func(t *testing.T) {
			var raw map[string]any
			if err := json.Unmarshal(base, &raw); err != nil {
				t.Fatal(err)
			}
			var d any
			if err := json.Unmarshal(c.Delegation, &d); err != nil {
				t.Fatal(err)
			}
			if d == nil {
				delete(raw, "delegation")
				delete(raw, "endpoints") // without delegated endpoints
				raw["endpoints"] = []any{map[string]any{"method": "GET", "path": "/openapi.json", "auth": "none", "trail": nil}}
			} else {
				raw["delegation"] = d
			}
			data, _ := json.Marshal(raw)
			m, err := manifest.Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(m.Registration())
			var reg map[string]any
			_ = json.Unmarshal(got, &reg)
			sent, present := reg["delegation"]
			if (d == nil) == present {
				t.Fatalf("delegation present=%t", present)
			}
			if d != nil {
				// An empty origins is sent left out: the same stored form and digest.
				for _, e := range d.(map[string]any)["callers"].([]any) {
					if o, ok := e.(map[string]any)["origins"].([]any); ok && len(o) == 0 {
						delete(e.(map[string]any), "origins")
					}
				}
				a, _ := canonical.Marshal(sent)
				b, _ := canonical.Marshal(d)
				if string(a) != string(b) {
					t.Errorf("sent %s, want %s", a, b)
				}
			}
		})
	}
}

// [being built] contracts/vectors/manifest-project-deletion-check.json cases, on the widgets
// example: Parse accepts each case exactly when it's valid, and the registration carries
// projectDeletionCheck exactly as the manifest has it, only when present.
func TestProjectDeletionCheckVectors(t *testing.T) {
	var file struct {
		Cases []manifestCase `json:"cases"`
	}
	var issues struct {
		Cases []struct {
			Issues []string `json:"issues"`
		} `json:"cases"`
	}
	vectors.Load(t, "manifest-project-deletion-check.json", &file)
	vectors.Load(t, "manifest-project-deletion-check.json", &issues)
	for i, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			body := c.apply(t, "examples/widgets.service-manifest.json")
			if _, err := manifest.Parse(body); (err == nil) != c.Valid {
				t.Fatalf("valid=%t: %v", c.Valid, err)
			}
			// The rules' issues, path for path, where the vectors name them.
			if want := issues.Cases[i].Issues; want != nil {
				var m manifest.Manifest
				if err := json.Unmarshal(body, &m); err != nil {
					t.Fatal(err)
				}
				got := []string{}
				for _, issue := range m.Check() {
					got = append(got, issue.Path)
				}
				if !slices.Equal(got, want) {
					t.Errorf("issues %v, want %v", got, want)
				}
			}
		})
	}
	declaring, err := manifest.Parse(file.Cases[1].apply(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if r := declaring.Registration(); r.ProjectDeletionCheck == nil || !*r.ProjectDeletionCheck {
		t.Errorf("registration %+v", r.ProjectDeletionCheck)
	}
	widgets, _ := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if body, _ := json.Marshal(widgets.Registration()); strings.Contains(string(body), "projectDeletionCheck") {
		t.Errorf("widgets sent projectDeletionCheck: %s", body)
	}
}
