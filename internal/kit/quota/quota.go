// Package quota is the product side of Quotas (contracts/quotas.md 1.0):
//
//   - Enforcer: each endpoint's quotas, read from the manifest; one check per request to
//     quotas-api after authorization; 429 quota_exceeded for a denied rate, usage or concurrency
//     quota; allocation values for the handler; leases released after the response; failing open
//     (and counting it) when Quotas can't answer.
//   - Client: quotas-api's check, resolve, lease release and renewal.
//   - Reserve and Release: allocation quotas counted exactly, in the product's own quota_usage,
//     in the transaction of the change, against the value Quotas gave (or without a condition,
//     when it didn't answer).
//   - BillingPeriod, ResolveValue, BucketCapacity: the reference computations, for the vectors.
package quota

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// Counter is one allocation counter: a (quota, scope key, region) row of quota_usage.
type Counter struct {
	Quota manifest.Quota
	// OrganizationID owns the scope key: GET /internal/v1/quotas/usage lists by it.
	OrganizationID string
	// ScopeKey is the organization's, project's or resource's ID.
	ScopeKey string
	// Region is the product's REGION for a perRegion quota, "" otherwise.
	Region string
	// ResourceName is the scope's, for a refusal's scopeResourceName ("" when unknown).
	ResourceName string
}

// Reserve takes amount of an allocation quota in tx, the transaction of the change that uses it,
// against limit (the value Quotas gave, from the request's check or from Resolve). Concurrent
// changes serialize on the counter row, so the value is never passed; a refusal is
// 409 quota_exceeded and, once tx rolls back, leaves nothing behind. When Quotas didn't answer
// (limit isn't enforced) it still counts, unconditionally, so usage stays exact for when it's back.
//
// Reserve when the request is accepted, not when the work finishes: an asynchronous create counts
// from its 202.
func Reserve(ctx context.Context, tx *db.Tx, c Counter, amount int64, limit Limit) error {
	if c.Quota.Kind != manifest.Allocation {
		return fmt.Errorf("quota: %s isn't an allocation quota", c.Quota.ID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO quota_usage (quota_id, organization_id, scope_key, region) VALUES ($1, $2, $3, $4)
		ON CONFLICT DO NOTHING`, c.Quota.ID, c.OrganizationID, c.ScopeKey, c.Region); err != nil {
		return err
	}
	if !limit.Enforced {
		_, err := tx.ExecContext(ctx, `UPDATE quota_usage SET used = used + $4 WHERE quota_id = $1 AND scope_key = $2 AND region = $3`,
			c.Quota.ID, c.ScopeKey, c.Region, amount)
		return err
	}
	var used int64
	err := tx.QueryRowContext(ctx, `
		UPDATE quota_usage SET used = used + $4
		WHERE quota_id = $1 AND scope_key = $2 AND region = $3 AND used + $4 <= $5
		RETURNING used`, c.Quota.ID, c.ScopeKey, c.Region, amount, limit.Value).Scan(&used)
	if db.IsNoRows(err) {
		if err := tx.QueryRowContext(ctx, `SELECT used FROM quota_usage WHERE quota_id = $1 AND scope_key = $2 AND region = $3`,
			c.Quota.ID, c.ScopeKey, c.Region).Scan(&used); err != nil {
			return err
		}
		return Exceeded(c, limit.Value, used)
	}
	return err
}

// Release gives amount back, in the transaction that removes what was counted: a finished delete,
// a failed create's rollback, a resize back down. Going below zero is a bug, and the CHECK
// refuses it.
func Release(ctx context.Context, tx *db.Tx, quotaID, scopeKey, region string, amount int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE quota_usage SET used = used - $4 WHERE quota_id = $1 AND scope_key = $2 AND region = $3`,
		quotaID, scopeKey, region, amount)
	return err
}

// Used is a counter's value (0 when it has none).
func Used(ctx context.Context, q db.Querier, quotaID, scopeKey, region string) (int64, error) {
	var used int64
	err := q.QueryRowContext(ctx, `SELECT used FROM quota_usage WHERE quota_id = $1 AND scope_key = $2 AND region = $3`, quotaID, scopeKey, region).Scan(&used)
	if db.IsNoRows(err) {
		return 0, nil
	}
	return used, err
}

// UsageRow is one counter, as GET /internal/v1/quotas/usage answers it.
type UsageRow struct {
	Quota    string `json:"quota"`
	ScopeKey string `json:"scopeKey"`
	Region   string `json:"region"`
	Used     int64  `json:"used"`
}

// UsageOf is every counter of an organization, rows at 0 included, in a stable order.
func UsageOf(ctx context.Context, q db.Querier, organizationID string) ([]UsageRow, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT quota_id, scope_key, region, used FROM quota_usage WHERE organization_id = $1
		ORDER BY quota_id, scope_key, region`, organizationID)
	if err != nil {
		return nil, err
	}
	out, err := db.Collect(rows, nil, func(r *sql.Rows) (UsageRow, error) {
		var u UsageRow
		return u, r.Scan(&u.Quota, &u.ScopeKey, &u.Region, &u.Used)
	})
	if out == nil {
		out = []UsageRow{}
	}
	return out, err
}

// DeleteOrganization deletes every counter of an organization, in tx, the transaction that purges
// the rest of a deleted organization's rows.
func DeleteOrganization(ctx context.Context, tx *db.Tx, organizationID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM quota_usage WHERE organization_id = $1`, organizationID)
	return err
}

// Exceeded is 409 quota_exceeded for an allocation quota: its value, and what's in use.
func Exceeded(c Counter, limit, used int64) *httpapi.Error {
	q := c.Quota
	body := map[string]any{"id": q.ID, "kind": q.Kind, "scope": q.Scope, "limit": limit, "used": used}
	if c.ResourceName != "" {
		body["scopeResourceName"] = c.ResourceName
	}
	if q.PerRegion && c.Region != "" {
		body["region"] = c.Region
	}
	message := fmt.Sprintf("This %s already has %d %s, and can have at most %d (%s)", scopeNoun(q), used, units(q.Unit, used), limit, q.ID)
	return &httpapi.Error{Status: http.StatusConflict, Code: "quota_exceeded", Message: message, Extra: map[string]any{"quota": body}}
}
