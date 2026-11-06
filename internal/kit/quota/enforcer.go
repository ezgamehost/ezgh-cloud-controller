package quota

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
)

// Enforcer is the product side of Quotas for a service's requests (contracts/quotas.md): it
// reads each endpoint's quotas from the manifest once, makes one check per request that needs
// one (after authorization, before the handler changes anything), turns a denial into
// 429 quota_exceeded, hands the handler its allocation values, fails open when Quotas can't
// answer, and releases leases after the response.
type Enforcer struct {
	// FailOpens is quotas_fail_open_total, and the paced log of fail-opens.
	FailOpens *FailOpens

	failOpenUnconfigured bool
	client               *Client
	region               string
	logger               *slog.Logger
	quotas               map[string]manifest.Quota
	plans                map[string]*Plan
}

// Options configure an Enforcer.
type Options struct {
	FailOpenUnconfigured bool
	Manifest             *manifest.Manifest
	// Client is quotas-api. Nil is the local fallback (APP_ENV=local without QUOTAS_API_URL):
	// no calls, allocation quotas at the manifest's default, everything else unlimited.
	Client *Client
	// Region is the product's REGION: perRegion quotas count and resolve in it.
	Region string
	Logger *slog.Logger
}

// Plan is what one request to an endpoint asks Quotas about: its rate, usage and concurrency
// quotas (consumed) and its organization- and project-scoped allocation quotas (resolved).
type Plan struct {
	Items []PlanItem
}

// PlanItem is one quota of a plan.
type PlanItem struct {
	Quota manifest.Quota
	// Amount is what one request consumes (for allocation, what the handler will count).
	Amount int64
	// ScopeParam is the path parameter holding a resource-scoped quota's scope key.
	ScopeParam string
}

// NewEnforcer builds each endpoint's plan from the manifest.
func NewEnforcer(o Options) *Enforcer {
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	e := &Enforcer{
		FailOpens: &FailOpens{Logger: logger}, failOpenUnconfigured: o.FailOpenUnconfigured,
		client: o.Client, region: o.Region, logger: logger,
		quotas: map[string]manifest.Quota{}, plans: map[string]*Plan{},
	}
	for _, q := range o.Manifest.Quotas {
		e.quotas[q.ID] = q
	}
	for _, ep := range o.Manifest.Endpoints {
		var items []PlanItem
		for _, ref := range ep.Quotas {
			q, ok := e.quotas[ref.ID]
			if !ok {
				continue
			}
			item := PlanItem{Quota: q, Amount: int64(ref.Amount)}
			if q.Scope == "resource" {
				// A resource-scoped allocation quota's value is resolved by the handler, which
				// learns its scope key; the others take theirs from the path.
				if q.Kind == manifest.Allocation {
					continue
				}
				item.ScopeParam = q.ResourceType + "Id"
			}
			items = append(items, item)
		}
		if len(items) > 0 {
			e.plans[ep.Method+" "+ep.Path] = &Plan{Items: items}
		}
	}
	return e
}

// Local reports whether this is the local fallback, which makes no calls.
func (e *Enforcer) Local() bool { return e.client == nil }

// Plan is an endpoint's plan, or nil when it consumes nothing Quotas checks (no call is made).
func (e *Enforcer) Plan(method, path string) *Plan { return e.plans[method+" "+path] }

// Quota is a quota of the manifest, by ID.
func (e *Enforcer) Quota(id string) (manifest.Quota, bool) {
	q, ok := e.quotas[id]
	return q, ok
}

// Request is who and where a check is for.
type Request struct {
	OrganizationID string
	// ProjectID is the path's project, or "".
	ProjectID string
	// RequestID is the customer request's X-Request-Id, for quotas-api's logs.
	RequestID string
	// Param reads a path parameter (a resource-scoped quota's scope key).
	Param func(string) string
	// The scopes' resource names, for a refusal's scopeResourceName; "" when unknown.
	OrganizationName, ProjectName, ResourceName string
}

var requestIDForm = regexp.MustCompile(`^[\x21-\x7e]{1,128}$`)

// Check runs a plan: one call to quotas-api, all or nothing. Allowed (or failed open), it
// returns the grant: allocation values and leases. Denied, it returns 429 quota_exceeded.
func (e *Enforcer) Check(ctx context.Context, plan *Plan, r Request) (*Grant, error) {
	if plan == nil {
		return &Grant{e: e, values: map[string]Resolved{}}, nil
	}
	return e.CheckItems(ctx, r, plan.Items)
}

