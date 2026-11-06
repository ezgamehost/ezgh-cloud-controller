// Package resource reads and writes resource names (contracts/resources.md):
//
//	ezgh:<region>:<slug>[:<slug>]...[/<path>]
//
// It mirrors org-api's src/authz/resources.ts, the reference parser, and
// contracts/vectors/resource-names.json checks it.
package resource

import (
	"slices"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
)

// Partition is the first part of every name.
const Partition = "ezgh"

// Name is a parsed resource name.
type Name struct {
	// Region is "" for what isn't regional (organizations, projects).
	Region string
	// Chain is the organization's slug, then each parent's, then the resource's own.
	Chain []string
	// Path is everything after the first "/", when the name has one (HasPath).
	Path    string
	HasPath bool
}

// Parse reads name, accepting only the configured regions (and "" for global names). Slugs never
// contain ":" or "/", so everything after the first "/" belongs to the resource.
func Parse(name string, regions []string) (Name, bool) {
	partition, rest, ok := strings.Cut(name, ":")
	if !ok || partition != Partition {
		return Name{}, false
	}
	region, rest, ok := strings.Cut(rest, ":")
	if !ok || (region != "" && !slices.Contains(regions, region)) {
		return Name{}, false
	}
	chainText, path, hasPath := strings.Cut(rest, "/")
	chain := strings.Split(chainText, ":")
	for _, slug := range chain {
		if !ids.IsSlug(slug) {
			return Name{}, false
		}
	}
	if !strings.HasPrefix(chain[0], "org_") {
		return Name{}, false
	}
	return Name{Region: region, Chain: chain, Path: path, HasPath: hasPath}, true
}

// String writes the name back.
func (n Name) String() string {
	s := Partition + ":" + n.Region + ":" + strings.Join(n.Chain, ":")
	if n.HasPath {
		s += "/" + n.Path
	}
	return s
}

// TypePrefix is the type of the resource the name identifies: its last slug's prefix (wgt).
func (n Name) TypePrefix() string { return n.Chain[len(n.Chain)-1][:3] }

// ResourceID is the resource's own slug, the last in the chain (wgt_4k2m9x0a7qpe). Usage event
// IDs start with it.
func (n Name) ResourceID() string { return n.Chain[len(n.Chain)-1] }

// Organization is the name of an organization: ezgh::org_….
func Organization(orgSlug string) string { return Partition + "::" + orgSlug }

// Project is the name of a project: ezgh::org_…:prj_….
func Project(orgSlug, projectSlug string) string {
	return Partition + "::" + orgSlug + ":" + projectSlug
}

// InProject is the name of a regional resource in a project: ezgh:<region>:org_…:prj_…:<slug>.
func InProject(region, orgSlug, projectSlug, slug string) string {
	return Partition + ":" + region + ":" + orgSlug + ":" + projectSlug + ":" + slug
}

// InProjectPath is a project resource's name with a path after its own slug, for a resource
// whose fixed name is its path: a registry repository, ezgh:<region>:org_…:prj_…:rep_…/team-a/api
// (contracts/resources.md), so that policies can match by name (rep_*/team-a/*).
func InProjectPath(region, orgSlug, projectSlug, slug, path string) string {
	return Name{Region: region, Chain: []string{orgSlug, projectSlug, slug}, Path: path, HasPath: true}.String()
}

// LastSlug is the last slug of a name, without parsing it fully ("" if there's none).
func LastSlug(name string) string {
	chain, _, _ := strings.Cut(name, "/")
	parts := strings.Split(chain, ":")
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-1]
}

// TypeName is how Trails and operations show a resource type: its camelCase name with a capital
// first letter (widget → Widget).
func TypeName(name string) string {
	if name == "" {
		return ""
	}
	return strings.ToUpper(name[:1]) + name[1:]
}
