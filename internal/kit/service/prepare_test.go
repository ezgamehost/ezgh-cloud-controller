package service_test

import (
	"context"
	"errors"
	"testing"
	"time"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

const slows = "/v1/organizations/" + orgID + "/things/slow"

// TransactPrepared's prepare (a slow network call: Workflows' sync to a customer endpoint) holds
// no database connection, with or without an Idempotency-Key: on a one-connection pool, prepare
// can still query the database. With a key, a second request arriving during prepare is 409
// idempotency_in_progress, a failed prepare frees the key for a retry, and a replay skips prepare.
func TestPrepareHoldsNoConnection(t *testing.T) {
	extra := []manifest.Endpoint{{
		Method: "POST", Path: "/v1/organizations/:orgId/things/slow", Auth: "caller", Action: "widgets.things.create",
		IdempotencyKey: true, Trail: &manifest.Trail{EventName: "SlowThing"},
	}}
	var h *harness
	prepared := 0
	var during []int
	h = newHarnessSized(t, 1, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/things/slow", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			return c.TransactPrepared(func() error {
				prepared++
				ctx, cancel := context.WithTimeout(c.Ctx, 3*time.Second)
				defer cancel()
				var one int
				if err := c.Service.DB.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
					return httpapi.Internal(errors.New("prepare couldn't reach the pool: " + err.Error()))
				}
				if key := c.R.Header.Get("Idempotency-Key"); key != "" && string(c.Body()) == `{"probe":true}` {
					headers := memberHeaders()
					headers["Idempotency-Key"] = key
					during = append(during, h.do(t, "POST", slows, `{"probe":true}`, headers).status)
				}
				if string(c.Body()) == `{"fail":true}` {
					return httpapi.Conflict("not_yet", "Not yet")
				}
				return nil
			}, func(tx *kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: 201, Body: map[string]any{"id": "x"}}, nil
			})
		})
	})
	if r := h.do(t, "POST", slows, `{}`, memberHeaders()); r.status != 201 {
		t.Fatalf("without a key: %d %v", r.status, r.body)
	}
	keyed := memberHeaders()
	keyed["Idempotency-Key"] = "k-slow-1"
	if r := h.do(t, "POST", slows, `{"probe":true}`, keyed); r.status != 201 {
		t.Fatalf("with a key: %d %v", r.status, r.body)
	}
	if len(during) != 1 || during[0] != 409 {
		t.Fatalf("a request with the same key during prepare: %v", during)
	}
	if r := h.do(t, "POST", slows, `{"probe":true}`, keyed); r.status != 201 || r.header.Get("Idempotent-Replayed") != "true" || prepared != 2 {
		t.Fatalf("replay: %d %v %v, prepared %d", r.status, r.body, r.header, prepared)
	}
	retry := memberHeaders()
	retry["Idempotency-Key"] = "k-slow-2"
	if r := h.do(t, "POST", slows, `{"fail":true}`, retry); r.status != 409 || code(r) != "not_yet" {
		t.Fatalf("failing prepare: %d %v", r.status, r.body)
	}
	if r := h.do(t, "POST", slows, `{"fail":true}`, retry); code(r) != "not_yet" || prepared != 4 {
		t.Fatalf("the failed key wasn't free for a retry: %d %v, prepared %d", r.status, r.body, prepared)
	}
}