// CheckItems checks items that depend on the request (a batch create's amount), which the
// manifest can't list: the handler calls it with the amount.
func (e *Enforcer) CheckItems(ctx context.Context, r Request, items []PlanItem) (*Grant, error) {
	g := &Grant{e: e, values: map[string]Resolved{}}
	if len(items) == 0 {
		return g, nil
	}
	if e.client == nil {
		if e.failOpenUnconfigured {
			g.failedOpen = true
			e.failedOpen(nil, errors.New("quotas unconfigured"))
			return g, nil
		}
		for _, it := range items {
			if it.Quota.Kind == manifest.Allocation {
				g.values[valueKey(it.Quota.ID, e.scopeKey(it, r))] = Resolved{Value: int64(it.Quota.Default), Source: SourceDefault}
			}
		}
		return g, nil
	}
	req := CheckRequest{OrganizationID: r.OrganizationID, Region: e.region, Items: make([]Item, len(items))}
	if r.ProjectID != "" {
		req.ProjectID = &r.ProjectID
	}
	if requestIDForm.MatchString(r.RequestID) {
		req.RequestID = &r.RequestID
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.Quota.ID
		item := Item{Quota: it.Quota.ID}
		if it.Quota.Scope == "resource" {
			item.ScopeKey = e.scopeKey(it, r)
		}
		if it.Amount > 1 && it.Quota.Kind != manifest.Allocation && it.Quota.Kind != manifest.Concurrency {
			item.Amount = it.Amount
		}
		req.Items[i] = item
	}
	resp, err := e.client.Check(ctx, req)
	if err != nil {
		e.failedOpen(ids, err)
		g.failedOpen = true
		return g, nil
	}
	e.FailOpens.Answered()
	if !resp.Allowed {
		return nil, e.refusal(*resp.Denied, r)
	}
	for _, res := range resp.Results {
		if res.Kind == manifest.Allocation {
			g.values[valueKey(res.Quota, res.ScopeKey)] = Resolved{Value: res.Value, Source: res.Source}
		}
	}
	g.leases = resp.Leases
	return g, nil
}

func (e *Enforcer) failedOpen(ids []string, err error) {
	var f *Failure
	if !errors.As(err, &f) {
		f = &Failure{Reason: ReasonUnreachable, Err: err}
	}
	e.FailOpens.Record(ids, f)
}

func (e *Enforcer) scopeKey(it PlanItem, r Request) string {
	switch it.Quota.Scope {
	case "project":
		return r.ProjectID
	case "resource":
		if r.Param != nil {
			return r.Param(it.ScopeParam)
		}
		return ""
	default:
		return r.OrganizationID
	}
}

func valueKey(quota, scopeKey string) string { return quota + "\x00" + scopeKey }

// Limit is an allocation quota's value for one scope key, applied in the product's transaction.
type Limit struct {
	Value  int64
	Source string
	// Enforced is false when Quotas didn't answer: count without the condition (fail open).
	Enforced bool
}

// Unlimited is the limit when Quotas didn't answer: the product counts, without refusing.
var Unlimited = Limit{}

// Resolve answers allocation values outside a check (a resource-scoped quota, whose scope key the
// handler learns; a worker): one limit per item, Unlimited for all of them when Quotas doesn't
// answer, the manifest's defaults locally.
func (e *Enforcer) Resolve(ctx context.Context, organizationID string, items []Item) []Limit {
	limits := make([]Limit, len(items))
	if e.client == nil {
		if e.failOpenUnconfigured {
			e.failedOpen(nil, errors.New("quotas unconfigured"))
			return limits
		}
		for i, it := range items {
			if q, ok := e.quotas[it.Quota]; ok {
				limits[i] = Limit{Value: int64(q.Default), Source: SourceDefault, Enforced: true}
			}
		}
		return limits
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.Quota
		if q, ok := e.quotas[it.Quota]; ok && q.PerRegion && it.Region == "" {
			items[i].Region = e.region
		}
	}
	values, err := e.client.Resolve(ctx, ResolveRequest{OrganizationID: organizationID, Items: items})
	if err != nil {
		e.failedOpen(ids, err)
		return limits
	}
	e.FailOpens.Answered()
	for i, v := range values {
		limits[i] = Limit{Value: v.Value, Source: v.Source, Enforced: true}
	}
	return limits
}

// ---- The grant ---------------------------------------------------------------------------------

