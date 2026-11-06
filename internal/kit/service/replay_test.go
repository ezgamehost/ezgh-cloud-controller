package service_test

import (
	"strings"
	"testing"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

const mints = "/v1/organizations/" + orgID + "/things/mint"

// A response carrying a one-time secret replays without it (contracts/api-conventions.md,
// "Idempotency keys"): the secret is never stored in idempotency_keys.
func TestReplayBodyKeepsSecretsOutOfReplays(t *testing.T) {
	extra := []manifest.Endpoint{{
		Method: "POST", Path: "/v1/organizations/:orgId/things/mint", Auth: "caller", Action: "widgets.things.create",
		IdempotencyKey: true, Trail: &manifest.Trail{EventName: "MintThing"},
	}}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/things/mint", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: 201,
					Body:       map[string]any{"id": "x", "secret": "whsec_one-time"},
					ReplayBody: map[string]any{"id": "x", "secret": nil}}, nil
			})
		})
	})
	headers := memberHeaders()
	headers["Idempotency-Key"] = "k-replay-1"
	first := h.do(t, "POST", mints, `{}`, headers)
	if first.status != 201 || first.body["secret"] != "whsec_one-time" {
		t.Fatalf("first: %d %v", first.status, first.body)
	}
	again := h.do(t, "POST", mints, `{}`, headers)
	if again.status != 201 || again.body["id"] != "x" || again.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v %v", again.status, again.body, again.header)
	}
	if secret, ok := again.body["secret"]; !ok || secret != nil {
		t.Fatalf("the replay carried the secret: %v", again.body)
	}
	var stored string
	if err := h.db.Pool.QueryRow(t.Context(), `SELECT body::text FROM idempotency_keys WHERE key = 'k-replay-1'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "whsec_") {
		t.Fatalf("the secret was stored: %s", stored)
	}
	for _, ev := range h.events(t) {
		if strings.Contains(string(ev.UserIdentity)+stringOf(ev.Response), "whsec_") {
			t.Fatalf("the secret was recorded: %+v", ev)
		}
	}
}

func stringOf(v map[string]any) string {
	var b strings.Builder
	for k, x := range v {
		b.WriteString(k)
		if s, ok := x.(string); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}
