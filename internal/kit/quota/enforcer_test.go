package quota_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota/quotatest"
)

const (
	org        = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	project    = "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	credential = "qk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	orgName    = "ezgh::org_k3f9a0x2m7qp"
	prjName    = "ezgh::org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2"
)

func intp(n int) *int { return &n }

var (
	perProject  = manifest.Quota{ID: "widgets.widgetsPerProject", DisplayName: "Widgets per project", Kind: "allocation", Scope: "project", Default: 20, Unit: "widget"}
	largePerOrg = manifest.Quota{ID: "widgets.largeWidgetsPerOrganization", DisplayName: "Large widgets", Kind: "allocation", Scope: "organization", Default: 5, Unit: "widget"}
	perMinute   = manifest.Quota{ID: "widgets.widgetCreatesPerMinute", DisplayName: "Creates per minute", Kind: "rate", Scope: "organization", Default: 3, Unit: "request", Window: "PT1M", Burst: intp(3)}
	perPeriod   = manifest.Quota{ID: "widgets.api.requestsPerPeriod", DisplayName: "Requests per period", Kind: "usage", Scope: "organization", Default: 1000, Unit: "request"}
	inFlight    = manifest.Quota{ID: "widgets.api.concurrentRequests", DisplayName: "Concurrent requests", Kind: "concurrency", Scope: "organization", Default: 2, Unit: "request", LeaseTTL: "PT3S"}
)

const create = "/v1/organizations/:orgId/projects/:projectId/widgets"

func testManifest() *manifest.Manifest {
	ref := func(ids ...string) []manifest.EndpointQuota {
		out := make([]manifest.EndpointQuota, len(ids))
		for i, id := range ids {
			out[i] = manifest.EndpointQuota{ID: id, Amount: 1}
		}
		return out
	}
	return &manifest.Manifest{
		Namespace: "widgets",
		Quotas:    []manifest.Quota{perProject, largePerOrg, perMinute, perPeriod, inFlight},
		Endpoints: []manifest.Endpoint{
			{Method: "POST", Path: create, Auth: "caller", Quotas: ref(perPeriod.ID, inFlight.ID, perProject.ID, largePerOrg.ID, perMinute.ID)},
			{Method: "GET", Path: create, Auth: "caller", Quotas: ref(perPeriod.ID, inFlight.ID)},
			{Method: "GET", Path: "/v1/organizations/:orgId/other", Auth: "caller"},
		},
	}
}

type logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func setup(t *testing.T) (*quotatest.Fake, *quota.Enforcer, *logs) {
	fake := quotatest.New(t, testManifest())
	out := &logs{}
	e := quota.NewEnforcer(quota.Options{
		Manifest: testManifest(), Region: "us-west-1", Logger: slog.New(slog.NewJSONHandler(out, nil)),
		// Past the 50 ms default: these tests aren't about the timeout (TestFailsOpen is), and 20
		// concurrent checks under -race on a busy CI runner can take longer, failing open.
		Client: &quota.Client{URL: fake.URL, Credential: credential, Timeout: 5 * time.Second},
	})
	return fake, e, out
}

var request = quota.Request{OrganizationID: org, ProjectID: project, RequestID: "req-1", OrganizationName: orgName, ProjectName: prjName}

func createPlan(e *quota.Enforcer) *quota.Plan { return e.Plan("POST", create) }

func TestPlansComeFromTheManifest(t *testing.T) {
	_, e, _ := setup(t)
	if p := e.Plan("GET", "/v1/organizations/:orgId/other"); p != nil {
		t.Errorf("an endpoint without quotas has a plan: %+v", p)
	}
	if p := createPlan(e); p == nil || len(p.Items) != 5 {
		t.Fatalf("create's plan: %+v", p)
	}
}

