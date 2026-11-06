package resource_test

import (
	"regexp"
	"slices"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resource"
)

// contracts/vectors/resource-names.json.
func TestResourceNameVectors(t *testing.T) {
	var file struct {
		Regions []string `json:"regions"`
		Valid   []struct {
			Name     string `json:"name"`
			Expected struct {
				Region     string   `json:"region"`
				Chain      []string `json:"chain"`
				Path       *string  `json:"path"`
				TypePrefix string   `json:"typePrefix"`
				ResourceID string   `json:"resourceId"`
			} `json:"expected"`
		} `json:"valid"`
		Invalid []struct {
			Reason, Name string
		} `json:"invalid"`
	}
	vectors.Load(t, "resource-names.json", &file)
	for _, c := range file.Valid {
		n, ok := resource.Parse(c.Name, file.Regions)
		if !ok {
			t.Errorf("refused %s", c.Name)
			continue
		}
		e := c.Expected
		if n.Region != e.Region || !slices.Equal(n.Chain, e.Chain) || n.TypePrefix() != e.TypePrefix || n.ResourceID() != e.ResourceID {
			t.Errorf("%s parsed as %+v", c.Name, n)
		}
		if (e.Path != nil) != n.HasPath || (e.Path != nil && *e.Path != n.Path) {
			t.Errorf("%s: path %q (%v), want %v", c.Name, n.Path, n.HasPath, e.Path)
		}
		if n.String() != c.Name {
			t.Errorf("%s written back as %s", c.Name, n.String())
		}
	}
	for _, c := range file.Invalid {
		if _, ok := resource.Parse(c.Name, file.Regions); ok {
			t.Errorf("parsed %q, but %s", c.Name, c.Reason)
		}
	}
}

func TestBuilders(t *testing.T) {
	if got := resource.InProject("us-west-1", "org_k3f9a0x2m7qp", "prj_g7h8i9j0k1l2", "wgt_4k2m9x0a7qpe"); got != "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:wgt_4k2m9x0a7qpe" {
		t.Error(got)
	}
	if resource.Project("org_k3f9a0x2m7qp", "prj_g7h8i9j0k1l2") != "ezgh::org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2" {
		t.Error("project")
	}
	if resource.TypeName("widget") != "Widget" {
		t.Error("type name")
	}
}

// A registry repository's name carries its fixed <scope>/<image> name as its path.
func TestInProjectPath(t *testing.T) {
	got := resource.InProjectPath("us-west-1", "org_k3f9a0x2m7qp", "prj_g7h8i9j0k1l2", "rep_7m2k9q4x0a1b", "team-a/api")
	want := "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:rep_7m2k9q4x0a1b/team-a/api"
	if got != want {
		t.Fatalf("got %s", got)
	}
	n, ok := resource.Parse(got, []string{"us-west-1"})
	if !ok || n.TypePrefix() != "rep" || n.ResourceID() != "rep_7m2k9q4x0a1b" || !n.HasPath || n.Path != "team-a/api" {
		t.Fatalf("parse %+v %v", n, ok)
	}
}

// A Workflows run action is checked on the app's name with the workflow as its path
// (contracts/resources.md, "Resource names"; vectors/workflows-resource-names.json).
func TestWorkflowPath(t *testing.T) {
	got := resource.InProjectPath("us-west-1", "org_k3f9a0x2m7qp", "prj_g7h8i9j0k1l2", "wfa_7m2k9q4x0a1b", "billing-invoice")
	want := "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:wfa_7m2k9q4x0a1b/billing-invoice"
	if got != want {
		t.Fatalf("got %s", got)
	}
	n, ok := resource.Parse(got, []string{"us-west-1"})
	if !ok || n.TypePrefix() != "wfa" || n.ResourceID() != "wfa_7m2k9q4x0a1b" || !n.HasPath || n.Path != "billing-invoice" {
		t.Fatalf("parse %+v %v", n, ok)
	}
	var file struct {
		Cases []struct {
			Pattern, Resource string
			Matches           bool
		} `json:"cases"`
	}
	vectors.Load(t, "workflows-resource-names.json", &file)
	if len(file.Cases) < 10 {
		t.Fatalf("only %d cases", len(file.Cases))
	}
	for _, c := range file.Cases {
		if n, ok := resource.Parse(c.Resource, []string{"us-west-1"}); !ok || n.TypePrefix() != "wfa" || !n.HasPath {
			t.Errorf("%s doesn't parse as an app name with a workflow path", c.Resource)
		}
	}
}

// [being built] contracts/vectors/flags-resource-names.json: Feature Flags config actions are
// checked on the environment's name with the flag's key as the path, fen_<12>/<flagKey>. Each
// environment case is built with InProjectPath and round-trips through Parse; every case matches
// with org-api's glob (manifest.MatchesActionPattern is the same algorithm) as the vector says.
func TestFlagsConfigPath(t *testing.T) {
	got := resource.InProjectPath("us-west-1", "org_k3f9a0x2m7qp", "prj_g7h8i9j0k1l2", "fen_7m2k9q4x0a1b", "new-checkout")
	if want := "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:fen_7m2k9q4x0a1b/new-checkout"; got != want {
		t.Fatalf("got %s", got)
	}
	var file struct {
		FlagKey string `json:"flagKey"`
		Cases   []struct {
			Pattern, Resource string
			Matches           bool
		} `json:"cases"`
		RefusedKeys []string `json:"refusedKeys"`
	}
	vectors.Load(t, "flags-resource-names.json", &file)
	if len(file.Cases) < 10 {
		t.Fatalf("only %d cases", len(file.Cases))
	}
	flagKey := regexp.MustCompile(file.FlagKey)
	for _, c := range file.Cases {
		n, ok := resource.Parse(c.Resource, []string{"us-west-1"})
		if !ok {
			t.Fatalf("%s doesn't parse", c.Resource)
		}
		if n.TypePrefix() == "fen" {
			if !n.HasPath || !flagKey.MatchString(n.Path) {
				t.Errorf("%s isn't an environment name with a flag-key path", c.Resource)
			}
			built := resource.InProjectPath(n.Region, n.Chain[0], n.Chain[1], n.ResourceID(), n.Path)
			if built != c.Resource || n.String() != c.Resource {
				t.Errorf("%s round-trips as %s / %s", c.Resource, built, n.String())
			}
		} else if n.HasPath || c.Matches {
			t.Errorf("%s: only environment names carry a flag key", c.Resource)
		}
		if got := manifest.MatchesActionPattern(c.Pattern, c.Resource); got != c.Matches {
			t.Errorf("%s against %s: matches %v, want %v", c.Pattern, c.Resource, got, c.Matches)
		}
	}
	for _, key := range file.RefusedKeys {
		if flagKey.MatchString(key) {
			t.Errorf("flag key %q is accepted", key)
		}
	}
}