// A claim left by a crashed request lapses: a live one is 409, an expired one is taken over. A
// request that runs no prepare is kept out by a live claim too, so it can't complete the key
// under the claiming request. Purging and an organization's deletion clear claims.
func TestIdempotencyClaimLapses(t *testing.T) {
	h := newPreparedHarness(t, func() error { return nil })
	claim := func(key, expires string) {
		t.Helper()
		if _, err := h.db.DB.ExecContext(context.Background(), `INSERT INTO idempotency_claims (organization_id, key, token, expires_at)
			VALUES ($1, $2, '0199a3c2-0000-7000-8000-000000000001', now() + $3::interval)`, orgID, key, expires); err != nil {
			t.Fatal(err)
		}
	}
	path := "/v1/organizations/" + orgID + "/things"
	claim("k-live", "1 minute")
	claim("k-stale", "-1 second")
	live := memberHeaders()
	live["Idempotency-Key"] = "k-live"
	if r := h.do(t, "POST", path, `{"name":"one"}`, live); r.status != 409 || code(r) != "idempotency_in_progress" || r.header.Get("Retry-After") != "1" {
		t.Fatalf("a live claim: %d %v", r.status, r.body)
	}
	stale := memberHeaders()
	stale["Idempotency-Key"] = "k-stale"
	if r := h.do(t, "POST", path, `{"name":"one"}`, stale); r.status != 201 {
		t.Fatalf("a lapsed claim: %d %v", r.status, r.body)
	}
	var claims int
	count := func() int {
		t.Helper()
		if err := h.db.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM idempotency_claims`).Scan(&claims); err != nil {
			t.Fatal(err)
		}
		return claims
	}
	if n := count(); n != 1 {
		t.Fatalf("claims left: %d (the live one only)", n)
	}
	if err := h.db.DB.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error { return service.DeleteIdempotencyKeys(context.Background(), tx, orgID) }); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("claims after the organization's deletion: %d", n)
	}
	claim("k-old", "-1 second")
	if _, err := service.PurgeIdempotencyKeys(context.Background(), h.db.DB, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("lapsed claims after a purge: %d", n)
	}
}

// Without prepare, a keyed write never touches idempotency_claims (a product whose runtime role
// isn't granted it yet still works): a live claim doesn't block it, nor does the table's absence.
// Cross-endpoint key reuse is caught by the fingerprint instead.
func TestTransactWithoutPrepareIgnoresClaims(t *testing.T) {
	h := newHarness(t)
	if _, err := h.db.DB.ExecContext(context.Background(), `INSERT INTO idempotency_claims (organization_id, key, token, expires_at)
		VALUES ($1, 'k-held', '0199a3c2-0000-7000-8000-000000000002', now() + interval '1 minute')`, orgID); err != nil {
		t.Fatal(err)
	}
	headers := memberHeaders()
	headers["Idempotency-Key"] = "k-held"
	if r := h.do(t, "POST", "/v1/organizations/"+orgID+"/things", `{"name":"one"}`, headers); r.status != 201 {
		t.Fatalf("a live claim blocked a write without prepare: %d %v", r.status, r.body)
	}
	if _, err := h.db.DB.ExecContext(context.Background(), `ALTER TABLE idempotency_claims RENAME TO idempotency_claims_gone`); err != nil {
		t.Fatal(err)
	}
	headers["Idempotency-Key"] = "k-no-table"
	if r := h.do(t, "POST", "/v1/organizations/"+orgID+"/things", `{"name":"two"}`, headers); r.status != 201 {
		t.Fatalf("without the claims table: %d %v", r.status, r.body)
	}
	if r := h.do(t, "POST", "/v1/organizations/"+orgID+"/things", `{"name":"two"}`, headers); r.status != 201 || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay without the claims table: %d %v", r.status, r.body)
	}
	// The purges and an organization's deletion skip a claims table the role can't reach.
	if _, err := service.PurgeIdempotencyKeys(context.Background(), h.db.DB, 24*time.Hour); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := h.db.DB.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error { return service.DeleteIdempotencyKeys(context.Background(), tx, orgID) }); err != nil {
		t.Fatalf("organization deletion: %v", err)
	}
}

// A keyed prepare runs under a deadline below its claim's lease (the endpoint's timeoutSeconds,
// or 60 s), so it can't outlive the claim and let a second request in.
func TestPrepareDeadlineIsBelowTheLease(t *testing.T) {
	extra := []manifest.Endpoint{{
		Method: "POST", Path: "/v1/organizations/:orgId/things/slow", Auth: "caller", Action: "widgets.things.create",
		IdempotencyKey: true, Trail: &manifest.Trail{EventName: "SlowThing"},
	}}
	var h *harness
	var deadline, expires time.Time
	var hadDeadline, after bool
	h = newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/things/slow", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			err := c.TransactPrepared(func() error {
				deadline, hadDeadline = c.Ctx.Deadline()
				return h.db.DB.QueryRowContext(context.Background(), `SELECT expires_at FROM idempotency_claims WHERE key = 'k-lease'`).Scan(&expires)
			}, func(tx *kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: 201, Body: map[string]any{"id": "x"}}, nil
			})
			_, after = c.Ctx.Deadline()
			return err
		})
	})
	headers := memberHeaders()
	headers["Idempotency-Key"] = "k-lease"
	started := time.Now()
	if r := h.do(t, "POST", slows, `{}`, headers); r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	if left := deadline.Sub(started); !hadDeadline || left < 55*time.Second || left > 61*time.Second || !deadline.Before(expires.Add(-30*time.Second)) {
		t.Fatalf("prepare's deadline %v (set %v), the claim's lease %v", deadline, hadDeadline, expires)
	}
	if after {
		t.Error("the prepare deadline outlived prepare")
	}
}
