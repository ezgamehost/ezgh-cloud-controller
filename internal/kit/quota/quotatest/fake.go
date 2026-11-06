// Package quotatest is a fake quotas-api for tests: check, resolve, lease release and renewal
// with Quotas' semantics (contracts/quotas.md), in memory, plus knobs to make it slow, fail or
// answer nonsense, so a product can test failing open.
package quotatest

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/instant"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
)

// Fake is quotas-api for one namespace's quotas.
type Fake struct {
	URL string

	mu     sync.Mutex
	quotas map[string]manifest.Quota
	// Values overrides a quota's value for a scope key ("" for every scope key).
	values map[[2]string]int64
	now    func() time.Time
	// Anchor is the organizations' billing anchor day (default 1).
	anchor int

	buckets map[string]*bucket
	usage   map[string]int64
	leases  map[string]map[string]time.Time // key → leaseId → expiry
	leaseOf map[string]string               // leaseId → key

	delay  time.Duration
	status int
	body   string

	// Checks, Resolves, Released and Renewed record what was asked, in order.
	Checks   []quota.CheckRequest
	Resolves []quota.ResolveRequest
	Released []string
	Renewed  []string
	// Registrations are the bodies PUT to /internal/v1/registrations/:namespace.
	Registrations []json.RawMessage
	// Auth is the last request's Authorization or X-Internal-Token header.
	Auth string
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New starts a fake quotas-api knowing the manifest's quotas, closed when the test ends.
func New(t testing.TB, m *manifest.Manifest) *Fake {
	f := &Fake{quotas: map[string]manifest.Quota{}, values: map[[2]string]int64{}, now: time.Now, anchor: 1,
		buckets: map[string]*bucket{}, usage: map[string]int64{}, leases: map[string]map[string]time.Time{}, leaseOf: map[string]string{}}
	for _, q := range m.Quotas {
		f.quotas[q.ID] = q
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	f.URL = server.URL
	return f
}

// SetValue sets a quota's value for scopeKey ("" for every scope key), as an override would.
func (f *Fake) SetValue(quotaID, scopeKey string, value int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[[2]string{quotaID, scopeKey}] = value
}

// SetClock replaces the clock.
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// Delay makes every answer wait d (past the kit's 50 ms: a timeout).
func (f *Fake) Delay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// Fail makes check and resolve answer status with body ("" for a JSON error), or answer
// normally again with status 0. A status of 200 answers body as it is: a malformed answer.
func (f *Fake) Fail(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

// LeasesHeld is how many unexpired leases a quota's scope key has.
func (f *Fake) LeasesHeld(quotaID, scopeKey string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, exp := range f.leases[quotaID+"|"+scopeKey+"|"] {
		if exp.After(f.now()) {
			n++
		}
	}
	return n
}

// LastAuth is the last request's Authorization (or X-Internal-Token) header.
func (f *Fake) LastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Auth
}

// RenewedIDs are the leases renewal was asked for, in order.
func (f *Fake) RenewedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Renewed...)
}

