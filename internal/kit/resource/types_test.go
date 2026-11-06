package resource_test

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resource"
)

// contracts/vectors/resource-types.json: every registered prefix types as the contract says, in
// any product's events, and anything else is Unknown.
func TestResourceTypeVectors(t *testing.T) {
	var file struct {
		Types []struct {
			Prefix, Type string
		} `json:"types"`
		NotTyped []string `json:"notTyped"`
		Unknown  string   `json:"unknown"`
	}
	vectors.Load(t, "resource-types.json", &file)
	if len(file.Types) == 0 || file.Unknown != resource.Unknown {
		t.Fatalf("vector: %+v", file)
	}
	seen := map[string]bool{}
	for _, c := range file.Types {
		seen[c.Prefix] = true
		got, ok := resource.RegisteredType(c.Prefix)
		if !ok || got != c.Type {
			t.Errorf("%s: %q %v, want %q", c.Prefix, got, ok, c.Type)
		}
		name := "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:" + c.Prefix + "_4k2m9x0a7qpe"
		if c.Prefix == "org" {
			name = "ezgh::org_k3f9a0x2m7qp"
		} else if c.Prefix == "prj" {
			name = "ezgh::org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2"
		}
		if got := resource.TypeOf(name); got != c.Type {
			t.Errorf("TypeOf(%s) = %q, want %q", name, got, c.Type)
		}
	}
	for _, p := range file.NotTyped {
		if _, ok := resource.RegisteredType(p); ok {
			t.Errorf("%s is typed, but the vector says it isn't", p)
		}
		if got := resource.TypeOf("ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:" + p + "_4k2m9x0a7qpe"); got != resource.Unknown {
			t.Errorf("%s types as %q", p, got)
		}
	}
	for _, name := range []string{"", "ezgh::", "not a name", "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:zzz_4k2m9x0a7qpe"} {
		if got := resource.TypeOf(name); got != resource.Unknown {
			t.Errorf("TypeOf(%q) = %q", name, got)
		}
	}
	// The kit's table is exactly the vector's: nothing more.
	for _, p := range resource.RegisteredPrefixes() {
		if !seen[p] {
			t.Errorf("the kit types %s, which the vector doesn't list", p)
		}
	}
}

func TestProjectOf(t *testing.T) {
	for name, want := range map[string]string{
		"ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:wgt_4k2m9x0a7qpe":        "ezgh::org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2",
		"ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:rep_4k2m9x0a7qpe/team-a": "ezgh::org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2",
		"ezgh::org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2":                                  "",
		"ezgh::org_k3f9a0x2m7qp":                                                   "",
		"ezgh:us-west-1:org_k3f9a0x2m7qp:dir_g7h8i9j0k1l2:usr_4k2m9x0a7qpe":        "",
		"nope": "",
	} {
		got, ok := resource.ProjectOf(name)
		if got != want || ok != (want != "") {
			t.Errorf("ProjectOf(%s) = %q %v", name, got, ok)
		}
	}
}

// EZGH Files' types. TypeOf reads a resource name's last slug, so a bare slug is Unknown.
func TestTypeOfFiles(t *testing.T) {
	for prefix, want := range map[string]string{"fsy": "FileSystem", "fmt": "MountTarget", "fap": "AccessPoint"} {
		name := "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:" + prefix + "_abcdefghijkl"
		if got := resource.TypeOf(name); got != want {
			t.Errorf("TypeOf(%s) = %q, want %q", name, got, want)
		}
	}
}

// EZGH Git's repository. TypeOf reads a resource name's last slug, so a bare slug is Unknown; a branch
// path (gtr_<12>/<branch>) doesn't change the type.
func TestTypeOfGit(t *testing.T) {
	for _, name := range []string{
		"ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:gtr_abcdefghijkl",
		"ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:gtr_abcdefghijkl/release/v1",
	} {
		if got := resource.TypeOf(name); got != "GitRepository" {
			t.Errorf("TypeOf(%s) = %q, want GitRepository", name, got)
		}
	}
}