func TestAllowed(t *testing.T) {
	fake, e, _ := setup(t)
	fake.SetValue(perProject.ID, project, 100)
	g, err := e.Check(context.Background(), createPlan(e), request)
	if err != nil {
		t.Fatal(err)
	}
	checks, _ := fake.Snapshot()
	if len(checks) != 1 || fake.LastAuth() != "Bearer "+credential {
		t.Fatalf("%d checks, auth %q", len(checks), fake.LastAuth())
	}
	c := checks[0]
	if c.OrganizationID != org || *c.ProjectID != project || c.Region != "us-west-1" || *c.RequestID != "req-1" || len(c.Items) != 5 {
		t.Errorf("check %+v", c)
	}
	for _, it := range c.Items {
		if it.ScopeKey != "" || it.Amount != 0 {
			t.Errorf("item %+v: organization and project scope keys and an amount of 1 are left out", it)
		}
	}
	if l, ok := g.Limit(perProject.ID, project); !ok || l != (quota.Limit{Value: 100, Source: "override", Enforced: true}) {
		t.Errorf("per project %+v %v", l, ok)
	}
	if l, ok := g.Limit(largePerOrg.ID, org); !ok || l.Value != 5 || !l.Enforced {
		t.Errorf("large %+v %v", l, ok)
	}
	if _, ok := g.Limit(largePerOrg.ID, project); ok {
		t.Error("a limit for a scope key that wasn't asked about")
	}
	if len(g.Leases()) != 1 || fake.LeasesHeld(inFlight.ID, org) != 1 || g.FailedOpen() {
		t.Fatalf("leases %+v", g.Leases())
	}
	g.Release()
	g.Release() // once
	waitFor(t, func() bool { return fake.LeasesHeld(inFlight.ID, org) == 0 })
	if _, released := fake.Snapshot(); len(released) != 1 || released[0] != g.Leases()[0].LeaseID {
		t.Errorf("released %v", released)
	}
	if e.FailOpens.Total() != 0 {
		t.Error("counted a fail-open")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func refusal(t *testing.T, err error) (*httpapi.Error, map[string]any) {
	t.Helper()
	var e *httpapi.Error
	if !errors.As(err, &e) || e.Code != "quota_exceeded" {
		t.Fatalf("not quota_exceeded: %v", err)
	}
	if e.RetryAfter != 0 {
		t.Errorf("Retry-After %d", e.RetryAfter)
	}
	w := httptest.NewRecorder()
	httpapi.WriteError(w, e)
	if w.Header().Get("Retry-After") != "" {
		t.Error("a quota refusal with Retry-After")
	}
	return e, e.Extra["quota"].(map[string]any)
}

func TestDeniedRate(t *testing.T) {
	fake, e, _ := setup(t)
	now := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	fake.SetClock(func() time.Time { return now })
	for i := range 3 {
		g, err := e.Check(context.Background(), createPlan(e), request)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		g.Release()
		waitFor(t, func() bool { return fake.LeasesHeld(inFlight.ID, org) == 0 })
	}
	_, err := e.Check(context.Background(), createPlan(e), request)
	he, q := refusal(t, err)
	if he.Status != 429 || he.Message != "At most 3 requests per minute in this organization ("+perMinute.ID+")" {
		t.Errorf("%d %q", he.Status, he.Message)
	}
	want := map[string]any{"id": perMinute.ID, "kind": "rate", "scope": "organization", "scopeResourceName": orgName, "limit": int64(3), "window": "PT1M"}
	if !equal(q, want) {
		t.Errorf("quota %v", q)
	}
	// All or nothing: the refused request took no lease and counted no usage.
	if fake.LeasesHeld(inFlight.ID, org) != 0 {
		t.Error("a refused request holds a lease")
	}
	// The bucket refills: 20 seconds later there's one token.
	now = now.Add(20 * time.Second)
	if _, err := e.Check(context.Background(), createPlan(e), request); err != nil {
		t.Errorf("after refilling: %v", err)
	}
}

func equal(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

func TestDeniedUsage(t *testing.T) {
	fake, e, _ := setup(t)
	fake.SetValue(perPeriod.ID, "", 2)
	fake.SetClock(func() time.Time { return time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC) })
	list := e.Plan("GET", create)
	for range 2 {
		g, err := e.Check(context.Background(), list, request)
		if err != nil {
			t.Fatal(err)
		}
		g.Release()
	}
	_, err := e.Check(context.Background(), list, request)
	he, q := refusal(t, err)
	if he.Status != 429 || he.Message != "This organization has used 2 of its 2 requests for the billing period, which resets at 2026-10-01T00:00:00.000Z ("+perPeriod.ID+")" {
		t.Errorf("%d %q", he.Status, he.Message)
	}
	want := map[string]any{"id": perPeriod.ID, "kind": "usage", "scope": "organization", "scopeResourceName": orgName, "limit": int64(2), "used": int64(2),
		"periodStart": "2026-09-01T00:00:00.000Z", "resetsAt": "2026-10-01T00:00:00.000Z"}
	if !equal(q, want) {
		t.Errorf("quota %v", q)
	}
}

func TestDeniedConcurrency(t *testing.T) {
	fake, e, _ := setup(t)
	list := e.Plan("GET", create)
	var held []*quota.Grant
	for range 2 {
		g, err := e.Check(context.Background(), list, request)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, g)
	}
	_, err := e.Check(context.Background(), list, request)
	he, q := refusal(t, err)
	if he.Status != 429 || he.Message != "This organization already has 2 requests in flight, and can have at most 2 at once ("+inFlight.ID+")" {
		t.Errorf("%d %q", he.Status, he.Message)
	}
	if !equal(q, map[string]any{"id": inFlight.ID, "kind": "concurrency", "scope": "organization", "scopeResourceName": orgName, "limit": int64(2), "inFlight": int64(2)}) {
		t.Errorf("quota %v", q)
	}
	// Releasing one makes room.
	held[0].Release()
	waitFor(t, func() bool { return fake.LeasesHeld(inFlight.ID, org) == 1 })
	if _, err := e.Check(context.Background(), list, request); err != nil {
		t.Error(err)
	}
}

// Every way of not getting a valid 200 fails open: no lease, nothing consumed, allocation
// unlimited (counted without a condition), and one count per quota of the request.
func TestFailsOpen(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		break_ func(f *quotatest.Fake, c *quota.Client)
	}{
		{"timeout", "timeout", func(f *quotatest.Fake, _ *quota.Client) { f.Delay(300 * time.Millisecond) }},
		{"refused connection", "unreachable", func(_ *quotatest.Fake, c *quota.Client) { c.URL = "http://127.0.0.1:1" }},
		{"503", "unavailable", func(f *quotatest.Fake, _ *quota.Client) { f.Fail(503, "") }},
		{"500", "refused", func(f *quotatest.Fake, _ *quota.Client) { f.Fail(500, "") }},
		{"400", "refused", func(f *quotatest.Fake, _ *quota.Client) { f.Fail(400, "") }},
		{"not JSON", "invalid_response", func(f *quotatest.Fake, _ *quota.Client) { f.Fail(200, "<html>") }},
		{"missing fields", "invalid_response", func(f *quotatest.Fake, _ *quota.Client) { f.Fail(200, `{"allowed":true}`) }},
		{"a result short", "invalid_response", func(f *quotatest.Fake, _ *quota.Client) {
			f.Fail(200, `{"allowed":true,"results":[],"leases":[],"denied":null}`)
		}},
		{"denied without a refusal", "invalid_response", func(f *quotatest.Fake, _ *quota.Client) {
			f.Fail(200, `{"allowed":false,"results":[],"leases":[],"denied":null}`)
		}},
		{"a malformed lease", "invalid_response", func(f *quotatest.Fake, _ *quota.Client) {
			f.Fail(200, `{"allowed":true,"results":[`+
				`{"quota":"widgets.api.requestsPerPeriod","kind":"usage","scopeKey":"`+org+`","region":"","value":9,"source":"default","used":1,"remaining":8,"window":null,"periodStart":"2026-09-01T00:00:00.000Z","resetsAt":"2026-10-01T00:00:00.000Z"},`+
				`{"quota":"widgets.api.concurrentRequests","kind":"concurrency","scopeKey":"`+org+`","region":"","value":9,"source":"default","used":1,"remaining":8,"window":null,"periodStart":null,"resetsAt":null}],`+
				`"leases":[{"quota":"widgets.api.concurrentRequests","scopeKey":"`+org+`","leaseId":"nope","expiresAt":"2026-09-29T10:00:00.000Z"}],"denied":null}`)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := quotatest.New(t, testManifest())
			out := &logs{}
			client := &quota.Client{URL: fake.URL, Credential: credential}
			e := quota.NewEnforcer(quota.Options{Manifest: testManifest(), Region: "us-west-1", Logger: slog.New(slog.NewJSONHandler(out, nil)), Client: client})
			c.break_(fake, client)
			plan := createPlan(e)
			if c.name == "a malformed lease" {
				plan = e.Plan("GET", create)
			}
			started := time.Now()
			g, err := e.Check(context.Background(), plan, request)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
				t.Errorf("took %s: the timeout is 50 ms", elapsed)
			}
			if !g.FailedOpen() || len(g.Leases()) != 0 {
				t.Fatalf("failed open %v, leases %v", g.FailedOpen(), g.Leases())
			}
			for _, q := range []string{perProject.ID, largePerOrg.ID} {
				if l, ok := g.Limit(q, project); !ok || l.Enforced {
					t.Errorf("%s: %+v", q, l)
				}
			}
			counts := e.FailOpens.Counts()
			if len(counts) != len(plan.Items) {
				t.Fatalf("counts %+v", counts)
			}
			for _, n := range counts {
				if n.Reason != c.reason || n.Value != 1 {
					t.Errorf("count %+v, want reason %s", n, c.reason)
				}
			}
			var metrics strings.Builder
			_ = e.FailOpens.WritePrometheus(&metrics)
			if !strings.Contains(metrics.String(), `quotas_fail_open_total{quota="`+perPeriod.ID+`",reason="`+c.reason+`"} 1`) {
				t.Errorf("metrics:\n%s", metrics.String())
			}
			level := `"level":"WARN"`
			if c.name == "400" {
				level = `"level":"ERROR"`
			}
			if !strings.Contains(out.String(), level) || !strings.Contains(out.String(), `"metric":"quotas_fail_open_total"`) {
				t.Errorf("log: %s", out.String())
			}
		})
	}
}