// Grant is what an allowed (or failed-open) check gave one request: its allocation values and its
// leases. Release it after the response.
type Grant struct {
	e          *Enforcer
	failedOpen bool
	values     map[string]Resolved
	leases     []Lease
	mu         sync.Mutex
	released   bool
}

// FailedOpen reports whether Quotas didn't answer (the request proceeds as if unlimited).
func (g *Grant) FailedOpen() bool { return g.failedOpen }

// Leases are the concurrency leases the request holds.
func (g *Grant) Leases() []Lease { return g.leases }

// Limit is an allocation quota's value for scopeKey, from the check. ok is false when the check
// didn't ask about it (resolve it instead). Failed open, it's Unlimited.
func (g *Grant) Limit(quotaID, scopeKey string) (Limit, bool) {
	if g.failedOpen {
		return Unlimited, true
	}
	v, ok := g.values[valueKey(quotaID, scopeKey)]
	if !ok {
		return Limit{}, false
	}
	return Limit{Value: v.Value, Source: v.Source, Enforced: true}, true
}

// Merge adds another check's grant (a handler's own CheckItems) to this one, so one release
// covers both.
func (g *Grant) Merge(other *Grant) {
	if other == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failedOpen = g.failedOpen || other.failedOpen
	for k, v := range other.values {
		g.values[k] = v
	}
	g.leases = append(g.leases, other.leases...)
}

// Release gives every lease back, fire-and-forget, once. The kit calls it after the response.
func (g *Grant) Release() {
	g.mu.Lock()
	if g.released || g.e == nil || g.e.client == nil {
		g.released = true
		g.mu.Unlock()
		return
	}
	g.released = true
	leases := g.leases
	g.mu.Unlock()
	for _, l := range leases {
		g.e.client.Release(l.LeaseID, func(err error) {
			if err != nil {
				g.e.logger.Warn("releasing a quota lease failed; it expires at its leaseTtl", "quota", l.Quota, "leaseId", l.LeaseID, "error", err.Error())
			}
		})
	}
}

// KeepAlive renews the grant's leases every leaseTtl ÷ 3 while long work holds them (a streaming
// download), until stop is called or ctx ends. A lease that has expired or been released is
// dropped quietly: the work carries on without it.
func (g *Grant) KeepAlive(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	if g.e == nil || g.e.client == nil {
		return cancel
	}
	for _, l := range g.leases {
		ttl := 2 * time.Minute
		if q, ok := g.e.quotas[l.Quota]; ok && q.LeaseTTL != "" {
			if d, err := ParseDuration(q.LeaseTTL); err == nil {
				ttl = d
			}
		}
		go g.e.heartbeat(ctx, l, ttl/3)
	}
	return cancel
}

func (e *Enforcer) heartbeat(ctx context.Context, l Lease, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, err := e.client.Renew(ctx, l.LeaseID)
		switch {
		case errors.Is(err, ErrLeaseNotFound):
			e.logger.Info("a quota lease expired before it was renewed; carrying on without it", "quota", l.Quota, "leaseId", l.LeaseID)
			return
		case err != nil && ctx.Err() == nil:
			e.logger.Warn("renewing a quota lease failed; will retry", "quota", l.Quota, "leaseId", l.LeaseID, "error", err.Error())
		}
	}
}

// ---- Refusals ----------------------------------------------------------------------------------

// refusal is 429 quota_exceeded for a denied rate, usage or concurrency quota, without
// Retry-After.
func (e *Enforcer) refusal(d Result, r Request) *httpapi.Error {
	q, ok := e.quotas[d.Quota]
	if !ok {
		q = manifest.Quota{ID: d.Quota, Kind: d.Kind, Scope: "organization", Unit: "request"}
	}
	name := ""
	switch q.Scope {
	case "organization":
		name = r.OrganizationName
	case "project":
		if d.ScopeKey == r.ProjectID {
			name = r.ProjectName
		}
	case "resource":
		name = r.ResourceName
	}
	body := map[string]any{"id": q.ID, "kind": q.Kind, "scope": q.Scope, "limit": d.Value}
	if name != "" {
		body["scopeResourceName"] = name
	}
	if q.PerRegion && d.Region != "" {
		body["region"] = d.Region
	}
	used := int64(0)
	if d.Used != nil {
		used = *d.Used
	}
	var message string
	switch q.Kind {
	case manifest.Rate:
		window := q.Window
		if d.Window != nil && *d.Window != "" {
			window = *d.Window
		}
		body["window"] = window
		message = fmt.Sprintf("At most %d %s per %s in this %s (%s)", d.Value, units(q.Unit, d.Value), windowText(window), scopeNoun(q), q.ID)
	case manifest.Usage:
		body["used"] = used
		resets := ""
		if d.PeriodStart != nil && d.ResetsAt != nil {
			body["periodStart"], body["resetsAt"] = *d.PeriodStart, *d.ResetsAt
			resets = ", which resets at " + *d.ResetsAt
		}
		message = fmt.Sprintf("This %s has used %d of its %d %s for the billing period%s (%s)", scopeNoun(q), used, d.Value, units(q.Unit, d.Value), resets, q.ID)
	default:
		body["inFlight"] = used
		message = fmt.Sprintf("This %s already has %d %s in flight, and can have at most %d at once (%s)", scopeNoun(q), used, units(q.Unit, used), d.Value, q.ID)
	}
	return &httpapi.Error{Status: http.StatusTooManyRequests, Code: "quota_exceeded", Message: message, Extra: map[string]any{"quota": body}}
}

