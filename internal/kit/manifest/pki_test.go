package manifest_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// pkiManifest is contracts/vectors/manifest-pki.json's Web Certificates excerpt, parsed.
func pkiManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	var v registryVectors
	vectors.Load(t, "manifest-pki.json", &v)
	m, err := manifest.Parse(v.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// contracts/vectors/manifest-pki.json: Parse accepts each case exactly when it's valid, and an
// invalid case's issue is among Check's (scripts/checks/pki-endpoints.mjs runs the same cases).
func TestPKIVectors(t *testing.T) {
	var v registryVectors
	vectors.Load(t, "manifest-pki.json", &v)
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

func TestPKIHost(t *testing.T) {
	m := pkiManifest(t)
	if m.PKI == nil || m.PKI.Hostname != manifest.PKIHostname || m.PKI.Port != 5705 || m.Namespace != manifest.PKINamespace {
		t.Fatalf("%+v", m.PKI)
	}
	// The PKI host doesn't move the API host, and the eventSource stays the namespace's.
	if m.API.Hostname != "certificates.ezghcloud.com" || m.EventSource() != "certificates.ezghcloud.com" {
		t.Fatalf("%s %s", m.API.Hostname, m.EventSource())
	}
	var got []string
	for _, e := range m.PKIEndpoints() {
		if !e.OnPKIHost() || e.OnDataHost() || e.OnIngestHost() || e.OnRegistryHost() || e.OnEdgeHost() || e.OnLiveHost() || e.Auth != "none" || e.Trail != nil || e.Builtin() {
			t.Errorf("%+v", e)
		}
		got = append(got, e.Method+" "+e.Path)
	}
	want := []string{
		"GET /ca/:file", "GET /crl/:file", "POST /ocsp/:caId", "GET /ocsp/:caId/:request",
		"GET /acme/:caId/directory", "HEAD /acme/:caId/directory", "GET /acme/:caId/new-nonce", "HEAD /acme/:caId/new-nonce",
		"POST /acme/:caId/new-account", "POST /acme/:caId/new-order", "POST /acme/:caId/order/:id/finalize",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if e, _ := m.Endpoint("GET", "/health"); e.OnPKIHost() {
		t.Error("/health is on the PKI host")
	}
}

// The host's two parts: ACME under /acme/, and revocation under /ca/, /crl/ and /ocsp/.
func TestPKIPart(t *testing.T) {
	for path, want := range map[string]string{
		"/acme/:caId/new-order": manifest.PKIPartACME,
		"/ca/:file":             manifest.PKIPartRevocation,
		"/crl/:file":            manifest.PKIPartRevocation,
		"/ocsp/:caId":           manifest.PKIPartRevocation,
		"/ocsp/:caId/:request":  manifest.PKIPartRevocation,
		"/certs/:file":          "",
		"/acme":                 "",
	} {
		if got := manifest.PKIPart(path); got != want {
			t.Errorf("PKIPart(%s) = %q, want %q", path, got, want)
		}
	}
}

// Only namespace certificates declares the PKI host, and only as pki.ezghcloud.com, whatever its
// API host.
func TestPKIHostIsReserved(t *testing.T) {
	m := pkiManifest(t)
	m.Namespace = "widgets"
	m.API.Hostname = "widgets.ezghcloud.com"
	m.Actions = []manifest.Action{{ID: "widgets.widgets.list", Description: "x", Resource: "project", AccessLevel: "list", ManagedPolicies: []string{"ReadOnlyAccess"}}}
	if !hasIssue(m, "pki") {
		t.Error("widgets declared the PKI host")
	}
	m = pkiManifest(t)
	m.API.Hostname = "certificates-api.ezghcloud.com"
	if issues := m.Check(); len(issues) != 0 {
		t.Errorf("certificates-api beside the PKI host: %v", issues)
	}
}

func hasIssue(m *manifest.Manifest, path string) bool {
	return slices.ContainsFunc(m.Check(), func(i manifest.Issue) bool { return i.Path == path })
}

func TestCheckPKIRoutes(t *testing.T) {
	m := pkiManifest(t)
	var all, acme, revocation []manifest.PKIRoute
	for _, e := range m.PKIEndpoints() {
		r := manifest.PKIRoute{Method: e.Method, Path: e.Path}
		all = append(all, r)
		if manifest.PKIPart(e.Path) == manifest.PKIPartACME {
			acme = append(acme, r)
		} else {
			revocation = append(revocation, r)
		}
	}
	// One server for the whole host, or one per part.
	for name, served := range map[string][]manifest.PKIRoute{"all": all, "acme": acme, "revocation": revocation} {
		if err := m.CheckPKIRoutes(served); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A part a server serves is served whole.
	if err := m.CheckPKIRoutes(acme[1:]); err == nil || !strings.Contains(err.Error(), "GET /acme/:caId/directory is in the manifest but isn't served") {
		t.Fatalf("missing ACME route: %v", err)
	}
	if err := m.CheckPKIRoutes(append(slices.Clone(acme), revocation[1:]...)); err == nil || !strings.Contains(err.Error(), "GET /ca/:file is in the manifest but isn't served") {
		t.Fatalf("missing revocation route: %v", err)
	}
	if err := m.CheckPKIRoutes(append(slices.Clone(revocation), manifest.PKIRoute{Method: "GET", Path: "/crl/:caId/delta"})); err == nil || !strings.Contains(err.Error(), "GET /crl/:caId/delta is served but isn't a PKI endpoint in the manifest") {
		t.Fatalf("extra route: %v", err)
	}
	if err := m.CheckPKIRoutes(append(slices.Clone(revocation), revocation[0])); err == nil || !strings.Contains(err.Error(), "GET /ca/:file is served twice") {
		t.Fatalf("duplicate: %v", err)
	}
	// An API-host endpoint isn't a PKI route, and serving nothing serves no part.
	if err := m.CheckPKIRoutes(append(slices.Clone(revocation), manifest.PKIRoute{Method: "GET", Path: "/openapi.json"})); err == nil || !strings.Contains(err.Error(), "isn't a PKI endpoint") {
		t.Fatalf("API route: %v", err)
	}
	if err := m.CheckPKIRoutes(nil); err == nil || !strings.Contains(err.Error(), "serves no PKI route") {
		t.Fatalf("nothing served: %v", err)
	}
}

// pki is the reserved PKI host's name: no namespace takes it.
func TestPKINamespaceIsReserved(t *testing.T) {
	r := manifest.Registration{
		RegistrationVersion: 1, Namespace: "pki", DisplayName: "PKI", Description: "x",
		ResourceTypes: []manifest.ResourceType{}, ManagedPolicies: []manifest.ManagedPolicy{},
		Actions: []manifest.Action{{ID: "pki.things.list", Description: "x", Resource: "project", AccessLevel: "list", ManagedPolicies: []string{"ReadOnlyAccess"}}},
	}
	if !slices.ContainsFunc(manifest.RegistrationIssues(r), func(i manifest.Issue) bool { return i.Path == "namespace" }) {
		t.Fatal("namespace pki was accepted")
	}
}