// The fail-open log is paced: the first, every 100th, and the recovery; a 400 every time.
func TestFailOpenLogsArePaced(t *testing.T) {
	fake, e, out := setup(t)
	fake.Fail(503, "")
	for range 150 {
		if _, err := e.Check(context.Background(), e.Plan("GET", create), request); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(out.String(), "fail open"); n != 2 {
		t.Errorf("%d fail-open lines for 150 fail-opens, want 2 (the 1st and the 100th)", n)
	}
	fake.Fail(0, "")
	for range 2 {
		g, err := e.Check(context.Background(), e.Plan("GET", create), request)
		if err != nil {
			t.Fatal(err)
		}
		g.Release()
	}
	if n := strings.Count(out.String(), "answering again"); n != 1 {
		t.Errorf("%d recovery lines", n)
	}
	fake.Fail(403, "")
	for range 3 {
		_, _ = e.Check(context.Background(), e.Plan("GET", create), request)
	}
	if n := strings.Count(out.String(), `"level":"ERROR"`); n != 3 {
		t.Errorf("%d error lines for 3 refusals", n)
	}
	if e.FailOpens.Total() != 153 {
		t.Errorf("total %d", e.FailOpens.Total())
	}
}

// A 401 is the service's own credential being refused: it still fails open (reason refused), but
// it's logged at error level every time and counted in quotas_credential_rejected_total, which
// nothing else (a 403, a 503) counts.
func TestCredentialRejected(t *testing.T) {
	fake, e, out := setup(t)
	fake.Fail(401, `{"error":{"code":"unauthenticated","message":"x"}}`)
	for range 3 {
		g, err := e.Check(context.Background(), e.Plan("GET", create), request)
		if err != nil {
			t.Fatal(err)
		}
		if !g.FailedOpen() {
			t.Fatal("a 401 didn't fail open")
		}
	}
	fake.Fail(403, "")
	_, _ = e.Check(context.Background(), e.Plan("GET", create), request)
	fake.Fail(503, "")
	_, _ = e.Check(context.Background(), e.Plan("GET", create), request)
	if got := e.FailOpens.CredentialRejected(); got != 3 {
		t.Errorf("CredentialRejected() = %d, want 3 (the 401s only)", got)
	}
	if got := e.FailOpens.Total(); got != 5 {
		t.Errorf("Total() = %d, want 5", got)
	}
	for _, c := range e.FailOpens.Counts() {
		if c.Reason != "refused" && c.Reason != "unavailable" {
			t.Errorf("count %+v: a 401's reason stays refused", c)
		}
	}
	if n := strings.Count(out.String(), `"alert":"quotas_credential_rejected_total"`); n != 3 {
		t.Errorf("%d credential lines for 3 401s:\n%s", n, out.String())
	}
	var metrics strings.Builder
	if err := e.FailOpens.WritePrometheus(&metrics); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metrics.String(), "# TYPE quotas_credential_rejected_total counter\nquotas_credential_rejected_total 3\n") {
		t.Errorf("metrics:\n%s", metrics.String())
	}
}