// scopeNoun is what a quota counts per, in a message: "organization", "project", or the resource
// type ("widget").
func scopeNoun(q manifest.Quota) string {
	if q.Scope == "resource" {
		if q.ResourceType != "" {
			return q.ResourceType
		}
		return "resource"
	}
	return q.Scope
}

// units is the unit for n of them, as quota_exceeded messages name it (contracts/quotas.md,
// "Refusals"; contracts/vectors/quotas.json units): singular for 1; otherwise a word's English
// plural ("widgets", "addresses", "policies"), and in a "<unit>-per-<unit>" its first word's
// ("megabytes-per-second"). A unit that isn't a lowercase word never changes: an abbreviation with
// no vowel ("kb"), one ending in "ps" (a rate: "iops", "reqps"), or anything with a digit, "/",
// "." or a capital.
func units(unit string, n int64) string {
	if unit == "" {
		unit = "unit"
	}
	if n == 1 || !unitWord.MatchString(unit) {
		return unit
	}
	if head, rest, ok := strings.Cut(unit, "-per-"); ok {
		return units(head, n) + "-per-" + rest
	}
	last := unit[strings.LastIndexByte(unit, '-')+1:]
	if !strings.ContainsAny(last, "aeiouy") || strings.HasSuffix(last, "ps") {
		return unit
	}
	switch {
	case strings.HasSuffix(unit, "s"), strings.HasSuffix(unit, "x"), strings.HasSuffix(unit, "z"),
		strings.HasSuffix(unit, "sh"), strings.HasSuffix(unit, "ch"):
		return unit + "es"
	case len(unit) > 1 && unit[len(unit)-1] == 'y' && !strings.ContainsRune("aeiou", rune(unit[len(unit)-2])):
		return unit[:len(unit)-1] + "ies"
	}
	return unit + "s"
}

// unitWord is a unit made of lowercase words, joined by hyphens.
var unitWord = regexp.MustCompile(`^[a-z]+(?:-[a-z]+)*$`)

// windowText is a window in words: PT1M "minute", PT10S "10 seconds".
func windowText(window string) string {
	d, err := ParseDuration(window)
	if err != nil {
		return window
	}
	for _, u := range []struct {
		d    time.Duration
		name string
	}{{time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		if d%u.d == 0 {
			if n := int64(d / u.d); n != 1 {
				return strconv.FormatInt(n, 10) + " " + u.name + "s"
			}
			return u.name
		}
	}
	return window
}

var durationPattern = regexp.MustCompile(`^PT(?:([1-9][0-9]*)S|([1-9][0-9]*)M|(1)H)$`)

// ParseDuration reads a rate window or a lease TTL: PT<n>S, PT<n>M or PT1H, from a second to an
// hour.
func ParseDuration(text string) (time.Duration, error) {
	m := durationPattern.FindStringSubmatch(text)
	if m == nil {
		return 0, fmt.Errorf("quota: %q isn't PT<n>S, PT<n>M or PT1H", text)
	}
	var d time.Duration
	switch {
	case m[1] != "":
		n, _ := strconv.ParseInt(m[1], 10, 64)
		d = time.Duration(n) * time.Second
	case m[2] != "":
		n, _ := strconv.ParseInt(m[2], 10, 64)
		d = time.Duration(n) * time.Minute
	default:
		d = time.Hour
	}
	if d < time.Second || d > time.Hour {
		return 0, fmt.Errorf("quota: %q is outside a second to an hour", text)
	}
	return d, nil
}