// Snapshot copies what was recorded, under the lock.
func (f *Fake) Snapshot() (checks []quota.CheckRequest, released []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]quota.CheckRequest(nil), f.Checks...), append([]string(nil), f.Released...)
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	delay := f.delay
	if a := r.Header.Get("Authorization"); a != "" {
		f.Auth = a
	} else {
		f.Auth = r.Header.Get("X-Internal-Token")
	}
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/internal/v1/quotas/check":
		var req quota.CheckRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_request", "message": err.Error()}})
			return
		}
		f.Checks = append(f.Checks, req)
		if f.failing(w) {
			return
		}
		f.check(w, req)
	case r.Method == http.MethodPost && path == "/internal/v1/quotas/resolve":
		var req quota.ResolveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_request", "message": err.Error()}})
			return
		}
		f.Resolves = append(f.Resolves, req)
		if f.failing(w) {
			return
		}
		values := []quota.Value{}
		for _, it := range req.Items {
			q := f.quotas[it.Quota]
			scope := it.ScopeKey
			if q.Scope == "organization" {
				scope = req.OrganizationID
			}
			v, source := f.value(q, scope)
			values = append(values, quota.Value{Quota: q.ID, Kind: q.Kind, ScopeKey: scope, Region: "", Value: v, Source: source})
		}
		writeJSON(w, 200, map[string]any{"values": values})
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/internal/v1/quotas/leases/"):
		id := strings.TrimPrefix(path, "/internal/v1/quotas/leases/")
		f.Released = append(f.Released, id)
		if key, ok := f.leaseOf[id]; ok {
			delete(f.leases[key], id)
			delete(f.leaseOf, id)
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/internal/v1/quotas/leases/") && strings.HasSuffix(path, "/renew"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/internal/v1/quotas/leases/"), "/renew")
		f.Renewed = append(f.Renewed, id)
		key, ok := f.leaseOf[id]
		if !ok || !f.leases[key][id].After(f.now()) {
			writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "lease_not_found", "message": "Lease not found"}})
			return
		}
		parts := strings.SplitN(key, "|", 3)
		exp := f.now().Add(f.ttl(f.quotas[parts[0]]))
		f.leases[key][id] = exp
		writeJSON(w, 200, quota.Lease{LeaseID: id, Quota: parts[0], ScopeKey: parts[1], ExpiresAt: instant.Format(exp)})
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/internal/v1/registrations/"):
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_request", "message": err.Error()}})
			return
		}
		f.Registrations = append(f.Registrations, body)
		ns := strings.TrimPrefix(path, "/internal/v1/registrations/")
		writeJSON(w, 200, map[string]any{"namespace": ns, "revision": 1, "digest": "sha256:" + strings.Repeat("0", 64), "dryRun": r.URL.Query().Get("dryRun") == "true",
			"changes": []any{}, "registeredAt": "2026-09-29T09:00:00.000Z", "updatedAt": "2026-09-29T09:00:00.000Z"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *Fake) failing(w http.ResponseWriter) bool {
	if f.status == 0 {
		return false
	}
	if f.body != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
		return true
	}
	writeJSON(w, f.status, map[string]any{"error": map[string]any{"code": "unavailable", "message": "Quotas is unavailable"}})
	return true
}

func (f *Fake) value(q manifest.Quota, scopeKey string) (int64, string) {
	if v, ok := f.values[[2]string{q.ID, scopeKey}]; ok {
		return v, quota.SourceOverride
	}
	if v, ok := f.values[[2]string{q.ID, ""}]; ok {
		return v, quota.SourceOrganizationOverride
	}
	return int64(q.Default), quota.SourceDefault
}

func (f *Fake) ttl(q manifest.Quota) time.Duration {
	if d, err := quota.ParseDuration(q.LeaseTTL); err == nil {
		return d
	}
	return 2 * time.Minute
}

