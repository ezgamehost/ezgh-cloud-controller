package service_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// replayQuotaHarness serves keyed creates that check their quotas each way a handler can: through
// Authorize, Check then CheckQuotas, CheckCreate then CheckQuotaItems, and CheckQuotaItems inside
// TransactPrepared's prepare.
func replayQuotaHarness(t *testing.T) *quotasHarness {
	t.Helper()
	m := quotasManifest()
	api := []manifest.EndpointQuota{{ID: perPeriod.ID, Amount: 1}, {ID: concurrency.ID, Amount: 1}}
	for _, path := range []string{"checked", "planned", "prepared"} {
		m.Endpoints = append(m.Endpoints, manifest.Endpoint{
			Method: "POST", Path: "/v1/organizations/:orgId/" + path, Auth: "caller", Action: "widgets.things.create",
			IdempotencyKey: true, Quotas: api, Trail: &manifest.Trail{EventName: "Create" + path},
		})
	}
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
	created := func(tx *kitdb.Tx) (*service.Response, error) {
		return &service.Response{Status: 201, Body: map[string]any{"id": ids.NewV7()}}, nil
	}
	extra := []quota.PlanItem{{Quota: perPeriod, Amount: 1}}
	s.Handle("GET", "/v1/organizations/:orgId/things", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		c.JSON(200, map[string]any{"things": []any{}})
		return nil
	})
	s.Handle("POST", "/v1/organizations/:orgId/things", func(c *service.Call) error {
		// A writer the handler installs (sso-stack's unsealing writer) goes in before Authorize,
		// which answers a replay itself.
		c.W = wrappedWriter{c.W}
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		return c.Transact(created)
	})
	s.Handle("POST", "/v1/organizations/:orgId/checked", func(c *service.Call) error {
		if err := allowed(c.Check([]string{c.Endpoint.Action}, "")); err != nil {
			return err
		}
		if err := c.CheckQuotas(); err != nil {
			return err
		}
		return c.Transact(created)
	})
	s.Handle("POST", "/v1/organizations/:orgId/planned", func(c *service.Call) error {
		if err := allowed(c.CheckCreate([]string{c.Endpoint.Action}, "ezgh:us-west-1:"+orgSlug+":prj_g7h8i9j0k1l2:wgt_4k2m9x0a7qpe")); err != nil {
			return err
		}
		if err := c.CheckQuotaItems(extra); err != nil {
			return err
		}
		return c.Transact(created)
	})
	s.Handle("POST", "/v1/organizations/:orgId/prepared", func(c *service.Call) error {
		if err := allowed(c.Check([]string{c.Endpoint.Action}, "")); err != nil {
			return err
		}
		return c.TransactPrepared(func() error { return c.CheckQuotaItems(extra) }, created)
	})
	s.Handle("GET", "/v1/organizations/:orgId/things/:thingId", func(c *service.Call) error {
		return c.AuthorizeMissing(c.Endpoint.Action, "Thing")
	})
	s.Handle("GET", "/v1/organizations/:orgId/boom", func(c *service.Call) error { return nil })
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

// A retry with the request's Idempotency-Key gets its stored response before any quota check
// counts it or refuses it: at the limit, it's the stored 201, not 429, whichever way the handler
// checks its quotas, and Quotas isn't asked again (contracts/api-conventions.md, "Idempotency keys").
func TestReplayComesBeforeQuotaChecks(t *testing.T) {
	for _, path := range []string{"things", "checked", "planned", "prepared"} {
		t.Run(path, func(t *testing.T) {
			h := replayQuotaHarness(t)
			// One request a period: the first spends it.
			h.quotas.SetValue(perPeriod.ID, "", 1)
			url := "/v1/organizations/" + orgID + "/" + path
			first := h.do(t, "POST", url, `{"name":"a"}`, withKey(member, "q-1"))
			if first.status != 201 {
				t.Fatalf("first: %d %v", first.status, first.body)
			}
			checks, _ := h.quotas.Snapshot()
			asked := len(checks)
			again := h.do(t, "POST", url, `{"name":"a"}`, withKey(member, "q-1"))
			if again.status != 201 || again.header.Get("Idempotent-Replayed") != "true" || again.body["id"] != first.body["id"] {
				t.Fatalf("replay at the limit: %d %v %v", again.status, again.body, again.header)
			}
			if checks, _ := h.quotas.Snapshot(); len(checks) != asked {
				t.Fatalf("the replay asked Quotas %d more times", len(checks)-asked)
			}
			// The create and its replay are one event each, both successes.
			var events []string
			for _, ev := range h.events(t) {
				events = append(events, ev.EventName+":"+fmt.Sprint(ev.ErrorCode != nil))
			}
			if len(events) != 2 || events[0] != events[1] || strings.HasSuffix(events[0], "true") {
				t.Fatalf("events %v", events)
			}
			// A new request at the limit is still refused.
			if r := h.do(t, "POST", url, `{"name":"b"}`, withKey(member, "q-2")); r.status != 429 || code(r) != "quota_exceeded" {
				t.Fatalf("a new request: %d %v", r.status, r.body)
			}
			// The same key with another request is a conflict, not a quota refusal.
			if r := h.do(t, "POST", url, `{"name":"c"}`, withKey(member, "q-1")); r.status != 409 || code(r) != "idempotency_conflict" {
				t.Fatalf("another request: %d %v", r.status, r.body)
			}
			// A caller who may not create is refused before anything is replayed.
			denied := map[string]string{"Cookie": "better-auth.session_token=denied", "Origin": goodOrigin, "Idempotency-Key": "q-1"}
			if r := h.do(t, "POST", url, `{"name":"a"}`, denied); r.header.Get("Idempotent-Replayed") != "" || r.status < 400 {
				t.Fatalf("denied: %d %v %v", r.status, r.body, r.header)
			}
		})
	}
}

// allowed is a Check's refusal of the create action.
func allowed(perm *authz.Permissions, err error) error {
	if err != nil {
		return err
	}
	if !perm.Allows("widgets.things.create") {
		return httpapi.AccessDenied("widgets.things.create", false)
	}
	return nil
}

// wrappedWriter marks the responses written through it.
type wrappedWriter struct{ http.ResponseWriter }

func (w wrappedWriter) WriteHeader(status int) {
	w.Header().Set("X-Wrapped", "yes")
	w.ResponseWriter.WriteHeader(status)
}

// A replay a quota check answers goes through the writer the handler installed before
// authorizing, and stops the handler there: nothing after Authorize runs again.
func TestReplayAtTheQuotaCheckUsesTheHandlersWriter(t *testing.T) {
	h := replayQuotaHarness(t)
	h.quotas.SetValue(perPeriod.ID, "", 1)
	url := "/v1/organizations/" + orgID + "/things"
	if r := h.do(t, "POST", url, `{"name":"a"}`, withKey(member, "w-1")); r.status != 201 || r.header.Get("X-Wrapped") != "yes" {
		t.Fatalf("first: %d %v %v", r.status, r.body, r.header)
	}
	if r := h.do(t, "POST", url, `{"name":"a"}`, withKey(member, "w-1")); r.status != 201 || r.header.Get("Idempotent-Replayed") != "true" || r.header.Get("X-Wrapped") != "yes" {
		t.Fatalf("replay: %d %v %v", r.status, r.body, r.header)
	}
}
