package service_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/cursor"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota/quotatest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

var (
	perOrg      = manifest.Quota{ID: "widgets.thingsPerOrganization", DisplayName: "Things", Kind: "allocation", Scope: "organization", Default: 3, Unit: "thing"}
	perPeriod   = manifest.Quota{ID: "widgets.api.requestsPerPeriod", DisplayName: "Requests", Kind: "usage", Scope: "organization", Default: 1000, Unit: "request"}
	concurrency = manifest.Quota{ID: "widgets.api.concurrentRequests", DisplayName: "In flight", Kind: "concurrency", Scope: "organization", Default: 10, Unit: "request", LeaseTTL: "PT1M"}
)

func quotasManifest() *manifest.Manifest {
	m := testManifest()
	api := []manifest.EndpointQuota{{ID: perPeriod.ID, Amount: 1}, {ID: concurrency.ID, Amount: 1}}
	m.Quotas = []manifest.Quota{perOrg, perPeriod, concurrency}
	m.Endpoints[2].Quotas = api
	m.Endpoints[3].Quotas = append(api, manifest.EndpointQuota{ID: perOrg.ID, Amount: 1})
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "GET", Path: "/v1/organizations/:orgId/things/:thingId", Auth: "caller", Action: "widgets.things.get", Quotas: api, Trail: &manifest.Trail{EventName: "GetThing"}},
		manifest.Endpoint{Method: "GET", Path: "/v1/organizations/:orgId/boom", Auth: "caller", Action: "widgets.things.get", Quotas: api, Trail: nil},
		manifest.Endpoint{Method: "GET", Path: "/internal/v1/quotas/usage", Auth: "service"},
	)
	return m
}

type quotasHarness struct {
	*harness
	quotas *quotatest.Fake
}

