package manifest_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// gitVectors is contracts/vectors/manifest-git.json: a valid Git excerpt and the cases that change it.
type gitVectors struct {
	Manifest json.RawMessage `json:"manifest"`
	Cases    []struct {
		manifestCase
		Issue string `json:"issue"`
	} `json:"cases"`
}

func gitManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	var v gitVectors
	vectors.Load(t, "manifest-git.json", &v)
	m, err := manifest.Parse(v.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGitHostValid(t *testing.T) {
	m := gitManifest(t)
	if m.Git == nil || m.Git.Domain != "git.ezghcloud.com" || m.Git.Port != 9001 {
		t.Fatalf("%+v", m.Git)
	}
	// The API moves to <namespace>-api; the Trails eventSource stays <namespace>.ezghcloud.com.
	if m.API.Hostname != "git-api.ezghcloud.com" || m.EventSource() != "git.ezghcloud.com" {
		t.Fatalf("%s %s", m.API.Hostname, m.EventSource())
	}
	var got []string
	for _, e := range m.GitEndpoints() {
		if !e.OnGitHost() || !slices.Contains(manifest.GitPaths, e.Path) {
			t.Errorf("%+v", e)
		}
		got = append(got, e.Method+" "+e.Path)
	}
	if len(got) != 6 || got[0] != "GET /<repo>.git/info/refs" {
		t.Fatalf("%v", got)
	}
	// Refusals-only reads are kept: their events aren't read-only (contracts/trails.md, "Git calls").
	for _, route := range [][2]string{{"GET", "/<repo>.git/info/refs"}, {"POST", "/<repo>.git/git-upload-pack"}, {"GET", "/v1/organizations/:orgId/projects/:projectId/repositories/:repoId/contents"}} {
		if e, _ := m.Endpoint(route[0], route[1]); e.ReadOnly() {
			t.Errorf("%s %s is read-only", route[0], route[1])
		}
	}
}

// contracts/vectors/manifest-git.json: Parse accepts each case exactly when it's valid, and an
// invalid case's issue is among Check's.
func TestGitVectors(t *testing.T) {
	var v gitVectors
	vectors.Load(t, "manifest-git.json", &v)
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

func TestCheckGitRoutes(t *testing.T) {
	m := gitManifest(t)
	var served []manifest.GitRoute
	for _, e := range m.GitEndpoints() {
		served = append(served, manifest.GitRoute{Method: e.Method, Path: e.Path})
	}
	if err := m.CheckGitRoutes(served); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckGitRoutes(served[1:]); err == nil || !strings.Contains(err.Error(), "GET /<repo>.git/info/refs is in the manifest but isn't served") {
		t.Fatalf("missing route: %v", err)
	}
	if err := m.CheckGitRoutes(append(slices.Clone(served), manifest.GitRoute{Method: "GET", Path: "/<repo>.git/HEAD"})); err == nil || !strings.Contains(err.Error(), "isn't a Git endpoint in the manifest") {
		t.Fatalf("extra route: %v", err)
	}
	if err := m.CheckGitRoutes(append(slices.Clone(served), served[0])); err == nil || !strings.Contains(err.Error(), "is served twice") {
		t.Fatalf("duplicate: %v", err)
	}
}