// With APP_ENV=local and no QUOTAS_API_URL: no calls, allocation at the manifest's defaults,
// everything else unlimited.
func TestLocalFallback(t *testing.T) {
	e := quota.NewEnforcer(quota.Options{Manifest: testManifest(), Region: "us-local-1"})
	if !e.Local() {
		t.Fatal("not local")
	}
	for range 10 {
		g, err := e.Check(context.Background(), createPlan(e), request)
		if err != nil {
			t.Fatal(err)
		}
		if l, ok := g.Limit(perProject.ID, project); !ok || l != (quota.Limit{Value: 20, Source: "default", Enforced: true}) {
			t.Errorf("%+v", l)
		}
		if len(g.Leases()) != 0 || g.FailedOpen() {
			t.Error("leases or a fail-open locally")
		}
		g.Release()
	}
	if l := e.Resolve(context.Background(), org, []quota.Item{{Quota: largePerOrg.ID}}); l[0].Value != 5 || !l[0].Enforced {
		t.Errorf("resolve %+v", l)
	}
}

func TestResolve(t *testing.T) {
	fake, e, _ := setup(t)
	fake.SetValue(perProject.ID, "", 40)
	limits := e.Resolve(context.Background(), org, []quota.Item{{Quota: perProject.ID, ScopeKey: project}, {Quota: largePerOrg.ID}})
	if limits[0] != (quota.Limit{Value: 40, Source: "organizationOverride", Enforced: true}) || limits[1].Value != 5 {
		t.Errorf("%+v", limits)
	}
	fake.Fail(503, "")
	limits = e.Resolve(context.Background(), org, []quota.Item{{Quota: perProject.ID, ScopeKey: project}})
	if limits[0].Enforced || e.FailOpens.Total() != 1 {
		t.Errorf("%+v", limits)
	}
}

