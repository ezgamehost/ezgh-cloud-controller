package manifest_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// dataManifest is contracts/vectors/manifest-data.json's Feature Flags excerpt, parsed.
func dataManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	var v registryVectors
	vectors.Load(t, "manifest-data.json", &v)
	m, err := manifest.Parse(v.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// contracts/vectors/manifest-data.json: Parse accepts each case exactly when it's valid, and an
// invalid case's issue is among Check's (scripts/checks/data-endpoints.mjs runs the same cases).
func TestDataVectors(t *testing.T) {
	var v registryVectors
	vectors.Load(t, "manifest-data.json", &v)
	if len(v.Cases) < 25 {
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

func TestDataHost(t *testing.T) {
	m := dataManifest(t)
	if m.Data == nil || m.Data.Hostname != "flags.ezghcloud.com" || m.Data.Port != 6501 {
		t.Fatalf("%+v", m.Data)
	}
	// The API moves to <namespace>-api; the Trails eventSource stays <namespace>.ezghcloud.com.
	if m.API.Hostname != "flags-api.ezghcloud.com" || m.EventSource() != "flags.ezghcloud.com" {
		t.Fatalf("%s %s", m.API.Hostname, m.EventSource())
	}
	var got []string
	for _, e := range m.DataEndpoints() {
		if !e.OnDataHost() || e.OnIngestHost() || e.OnRegistryHost() || e.OnEdgeHost() || e.OnLiveHost() || e.Auth != manifest.AuthProductKey {
			t.Errorf("%+v", e)
		}
		got = append(got, e.Method+" "+e.Path)
	}
	want := []string{"POST /ofrep/v1/evaluate/flags/:key", "GET /v1/sync/snapshot", "POST /v1/sync/stream"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if e, _ := m.Endpoint("GET", "/health"); e.OnDataHost() {
		t.Error("/health is on the data host")
	}
}

// The API host's choices are unchanged without a data host; with one, only <namespace>-api.
func TestDataHostMovesTheAPI(t *testing.T) {
	for _, tc := range []struct {
		host        string
		data, valid bool
	}{
		{"flags.ezghcloud.com", false, true},
		{"flags-api.ezghcloud.com", false, true},
		{"flags-api.ezghcloud.com", true, true},
		{"flags.ezghcloud.com", true, false},
	} {
		m := &manifest.Manifest{Namespace: "flags", API: manifest.API{Hostname: tc.host, Port: 6500}}
		if tc.data {
			m.Data = &manifest.DataHost{Hostname: "flags.ezghcloud.com", Port: 6501}
		}
		if got := !apiHostIssue(m); got != tc.valid {
			t.Errorf("%s (data %v): allowed %v, want %v", tc.host, tc.data, got, tc.valid)
		}
	}
}

func TestCheckDataRoutes(t *testing.T) {
	m := dataManifest(t)
	var served []manifest.DataRoute
	for _, e := range m.DataEndpoints() {
		served = append(served, manifest.DataRoute{Method: e.Method, Path: e.Path})
	}
	if err := m.CheckDataRoutes(served); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckDataRoutes(served[1:]); err == nil || !strings.Contains(err.Error(), "POST /ofrep/v1/evaluate/flags/:key is in the manifest but isn't served") {
		t.Fatalf("missing route: %v", err)
	}
	if err := m.CheckDataRoutes(append(slices.Clone(served), manifest.DataRoute{Method: "POST", Path: "/ofrep/v1/evaluate/flags"})); err == nil || !strings.Contains(err.Error(), "POST /ofrep/v1/evaluate/flags is served but isn't a data endpoint in the manifest") {
		t.Fatalf("extra route: %v", err)
	}
	if err := m.CheckDataRoutes(append(slices.Clone(served), served[0])); err == nil || !strings.Contains(err.Error(), "POST /ofrep/v1/evaluate/flags/:key is served twice") {
		t.Fatalf("duplicate: %v", err)
	}
	// An API-host endpoint isn't a data route.
	if err := m.CheckDataRoutes(append(slices.Clone(served), manifest.DataRoute{Method: "GET", Path: "/openapi.json"})); err == nil || !strings.Contains(err.Error(), "isn't a data endpoint") {
		t.Fatalf("API route: %v", err)
	}
}

// An ingest-host endpoint's method and path on another host break Service.Handler the same way:
// an edge endpoint may be POST /v1/logs (edge paths aren't under the organization).
func TestIngestRouteOnAnotherHost(t *testing.T) {
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	ingest := slices.IndexFunc(m.Endpoints, func(e manifest.Endpoint) bool { return e.OnIngestHost() && e.Method == "POST" && e.Path == "/v1/logs" })
	if ingest < 0 {
		t.Fatal("no POST /v1/logs")
	}
	m.Edge = &manifest.EdgeHost{Domain: "obs.ezgh.app"}
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "POST", Path: "/v1/logs", Host: manifest.HostEdge, Auth: "caller", Action: "obs.datasets.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "EdgeLogs"}})
	var paths []string
	for _, i := range m.Check() {
		paths = append(paths, i.Path)
	}
	if want := fmt.Sprintf("endpoints.%d.path", ingest); !slices.Contains(paths, want) {
		t.Fatalf("issue %s not among %v", want, paths)
	}
	// Another method on the same path is fine.
	m.Endpoints[len(m.Endpoints)-1].Method = "PUT"
	for _, i := range m.Check() {
		if i.Path == fmt.Sprintf("endpoints.%d.path", ingest) {
			t.Fatalf("PUT on the edge host: %v", i)
		}
	}
}
