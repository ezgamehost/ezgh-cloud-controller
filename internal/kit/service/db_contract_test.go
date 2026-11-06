package service_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/cursor"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/testent"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// The data layer's contract with Transact (kit/go/db/contract_test.go has the rest).

// dataService serves POST things with a Transact that writes through the kit's ent client, a
// second generated client (a product's, here the test one), quota.Reserve and, through Transact
// itself, the idempotency record and the Trails event: all on one transaction. mode "fn fails"
// returns an error after writing; "ent rolls back" swallows an ent UpdateOne's NotFound, which
// poisons the transaction without failing a statement, so Transact goes on to store the key and
// record the event, and only the commit is refused (db.ErrJoinedRollback).
func dataService(t *testing.T, d *kitdb.DB, h *harness, mode string) {
	t.Helper()
	orgServer := httptest.NewServer(h.org)
	t.Cleanup(orgServer.Close)
	s := service.New(service.Options{
		Manifest: testManifest(), DB: d, OrgAPI: &authz.Client{BaseURL: orgServer.URL, InternalToken: "t"},
		Cursors: cursor.NewSigner(strings.Repeat("k", 32)), Origins: httpapi.Origins{List: []string{goodOrigin}},
		Logger: testdb.Logger(), InternalToken: "internal-secret-token", OpenAPIOverlay: stubOverlay(testManifest()),
	})
	counter := quota.Counter{Quota: manifest.Quota{ID: "widgets.thingsPerOrganization", Kind: manifest.Allocation}, OrganizationID: orgID, ScopeKey: orgID}
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
	s.Handle("POST", "/v1/organizations/:orgId/things", func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
			if err := entdb.Client(tx).OrganizationClosure.Create().SetID(orgID).SetCompleteThrough(time.Now()).Exec(c.Ctx); err != nil {
				return nil, err
			}
			id := ids.NewV7()
			if err := testent.NewClient(testent.Driver(tx.Driver())).ContractThing.Create().SetID(id).SetName("a").Exec(c.Ctx); err != nil {
				return nil, err
			}
			if err := quota.Reserve(c.Ctx, tx, counter, 1, quota.Limit{}); err != nil {
				return nil, err
			}
			switch mode {
			case "fn fails":
				return nil, errors.New("fn failed after writing")
			case "ent rolls back":
				if _, err := entdb.Client(tx).UsageWatermark.UpdateOneID("nowhere").SetCompleteThrough(time.Now()).Save(c.Ctx); err == nil {
					return nil, errors.New("UpdateOne found nowhere")
				}
			}
			return &service.Response{Status: 201, Body: map[string]any{"id": id}}, nil
		})
	})
	handler, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(handler)
	t.Cleanup(h.server.Close)
}

// Inside Transact, an insert from a second generated ent client, the kit's ent client,
// quota.Reserve, the idempotency record and the Trails event all commit together, and are all
// absent when fn fails, an ent operation in it rolls back (fn swallowing its error), or the commit
// itself fails. Each failure is then recorded in Trails in a transaction of its own.
func TestTransactIsAtomicAcrossClients(t *testing.T) {
	for _, mode := range []string{"commits", "fn fails", "ent rolls back", "commit fails"} {
		t.Run(mode, func(t *testing.T) {
			h := &harness{db: testdb.New(t), org: &fakeOrgAPI{}}
			if _, err := h.db.Pool.Exec(context.Background(), `CREATE TABLE contract_things (id uuid PRIMARY KEY, name text NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if mode == "commit fails" {
				if _, err := h.db.Pool.Exec(context.Background(), `CREATE FUNCTION contract_reject() RETURNS trigger LANGUAGE plpgsql AS $$
					BEGIN RAISE EXCEPTION 'rejected at commit'; END $$;
					CREATE CONSTRAINT TRIGGER contract_reject AFTER INSERT ON contract_things DEFERRABLE INITIALLY DEFERRED
					FOR EACH ROW EXECUTE FUNCTION contract_reject()`); err != nil {
					t.Fatal(err)
				}
			}
			dataService(t, h.db.DB, h, mode)
			r := h.do(t, "POST", things, `{}`, map[string]string{"Cookie": "better-auth.session_token=member", "Origin": goodOrigin, "Idempotency-Key": "k-1"})
			want, wantStatus := int64(0), 500
			if mode == "commits" {
				want, wantStatus = 1, 201
			}
			if r.status != wantStatus {
				t.Fatalf("status %d: %v", r.status, r.body)
			}
			for _, q := range []string{
				`SELECT count(*) FROM organization_closures`,
				`SELECT count(*) FROM contract_things`,
				`SELECT coalesce(sum(used), 0) FROM quota_usage`,
				`SELECT count(*) FROM quota_usage`,
				`SELECT count(*) FROM idempotency_keys`,
				`SELECT count(*) FROM audit_outbox WHERE event->>'errorCode' IS NULL`,
			} {
				var n int64
				if err := h.db.Pool.QueryRow(context.Background(), q).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n != want {
					t.Errorf("%s = %d, want %d", q, n, want)
				}
			}
			// A failure is recorded after the rollback, in a transaction of its own.
			if mode != "commits" {
				events := h.events(t)
				if len(events) != 1 || events[0].ErrorCode == nil || *events[0].ErrorCode != "internal_error" {
					t.Errorf("the failure wasn't recorded on its own: %+v", events)
				}
			}
		})
	}
}

// A database that can't be reached is 503, at /ready and in Transact.
func TestUnreachableDatabaseIs503(t *testing.T) {
	bad, err := kitdb.Open(context.Background(), "postgres://postgres:postgres@127.0.0.1:1/none?connect_timeout=2", kitdb.PoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bad.Close)
	h := &harness{org: &fakeOrgAPI{}}
	dataService(t, bad, h, "commits")
	if r := h.do(t, "GET", "/ready", "", nil); r.status != 503 || code(r) != "unavailable" {
		t.Errorf("/ready: %d %v", r.status, r.body)
	}
	if r := h.do(t, "POST", things, `{}`, member); r.status != 503 || code(r) != "unavailable" {
		t.Errorf("Transact: %d %v", r.status, r.body)
	}
}
