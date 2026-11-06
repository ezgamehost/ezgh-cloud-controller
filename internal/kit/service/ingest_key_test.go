package service_test

import "testing"

// A project ingest key (contracts/identity-and-authz.md, "Project ingest keys"): no product route takes it.
const ingestKey = "ezgh_ik_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"

// A project ingest key is 401 on every product route, public and internal, without asking
// org-api, falling back to the good cookie beside it, or recording anything.
func TestIngestKeysAre401OnProductRoutes(t *testing.T) {
	h := newHarness(t)
	for _, req := range []struct{ method, path, body string }{
		{"GET", things, ""},
		{"POST", things, `{"name":"x"}`},
		{"GET", "/internal/v1/things", ""},
	} {
		r := h.do(t, req.method, req.path, req.body, map[string]string{"Authorization": "Bearer " + ingestKey, "Cookie": "better-auth.session_token=member", "Origin": goodOrigin})
		if r.status != 401 || code(r) != "unauthenticated" {
			t.Errorf("%s %s: %d %v", req.method, req.path, r.status, r.body)
		}
	}
	if h.org.calls != 0 {
		t.Errorf("org-api was asked %d times", h.org.calls)
	}
	if ev := h.events(t); len(ev) != 0 {
		t.Errorf("%d events recorded", len(ev))
	}
}