// A request-dependent amount is checked by the handler, with the amount; a concurrency item's
// amount is always one lease.
func TestCheckItemsWithAnAmount(t *testing.T) {
	fake, e, _ := setup(t)
	fake.SetClock(func() time.Time { return time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC) })
	items := []quota.PlanItem{{Quota: perMinute, Amount: 2}, {Quota: inFlight, Amount: 1}}
	g, err := e.CheckItems(context.Background(), request, items)
	if err != nil {
		t.Fatal(err)
	}
	g.Release()
	checks, _ := fake.Snapshot()
	if got := checks[0].Items; got[0].Amount != 2 || got[1].Amount != 0 {
		t.Errorf("items %+v", got)
	}
	if _, err := e.CheckItems(context.Background(), request, items); err == nil {
		t.Error("4 of 3 tokens taken")
	}
}

// Long work renews its leases every leaseTtl ÷ 3; a lease that's gone stops being renewed.
func TestKeepAlive(t *testing.T) {
	fake, e, _ := setup(t)
	g, err := e.Check(context.Background(), e.Plan("GET", create), request)
	if err != nil {
		t.Fatal(err)
	}
	stop := g.KeepAlive(context.Background())
	waitForLong(t, func() bool { return renewed(fake) >= 1 })
	g.Release()
	waitForLong(t, func() bool { return renewed(fake) >= 2 })
	n := renewed(fake)
	time.Sleep(1300 * time.Millisecond)
	if renewed(fake) != n {
		t.Error("renewed a released lease again after its 404")
	}
	stop()
}

func renewed(f *quotatest.Fake) int { return len(f.RenewedIDs()) }

func waitForLong(t *testing.T, cond func() bool) {
	t.Helper()
	for range 60 {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestUnconfiguredFailOpenOption(t *testing.T) {
	e := quota.NewEnforcer(quota.Options{Manifest: testManifest(), FailOpenUnconfigured: true})
	g, err := e.Check(context.Background(), createPlan(e), request)
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := g.Limit(perProject.ID, project); l.Enforced {
		t.Fatal("unconfigured quota enforced")
	}
	if e.Resolve(context.Background(), org, []quota.Item{{Quota: perProject.ID, ScopeKey: project}})[0].Enforced {
		t.Fatal("resolve enforced")
	}
}
