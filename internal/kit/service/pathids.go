package service

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
)

// PathID says a …Id path parameter also takes its resource's prefixed ID, <Prefix>_<12>: the
// kit finds it in Table's slug column and hands the handler the row's id (its UUID).
type PathID struct {
	Prefix string
	Table  string
}

var (
	prefixForm = regexp.MustCompile(`^[a-z]{3}$`)
	tableForm  = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
)

// PathIDs declares, by path parameter, which …Id parameters also take a prefixed ID
// (contracts/api-conventions.md, "IDs in paths"). orgId and projectId always do (org-api's
// directory lookup resolves them); a parameter not declared stays UUID-only. Call it with the
// routes, before Handler:
//
//	s.PathIDs(map[string]service.PathID{"widgetId": {Prefix: "wgt", Table: "widgets"}})
//
// A bad declaration panics: it's the product's bug.
func (s *Service) PathIDs(declared map[string]PathID) {
	for param, d := range declared {
		if !isIDParam(param) || param == "orgId" || param == "projectId" || !prefixForm.MatchString(d.Prefix) || !tableForm.MatchString(d.Table) {
			panic(fmt.Sprintf("service: PathIDs: %s → %+v isn't a …Id parameter (other than orgId and projectId) with a 3-letter prefix and a table name", param, d))
		}
		if s.pathIDs == nil {
			s.pathIDs = map[string]PathID{}
		}
		s.pathIDs[param] = d
	}
}

// pathPrefix is the prefix a …Id path parameter takes, or "" when it takes UUIDs only.
func (s *Service) pathPrefix(param string) string {
	switch param {
	case "orgId":
		return "org"
	case "projectId":
		return "prj"
	}
	return s.pathIDs[param].Prefix
}

// resolvePathIDs checks every …Id path parameter and turns a prefixed ID into the UUID
// (SetPathValue), so handlers see UUIDs only. Anything else that isn't a UUID, or a prefixed ID
// nothing has, is 404 like an unknown ID, so IDs can't be probed; org-api or the database
// failing is 503. org is the call's organization when the path has none (a delegated call's): a
// prefixed project must be in the path's organization, or in org. Slugs are immutable, so
// nothing needs caching.
func (c *Call) resolvePathIDs(org string) error {
	var orgSlug, projectSlug string
	var others []string
	for _, p := range c.params {
		v := c.R.PathValue(p)
		if !isIDParam(p) || ids.IsUUID(v) {
			continue
		}
		prefix := c.Service.pathPrefix(p)
		if prefix == "" || !ids.IsSlug(v) || !strings.HasPrefix(v, prefix+"_") {
			return httpapi.NotFound(pathLabel(p))
		}
		switch p {
		case "orgId":
			orgSlug = v
		case "projectId":
			projectSlug = v
		default:
			others = append(others, p)
		}
	}
	if orgSlug != "" || projectSlug != "" {
		if err := c.resolveDirectory(org, orgSlug, projectSlug); err != nil {
			return err
		}
	}
	for _, p := range others {
		var id string
		// The table name is checked by PathIDs; the value is a well-formed slug.
		err := c.Service.DB.QueryRowContext(c.Ctx, `SELECT id::text FROM `+c.Service.pathIDs[p].Table+` WHERE slug = $1`, c.R.PathValue(p)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return httpapi.NotFound(pathLabel(p))
		}
		if err != nil {
			return databaseError(err)
		}
		c.R.SetPathValue(p, id)
	}
	return nil
}

// resolveDirectory resolves a prefixed orgId and projectId with one directory lookup.
func (c *Call) resolveDirectory(org, orgSlug, projectSlug string) error {
	var orgSlugs, projectSlugs []string
	if orgSlug != "" {
		orgSlugs = []string{orgSlug}
	}
	if projectSlug != "" {
		projectSlugs = []string{projectSlug}
	}
	d, err := c.Service.OrgAPI.LookupSlugs(c.Ctx, orgSlugs, projectSlugs)
	if err != nil {
		return httpapi.Unavailable("The organization directory can't be reached right now", err)
	}
	if orgSlug != "" {
		found := false
		for _, o := range d.Organizations {
			if o.Slug == orgSlug {
				c.R.SetPathValue("orgId", o.ID)
				found = true
			}
		}
		if !found {
			return httpapi.NotFound(pathLabel("orgId"))
		}
	}
	if v := c.R.PathValue("orgId"); v != "" {
		org = v
	}
	if projectSlug != "" {
		found := false
		for _, p := range d.Projects {
			if p.Slug == projectSlug && (org == "" || p.OrganizationID == org) {
				c.R.SetPathValue("projectId", p.ID)
				found = true
			}
		}
		if !found {
			return httpapi.NotFound(pathLabel("projectId"))
		}
	}
	return nil
}
