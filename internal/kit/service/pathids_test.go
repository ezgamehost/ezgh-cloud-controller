package service_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

// Prefixed IDs in paths (contracts/api-conventions.md, "IDs in paths"): org_ and prj_ resolve
// through org-api's directory lookup, a declared …Id through the product's table; the handler
// sees UUIDs only.

const (
	pathProjectID   = "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	pathProjectSlug = "prj_g7h8i9j0k1l2"
	otherProj       = "prj_otherorgproj" // a project in another organization
	thingID         = "0199a3d0-2b3c-7d4e-9f50-6b7c8d9e0f1a"
	thingSlug       = "thg_4k2m9x0a7qpe"
	thingPath       = "/v1/organizations/:orgId/projects/:projectId/things/:thingId"
)

// fakeDirectory is org-api's directory lookup over orgID/pathProjectID (and otherProj elsewhere),
// recording each request body.
type fakeDirectory struct {
	mu       sync.Mutex
	requests []map[string]any
	down     bool
}

func (f *fakeDirectory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.mu.Unlock()
	if f.down {
		w.WriteHeader(500)
		return
	}
	orgs, projects := []any{}, []any{}
	for _, s := range body["organizationSlugs"].([]any) {
		if s == orgSlug {
			orgs = append(orgs, map[string]any{"id": orgID, "slug": orgSlug, "state": "active"})
		}
	}
	for _, s := range body["projectSlugs"].([]any) {
		switch s {
		case pathProjectSlug:
			projects = append(projects, map[string]any{"id": pathProjectID, "organizationId": orgID, "slug": pathProjectSlug, "deletedAt": nil})
		case otherProj:
			projects = append(projects, map[string]any{"id": "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a99", "organizationId": "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f99", "slug": otherProj, "deletedAt": nil})
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"organizations": orgs, "projects": projects})
}

func (f *fakeDirectory) lookups() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func newPathIDHarness(t *testing.T) (*harness, *fakeDirectory) {
	dir := &fakeDirectory{}
	h := newHarnessWith(t, nil, []manifest.Endpoint{
		{Method: "GET", Path: thingPath, Auth: "caller", Action: "widgets.things.get", Trail: &manifest.Trail{EventName: "GetThing"}},
		{Method: "GET", Path: "/v1/organizations/:orgId/gadgets/:gadgetId", Auth: "caller", Action: "widgets.gadgets.get", Trail: &manifest.Trail{EventName: "GetGadget"}},
	}, func(s *service.Service) {
		s.PathIDs(map[string]service.PathID{"thingId": {Prefix: "thg", Table: "things"}})
		answer := func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			project := ""
			if c.ProjectID != nil {
				project = *c.ProjectID
			}
			c.JSON(200, map[string]any{"orgId": c.Param("orgId"), "projectId": c.Param("projectId"), "thingId": c.Param("thingId"), "organizationId": c.OrganizationID, "project": project})
			return nil
		}
		s.Handle("GET", thingPath, answer)
		s.Handle("GET", "/v1/organizations/:orgId/gadgets/:gadgetId", answer)
	})
	h.org.directory = dir
	if _, err := h.db.Pool.Exec(context.Background(), `CREATE TABLE things (id uuid PRIMARY KEY, slug text NOT NULL UNIQUE);
		INSERT INTO things VALUES ('`+thingID+`', '`+thingSlug+`')`); err != nil {
		t.Fatal(err)
	}
	return h, dir
}