func newQuotasHarness(t *testing.T) *quotasHarness {
	m := quotasManifest()
	h := &quotasHarness{harness: &harness{db: testdb.New(t), org: &fakeOrgAPI{}}, quotas: quotatest.New(t, m)}
	orgServer := httptest.NewServer(h.org)
	t.Cleanup(orgServer.Close)
	enforcer := quota.NewEnforcer(quota.Options{Manifest: m, Region: "us-west-1", Logger: testdb.Logger(),
		Client: &quota.Client{URL: h.quotas.URL, InternalToken: "internal-secret-token"}})
	s := service.New(service.Options{
		Manifest: m, DB: h.db.DB, OrgAPI: &authz.Client{BaseURL: orgServer.URL, InternalToken: "t"},
		Cursors: cursor.NewSigner(strings.Repeat("k", 32)), Origins: httpapi.Origins{List: []string{goodOrigin}},
		Logger: testdb.Logger(), InternalToken: "internal-secret-token", Quotas: enforcer, OpenAPIOverlay: stubOverlay(m),
	})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		c.JSON(200, map[string]any{"things": []any{}, "leases": len(c.QuotaGrant().Leases())})
		return nil
	})
	s.Handle("POST", "/v1/organizations/:orgId/things", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		body, bad := httpapi.DecodeObject(c.Body(), false)
		if bad != nil {
			return bad
		}
		f := httpapi.NewFields(body)
		f.String("name", true)
		if err := f.Finish(); err != nil {
			return err
		}
		limit := c.QuotaLimit(perOrg.ID, c.OrganizationID)
		return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
			counter := quota.Counter{Quota: perOrg, OrganizationID: c.OrganizationID, ScopeKey: c.OrganizationID, ResourceName: c.Permissions().Organization.ResourceName}
			if err := quota.Reserve(c.Ctx, tx, counter, 1, limit); err != nil {
				return nil, err
			}
			return &service.Response{Status: 201, Body: map[string]any{"id": ids.NewV7()}}, nil
		})
	})
	s.Handle("GET", "/v1/organizations/:orgId/things/:thingId", func(c *service.Call) error {
		return c.AuthorizeMissing(c.Endpoint.Action, "Thing")
	})
	s.Handle("GET", "/v1/organizations/:orgId/boom", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		panic("boom")
	})
	s.Handle("GET", "/internal/v1/things", func(c *service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/quotas/usage", s.QuotaUsage)
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(handler)
	t.Cleanup(h.server.Close)
	return h
}

// released waits for the fire-and-forget releases of n leases.
func (h *quotasHarness) released(t *testing.T, n int) {
	t.Helper()
	for range 200 {
		if _, r := h.quotas.Snapshot(); len(r) == n && h.quotas.LeasesHeld(concurrency.ID, orgID) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, r := h.quotas.Snapshot()
	t.Fatalf("%d leases released, want %d; %d held", len(r), n, h.quotas.LeasesHeld(concurrency.ID, orgID))
}

func TestLeasesAreReleasedAfterTheResponse(t *testing.T) {
	h := newQuotasHarness(t)
	// A success.
	if r := h.do(t, "GET", things, "", member); r.status != 200 || r.body["leases"] != float64(1) {
		t.Fatalf("list %d %v", r.status, r.body)
	}
	h.released(t, 1)
	// A validation failure after the check.
	if r := h.do(t, "POST", things, `{"nope":1}`, member); r.status != 400 {
		t.Fatalf("invalid %d", r.status)
	}
	h.released(t, 2)
	// A panic.
	if r := h.do(t, "GET", "/v1/organizations/"+orgID+"/boom", "", member); r.status != 500 {
		t.Fatalf("boom %d", r.status)
	}
	h.released(t, 3)
	// An allocation refusal in the transaction.
	h.quotas.SetValue(perOrg.ID, "", 0)
	r := h.do(t, "POST", things, `{"name":"a"}`, member)
	if r.status != 409 || code(r) != "quota_exceeded" || r.header.Get("Retry-After") != "" {
		t.Fatalf("over the allocation %d %v", r.status, r.body)
	}
	h.released(t, 4)
	checks, _ := h.quotas.Snapshot()
	if len(checks) != 4 || checks[3].OrganizationID != orgID || checks[3].ProjectID != nil || checks[3].Region != "us-west-1" || len(checks[3].Items) != 3 {
		t.Errorf("checks %+v", checks)
	}
}

func TestAllocationUsesTheValueFromTheCheck(t *testing.T) {
	h := newQuotasHarness(t)
	h.quotas.SetValue(perOrg.ID, orgID, 2)
	for i := range 2 {
		if r := h.do(t, "POST", things, `{"name":"a"}`, member); r.status != 201 {
			t.Fatalf("create %d: %d %v", i, r.status, r.body)
		}
	}
	r := h.do(t, "POST", things, `{"name":"a"}`, member)
	q, _ := r.body["error"].(map[string]any)["quota"].(map[string]any)
	if r.status != 409 || q["limit"] != float64(2) || q["used"] != float64(2) || q["scopeResourceName"] != "ezgh::"+orgSlug || q["kind"] != "allocation" {
		t.Fatalf("third %d %v", r.status, r.body)
	}
	if msg := r.body["error"].(map[string]any)["message"]; msg != "This organization already has 2 things, and can have at most 2 ("+perOrg.ID+")" {
		t.Errorf("message %q", msg)
	}
	// Quotas down: the request proceeds, and the allocation still counts.
	h.quotas.Fail(503, "")
	if r := h.do(t, "POST", things, `{"name":"a"}`, member); r.status != 201 {
		t.Fatalf("failing open %d %v", r.status, r.body)
	}
	usage := h.do(t, "GET", "/internal/v1/quotas/usage?organizationId="+orgID, "", map[string]string{"X-Internal-Token": "internal-secret-token"})
	rows, _ := usage.body["usage"].([]any)
	if usage.status != 200 || len(rows) != 1 || rows[0].(map[string]any)["used"] != float64(3) || rows[0].(map[string]any)["region"] != "" || rows[0].(map[string]any)["scopeKey"] != orgID {
		t.Errorf("usage %d %v", usage.status, usage.body)
	}
}

func TestQuotaRefusalIs429WithoutRetryAfterAndRecorded(t *testing.T) {
	h := newQuotasHarness(t)
	h.quotas.SetValue(perPeriod.ID, "", 1)
	if r := h.do(t, "GET", things, "", member); r.status != 200 {
		t.Fatal(r.status)
	}
	r := h.do(t, "POST", things, `{"name":"a"}`, member)
	if r.status != 429 || code(r) != "quota_exceeded" || r.header.Get("Retry-After") != "" {
		t.Fatalf("%d %v %v", r.status, r.body, r.header)
	}
	q := r.body["error"].(map[string]any)["quota"].(map[string]any)
	if q["kind"] != "usage" || q["used"] != float64(1) || q["limit"] != float64(1) || q["resetsAt"] == nil || q["scopeResourceName"] != "ezgh::"+orgSlug {
		t.Errorf("quota %v", q)
	}
	var n int
	_ = h.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM quota_usage`).Scan(&n)
	if n != 0 {
		t.Error("the handler ran after a refusal")
	}
	events := h.events(t)
	last := events[len(events)-1]
	if last.EventName != "CreateThing" || last.ErrorCode == nil || *last.ErrorCode != "quota_exceeded" {
		t.Errorf("recorded %+v", last)
	}
}

// Nothing is asked of Quotas for an endpoint without quotas, a request refused before
// authorization, or a missing resource (404 whatever org-api says).
func TestNoCheckWhenNotNeeded(t *testing.T) {
	h := newQuotasHarness(t)
	h.do(t, "GET", "/internal/v1/things", "", map[string]string{"X-Internal-Token": "internal-secret-token"})
	h.do(t, "GET", things, "", map[string]string{"Cookie": "better-auth.session_token=outsider"})
	h.do(t, "GET", things, "", map[string]string{"Cookie": "better-auth.session_token=denied"})
	if r := h.do(t, "GET", things+"/"+ids.NewV7(), "", member); r.status != 404 {
		t.Fatalf("missing %d", r.status)
	}
	if checks, _ := h.quotas.Snapshot(); len(checks) != 0 {
		t.Errorf("%d checks", len(checks))
	}
}

func TestQuotaUsageEndpoint(t *testing.T) {
	h := newQuotasHarness(t)
	token := map[string]string{"X-Internal-Token": "internal-secret-token"}
	if r := h.do(t, "GET", "/internal/v1/quotas/usage?organizationId="+orgID, "", nil); r.status != 401 {
		t.Errorf("no credential %d", r.status)
	}
	r := h.do(t, "GET", "/internal/v1/quotas/usage?organizationId=x&other=1", "", token)
	if r.status != 400 || len(r.body["error"].(map[string]any)["issues"].([]any)) != 2 {
		t.Errorf("bad query %d %v", r.status, r.body)
	}
	r = h.do(t, "GET", "/internal/v1/quotas/usage?organizationId="+orgID, "", token)
	if r.status != 200 || len(r.body["usage"].([]any)) != 0 {
		t.Errorf("empty %d %v", r.status, r.body)
	}
}
