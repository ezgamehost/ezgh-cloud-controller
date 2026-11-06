package service_test

import (
	"testing"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

// A deadlock (40P01) or serialization failure (40001) in the request's transaction is 503 with
// Retry-After, never 500; nothing is committed, and the kit runs the handler's fn once.
func TestTransactContentionIs503(t *testing.T) {
	for _, sqlState := range []string{"40P01", "40001"} {
		runs := 0
		extra := []manifest.Endpoint{{Method: "POST", Path: "/v1/organizations/:orgId/contended", Auth: "caller", Action: "widgets.things.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "Contend"}}}
		h := newHarnessWith(t, nil, extra, func(s *service.Service) {
			s.Handle("POST", "/v1/organizations/:orgId/contended", func(c *service.Call) error {
				if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
					return err
				}
				return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
					runs++
					_, err := tx.ExecContext(c.Ctx, `DO $$ BEGIN RAISE EXCEPTION 'forced' USING ERRCODE = '`+sqlState+`'; END $$`)
					return &service.Response{Status: 201, Body: map[string]any{"id": "x"}}, err
				})
			})
		})
		r := h.do(t, "POST", "/v1/organizations/"+orgID+"/contended", `{}`, member)
		if r.status != 503 || code(r) != "unavailable" || r.header.Get("Retry-After") != "1" || runs != 1 {
			t.Errorf("%s: %d %s Retry-After %q, %d runs", sqlState, r.status, code(r), r.header.Get("Retry-After"), runs)
		}
		ev := h.events(t)
		if len(ev) != 1 || ev[0].ErrorCode == nil || *ev[0].ErrorCode != "unavailable" {
			t.Errorf("%s: events %+v", sqlState, ev)
		}
	}
}