func TestPrefixedPathIDs(t *testing.T) {
	h, dir := newPathIDHarness(t)
	member := map[string]string{"Cookie": "better-auth.session_token=member"}
	path := func(org, project, thing string) string {
		return "/v1/organizations/" + org + "/projects/" + project + "/things/" + thing
	}
	want := map[string]any{"orgId": orgID, "projectId": pathProjectID, "thingId": thingID, "organizationId": orgID, "project": pathProjectID}

	// UUIDs: unchanged, and no lookup.
	if r := h.do(t, "GET", path(orgID, pathProjectID, thingID), "", member); r.status != 200 || !sameJSON(r.body, want) {
		t.Fatalf("UUIDs: %d %v", r.status, r.body)
	}
	if n := len(dir.lookups()); n != 0 {
		t.Fatalf("%d lookups for UUIDs", n)
	}

	// Prefixed IDs: one lookup for both, and the handler sees UUIDs.
	if r := h.do(t, "GET", path(orgSlug, pathProjectSlug, thingSlug), "", member); r.status != 200 || !sameJSON(r.body, want) {
		t.Fatalf("prefixed IDs: %d %v", r.status, r.body)
	}
	if got := dir.lookups(); len(got) != 1 || !sameJSON(got[0], map[string]any{"organizationIds": []any{}, "projectIds": []any{}, "organizationSlugs": []any{orgSlug}, "projectSlugs": []any{pathProjectSlug}}) {
		t.Fatalf("lookups %v", got)
	}
	// Mixed: a UUID organization and a prefixed project.
	if r := h.do(t, "GET", path(orgID, pathProjectSlug, thingID), "", member); r.status != 200 || !sameJSON(r.body, want) {
		t.Fatalf("mixed: %d %v", r.status, r.body)
	}

	notFound := []struct{ name, path, message string }{
		{"unknown organization", path("org_aaaaaaaaaaaa", pathProjectID, thingID), "Organization not found"},
		{"unknown project", path(orgSlug, "prj_aaaaaaaaaaaa", thingID), "Project not found"},
		{"a project in another organization", path(orgSlug, otherProj, thingID), "Project not found"},
		{"a project in another organization, UUID organization", path(orgID, otherProj, thingID), "Project not found"},
		{"another type's prefix", path(pathProjectSlug, pathProjectID, thingID), "Organization not found"},
		{"malformed", path("org_SHOUTING0000", pathProjectID, thingID), "Organization not found"},
		{"unknown thing", path(orgID, pathProjectID, "thg_aaaaaaaaaaaa"), "Thing not found"},
		{"a thing with another type's prefix", path(orgID, pathProjectID, "wgt_4k2m9x0a7qpe"), "Thing not found"},
		{"an undeclared parameter stays UUID-only", "/v1/organizations/" + orgID + "/gadgets/gad_4k2m9x0a7qpe", "Gadget not found"},
	}
	for _, c := range notFound {
		r := h.do(t, "GET", c.path, "", member)
		if r.status != 404 || r.body["error"].(map[string]any)["message"] != c.message {
			t.Errorf("%s: %d %v", c.name, r.status, r.body)
		}
	}

	// The document says which parameters take prefixed IDs.
	doc := h.do(t, "GET", "/openapi.json", "", nil)
	params, _ := json.Marshal(doc.body["paths"].(map[string]any)["/v1/organizations/{orgId}/projects/{projectId}/things/{thingId}"].(map[string]any)["get"].(map[string]any)["parameters"])
	if !strings.Contains(string(params), `"UUID or prefixed ID (thg_…)"`) || !strings.Contains(string(params), `"UUID or prefixed ID (org_…)"`) {
		t.Errorf("document parameters: %s", params)
	}

	// org-api failing is 503, not 404.
	dir.down = true
	if r := h.do(t, "GET", path(orgSlug, pathProjectID, thingID), "", member); r.status != 503 {
		t.Errorf("lookup down: %d %v", r.status, r.body)
	}
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestPathIDsRefusesBadDeclarations(t *testing.T) {
	for _, c := range []struct {
		param string
		id    service.PathID
	}{
		{"thing", service.PathID{Prefix: "thg", Table: "things"}},
		{"orgId", service.PathID{Prefix: "org", Table: "orgs"}},
		{"thingId", service.PathID{Prefix: "THG", Table: "things"}},
		{"thingId", service.PathID{Prefix: "thg", Table: "things; DROP TABLE x"}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s %+v: accepted", c.param, c.id)
				}
			}()
			service.New(service.Options{Manifest: testManifest()}).PathIDs(map[string]service.PathID{c.param: c.id})
		}()
	}
}
