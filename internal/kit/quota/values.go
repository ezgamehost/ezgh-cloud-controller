package quota

import (
	"math/big"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// The reference for what quotas-api computes, as scripts/lib/quotas.mjs has it: billing periods,
// value resolution and a rate bucket's capacity. Products don't resolve values themselves (Quotas
// does, on every check); these are here so both kits and quotas-stack run the same vectors
// (contracts/vectors/quotas.json).

// BillingPeriod is the billing period containing t for an account's anchor day (1-28): from the
// anchor day at 00:00 UTC to the same day the next month.
func BillingPeriod(t time.Time, anchorDay int) (start, end time.Time) {
	y, m, d := t.UTC().Date()
	if d < anchorDay {
		m--
	}
	// time.Date normalizes month 0 to December of the year before.
	return time.Date(y, m, anchorDay, 0, 0, 0, 0, time.UTC), time.Date(y, m+1, anchorDay, 0, 0, 0, 0, time.UTC)
}

// Override is a value staff set for one (quota, organization, scope key or "" for the whole
// organization, region or "" for every region).
type Override struct {
	Quota          string     `json:"quota"`
	OrganizationID string     `json:"organizationId"`
	ScopeKey       string     `json:"scopeKey"`
	Region         string     `json:"region"`
	Value          int64      `json:"value"`
	ExpiresAt      *time.Time `json:"expiresAt"`
}

// Value sources.
const (
	SourceOverride             = "override"
	SourceOrganizationOverride = "organizationOverride"
	SourceTier                 = "tier"
	SourceDefault              = "default"
)

// Resolved is a value and where it came from.
type Resolved struct {
	Value  int64  `json:"value"`
	Source string `json:"source"`
}

// ResolveValue is the value for one (quota, scope key, region): an override for the exact scope,
// then one for the organization (when the scope is narrower), then the tier default, then the
// default. At each level an override for the exact region beats one for every region ("");
// expired overrides don't count.
func ResolveValue(q manifest.Quota, organizationID, scopeKey, region, tier string, overrides []Override, now time.Time) Resolved {
	regions := []string{""}
	if q.PerRegion {
		regions = []string{region, ""}
	}
	exact := scopeKey
	if q.Scope == "organization" {
		exact = ""
	}
	levels := []struct{ scope, source string }{{exact, SourceOverride}}
	if exact != "" {
		levels = append(levels, struct{ scope, source string }{"", SourceOrganizationOverride})
	}
	for _, level := range levels {
		for _, r := range regions {
			for _, o := range overrides {
				if o.Quota == q.ID && o.OrganizationID == organizationID && o.ScopeKey == level.scope && o.Region == r &&
					(o.ExpiresAt == nil || o.ExpiresAt.After(now)) {
					return Resolved{Value: o.Value, Source: level.source}
				}
			}
		}
	}
	if v, ok := q.TierDefaults[tier]; ok {
		return Resolved{Value: int64(v), Source: SourceTier}
	}
	return Resolved{Value: int64(q.Default), Source: SourceDefault}
}

// BucketCapacity is a rate quota's bucket size at value: the definition's burst scaled by
// value ÷ default and rounded up, or the value when there's no burst (or the default is 0).
func BucketCapacity(q manifest.Quota, value int64) int64 {
	if q.Burst == nil || q.Default == 0 {
		return value
	}
	// Exact for any value and burst up to 10¹²: the product can pass 2⁶³.
	n := new(big.Int).Mul(big.NewInt(value), big.NewInt(int64(*q.Burst)))
	d := big.NewInt(int64(q.Default))
	n.Add(n, new(big.Int).Sub(d, big.NewInt(1)))
	return n.Quo(n, d).Int64()
}