// check applies Quotas' semantics: resolve every item, then take every rate, usage and
// concurrency item in one step, or none.
func (f *Fake) check(w http.ResponseWriter, req quota.CheckRequest) {
	now := f.now()
	type planned struct {
		q        manifest.Quota
		scope    string
		key      string
		value    int64
		source   string
		amount   int64
		capacity float64
		tokens   float64
	}
	var items []planned
	for i, it := range req.Items {
		q, ok := f.quotas[it.Quota]
		if !ok {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_request", "message": "Unknown quota", "issues": []any{map[string]any{"path": fmt.Sprintf("items.%d.quota", i), "message": "isn't a quota"}}}})
			return
		}
		scope := it.ScopeKey
		switch q.Scope {
		case "organization":
			scope = req.OrganizationID
		case "project":
			if scope == "" && req.ProjectID != nil {
				scope = *req.ProjectID
			}
		}
		amount := it.Amount
		if amount == 0 {
			amount = 1
		}
		v, source := f.value(q, scope)
		p := planned{q: q, scope: scope, key: q.ID + "|" + scope + "|", value: v, source: source, amount: amount}
		if q.Kind == manifest.Rate {
			window, _ := quota.ParseDuration(q.Window)
			p.capacity = float64(quota.BucketCapacity(q, v))
			b := f.buckets[p.key]
			if b == nil {
				b = &bucket{tokens: p.capacity, at: now}
				f.buckets[p.key] = b
			}
			p.tokens = min(p.capacity, b.tokens+float64(v)*now.Sub(b.at).Seconds()/window.Seconds())
		}
		items = append(items, p)
	}
	start, end := quota.BillingPeriod(now, f.anchor)
	periodStart, resetsAt := instant.Format(start), instant.Format(end)
	result := func(p planned, used *int64, remaining *int64) map[string]any {
		r := map[string]any{"quota": p.q.ID, "kind": p.q.Kind, "scopeKey": p.scope, "region": "", "value": p.value, "source": p.source,
			"used": used, "remaining": remaining, "window": nil, "periodStart": nil, "resetsAt": nil}
		switch p.q.Kind {
		case manifest.Rate:
			r["window"] = p.q.Window
		case manifest.Usage:
			r["periodStart"], r["resetsAt"] = periodStart, resetsAt
		}
		return r
	}
	ptr := func(n int64) *int64 { return &n }
	// Deny the first that doesn't fit, consuming nothing.
	for _, p := range items {
		switch p.q.Kind {
		case manifest.Rate:
			if p.tokens < float64(p.amount) {
				writeJSON(w, 200, map[string]any{"allowed": false, "results": []any{}, "leases": []any{}, "denied": result(p, nil, ptr(int64(p.tokens)))})
				return
			}
		case manifest.Usage:
			used := f.usage[p.key+periodStart]
			if used+p.amount > p.value {
				writeJSON(w, 200, map[string]any{"allowed": false, "results": []any{}, "leases": []any{}, "denied": result(p, ptr(used), ptr(max(p.value-used, 0)))})
				return
			}
		case manifest.Concurrency:
			held := f.held(p.key, now)
			if held+1 > p.value {
				writeJSON(w, 200, map[string]any{"allowed": false, "results": []any{}, "leases": []any{}, "denied": result(p, ptr(held), ptr(max(p.value-held, 0)))})
				return
			}
		}
	}
	results := []any{}
	leases := []quota.Lease{}
	for _, p := range items {
		switch p.q.Kind {
		case manifest.Allocation:
			results = append(results, result(p, nil, nil))
		case manifest.Rate:
			b := f.buckets[p.key]
			b.tokens, b.at = p.tokens-float64(p.amount), now
			results = append(results, result(p, nil, ptr(int64(b.tokens))))
		case manifest.Usage:
			f.usage[p.key+periodStart] += p.amount
			used := f.usage[p.key+periodStart]
			results = append(results, result(p, ptr(used), ptr(max(p.value-used, 0))))
		case manifest.Concurrency:
			id := newLeaseID()
			if f.leases[p.key] == nil {
				f.leases[p.key] = map[string]time.Time{}
			}
			exp := now.Add(f.ttl(p.q))
			f.leases[p.key][id] = exp
			f.leaseOf[id] = p.key
			leases = append(leases, quota.Lease{Quota: p.q.ID, ScopeKey: p.scope, LeaseID: id, ExpiresAt: instant.Format(exp)})
			held := f.held(p.key, now)
			results = append(results, result(p, ptr(held), ptr(max(p.value-held, 0))))
		}
	}
	writeJSON(w, 200, map[string]any{"allowed": true, "results": results, "leases": leases, "denied": nil})
}

func (f *Fake) held(key string, now time.Time) int64 {
	var n int64
	for _, exp := range f.leases[key] {
		if exp.After(now) {
			n++
		}
	}
	return n
}

func newLeaseID() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return "qls_" + string(b)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
