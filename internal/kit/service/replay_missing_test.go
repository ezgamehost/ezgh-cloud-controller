package service_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

const (
	gadgetA = "0199a3d0-1a2b-7c3d-8e4f-5a6b7c8d9e01"
	gadgetB = "0199a3d0-1a2b-7c3d-8e4f-5a6b7c8d9e02"
	gadgets = "/v1/organizations/" + orgID + "/gadgets/"
)

// gadgetHarness serves gadgets that a DELETE removes and a stop stops, the way products do: the
// handler resolves its target, and refuses a missing one or one in the wrong state, before its
// transaction.
func gadgetHarness(t *testing.T) *harness { return gadgetHarnessPrepared(t, nil) }

// gadgetHarnessPrepared is gadgetHarness whose POST /things runs prepare before its transaction.
func gadgetHarnessPrepared(t *testing.T, prepare func() error) *harness {
	t.Helper()
	var mu sync.Mutex
	exists := map[string]bool{gadgetA: true, gadgetB: true}
	stopped := map[string]bool{}
	extra := []manifest.Endpoint{
		{Method: "DELETE", Path: "/v1/organizations/:orgId/gadgets/:gadgetId", Auth: "caller", Action: "widgets.gadgets.delete", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "DeleteGadget"}},
		{Method: "POST", Path: "/v1/organizations/:orgId/gadgets/:gadgetId/stop", Auth: "caller", Action: "widgets.gadgets.stop", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "StopGadget"}},
		// Checks a read action on a missing target, as the operations helper's cancel does.
		{Method: "DELETE", Path: "/v1/organizations/:orgId/gizmos/:gizmoId", Auth: "caller", Action: "widgets.gizmos.delete", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "DeleteGizmo"}},
		// Asks org-api, ignores the answer, and replays: only an allowed caller gets the stored response.
		{Method: "POST", Path: "/v1/organizations/:orgId/gadgets/:gadgetId/peek", Auth: "caller", Action: "widgets.gadgets.peek", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "PeekGadget"}},
		// Resolves its target inside the transaction.
		{Method: "DELETE", Path: "/v1/organizations/:orgId/gadgets/:gadgetId/late", Auth: "caller", Action: "widgets.gadgets.delete", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "DeleteGadgetLate"}},
		// Learns its target is gone from another service, in TransactPrepared's prepare.
		{Method: "DELETE", Path: "/v1/organizations/:orgId/gadgets/:gadgetId/remote", Auth: "caller", Action: "widgets.gadgets.delete", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "DeleteGadgetRemote"}},
	}
	return newHarnessWith(t, prepare, extra, func(s *service.Service) {
		gizmos := map[string]bool{gadgetA: true}
		s.Handle("DELETE", "/v1/organizations/:orgId/gizmos/:gizmoId", func(c *service.Call) error {
			id := c.Param("gizmoId")
			mu.Lock()
			ok := gizmos[id]
			mu.Unlock()
			if !ok {
				return c.AuthorizeMissing("widgets.gizmos.list", "Gizmo")
			}
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				mu.Lock()
				delete(gizmos, id)
				mu.Unlock()
				return &service.Response{Status: http.StatusNoContent}, nil
			})
		})
		s.Handle("POST", "/v1/organizations/:orgId/gadgets/:gadgetId/peek", func(c *service.Call) error {
			if _, err := c.Check([]string{c.Endpoint.Action}, ""); err != nil {
				return err
			}
			if err := c.Replay(); err != nil {
				return err
			}
			if !c.Permissions().Allows(c.Endpoint.Action) {
				return httpapi.Conflict("not_replayed", "Nothing was replayed")
			}
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: http.StatusOK, Body: map[string]any{"peeked": c.Principal().ID}}, nil
			})
		})
		s.Handle("DELETE", "/v1/organizations/:orgId/gadgets/:gadgetId/remote", func(c *service.Call) error {
			return c.TransactPrepared(func() error {
				return c.AuthorizeMissing(c.Endpoint.Action, "Gadget")
			}, func(tx *kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: http.StatusNoContent}, nil
			})
		})
		s.Handle("DELETE", "/v1/organizations/:orgId/gadgets/:gadgetId/late", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				return nil, c.AuthorizeMissing(c.Endpoint.Action, "Gadget")
			})
		})
		s.Handle("DELETE", "/v1/organizations/:orgId/gadgets/:gadgetId", func(c *service.Call) error {
			id := c.Param("gadgetId")
			mu.Lock()
			ok := exists[id]
			mu.Unlock()
			if !ok {
				return c.AuthorizeMissing(c.Endpoint.Action, "Gadget")
			}
			if err := c.Authorize(c.Endpoint.Action, gadgetName(id)); err != nil {
				return err
			}
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				mu.Lock()
				delete(exists, id)
				mu.Unlock()
				return &service.Response{Status: http.StatusNoContent}, nil
			})
		})
		s.Handle("POST", "/v1/organizations/:orgId/gadgets/:gadgetId/stop", func(c *service.Call) error {
			id := c.Param("gadgetId")
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			if err := c.Replay(); err != nil {
				return err
			}
			mu.Lock()
			already := stopped[id]
			mu.Unlock()
			if already {
				return httpapi.Conflict("gadget_not_running", "The gadget isn't running")
			}
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				mu.Lock()
				stopped[id] = true
				mu.Unlock()
				return &service.Response{Status: http.StatusAccepted, Body: map[string]any{"id": id, "status": "stopping"}}, nil
			})
		})
	})
}

// gadgetName is a gadget's resource name: another product's (gdg_ isn't in the manifest).
func gadgetName(id string) string {
	return "ezgh:us-west-1:" + orgSlug + ":gdg_" + strings.ReplaceAll(id, "-", "")[20:]
}

func withKey(headers map[string]string, key string) map[string]string {
	out := map[string]string{"Idempotency-Key": key}
	for k, v := range headers {
		out[k] = v
	}
	return out
}

// N1/B1: a DELETE retried with its Idempotency-Key, once its target is gone, gets the stored 204
// with Idempotent-Replayed, not 404 (contracts/api-conventions.md, "Idempotency keys").
func TestDeleteReplayAfterTheTargetIsGone(t *testing.T) {
	h := gadgetHarness(t)
	first := h.do(t, "DELETE", gadgets+gadgetA, "", withKey(memberHeaders(), "del-1"))
	if first.status != 204 {
		t.Fatalf("first: %d %v", first.status, first.body)
	}
	again := h.do(t, "DELETE", gadgets+gadgetA, "", withKey(memberHeaders(), "del-1"))
	if again.status != 204 || again.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v %v", again.status, again.body, again.header)
	}
	// Without the key it's a new request: the gadget is gone.
	if r := h.do(t, "DELETE", gadgets+gadgetA, "", memberHeaders()); r.status != 404 || code(r) != "not_found" {
		t.Fatalf("no key: %d %v", r.status, r.body)
	}
	// The key on another request is a conflict, not a replay.
	h.do(t, "DELETE", gadgets+gadgetB, "", memberHeaders())
	if r := h.do(t, "DELETE", gadgets+gadgetB, "", withKey(memberHeaders(), "del-1")); r.status != 409 || code(r) != "idempotency_conflict" {
		t.Fatalf("another request: %d %v", r.status, r.body)
	}
	// A caller who may not delete gets the 404 a missing gadget always gets, never the stored answer.
	denied := map[string]string{"Cookie": "better-auth.session_token=denied", "Origin": goodOrigin, "Idempotency-Key": "del-1"}
	if r := h.do(t, "DELETE", gadgets+gadgetA, "", denied); r.status != 404 || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("denied: %d %v %v", r.status, r.body, r.header)
	}
	// Each call is its own event: the delete and its replay succeeded.
	var deletes, failures int
	for _, ev := range h.events(t) {
		if ev.EventName != "DeleteGadget" {
			continue
		}
		if ev.ErrorCode == nil {
			deletes++
		} else {
			failures++
		}
	}
	if deletes != 3 || failures != 3 {
		t.Fatalf("recorded %d successes, %d failures", deletes, failures)
	}
}

// C6: a handler that checks state before its transaction calls Replay first, so a retried stop
// gets the stored 202, and the same key with another body is idempotency_conflict.
func TestReplayBeforeStateChecks(t *testing.T) {
	h := gadgetHarness(t)
	stop := gadgets + gadgetA + "/stop"
	first := h.do(t, "POST", stop, `{}`, withKey(memberHeaders(), "stop-1"))
	if first.status != 202 {
		t.Fatalf("first: %d %v", first.status, first.body)
	}
	again := h.do(t, "POST", stop, `{}`, withKey(memberHeaders(), "stop-1"))
	if again.status != 202 || again.header.Get("Idempotent-Replayed") != "true" || again.body["status"] != "stopping" {
		t.Fatalf("replay: %d %v %v", again.status, again.body, again.header)
	}
	if r := h.do(t, "POST", stop, `{"force":true}`, withKey(memberHeaders(), "stop-1")); r.status != 409 || code(r) != "idempotency_conflict" {
		t.Fatalf("another body: %d %v", r.status, r.body)
	}
	if r := h.do(t, "POST", stop, `{}`, memberHeaders()); r.status != 409 || code(r) != "gadget_not_running" {
		t.Fatalf("no key: %d %v", r.status, r.body)
	}
	// Replay on an endpoint without the key, or without a key, lets the handler go on.
	if r := h.do(t, "POST", gadgets+gadgetB+"/stop", `{}`, withKey(memberHeaders(), "stop-2")); r.status != 202 || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("a new key: %d %v", r.status, r.body)
	}
}

// B2: an asynchronous replay refreshes the operation and keeps what the first answer carried
// beside it (a create's new volume).
func TestAsyncReplayKeepsFieldsBesideTheOperation(t *testing.T) {
	const op = "0199a3d0-2b3c-7d4e-9f50-6b7c8d9e0f1a"
	extra := []manifest.Endpoint{
		{Method: "POST", Path: "/v1/organizations/:orgId/volumes", Auth: "caller", Action: "widgets.volumes.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CreateVolume"}},
		{Method: "POST", Path: "/v1/organizations/:orgId/plain", Auth: "caller", Action: "widgets.plain.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CreatePlain"}},
	}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.ReplayOperation = func(ctx context.Context, q kitdb.Querier, organizationID, operationID string) (any, error) {
			return map[string]any{"id": operationID, "status": "succeeded"}, nil
		}
		for _, path := range []string{"/v1/organizations/:orgId/volumes", "/v1/organizations/:orgId/plain"} {
			s.Handle("POST", path, func(c *service.Call) error {
				if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
					return err
				}
				return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
					body := map[string]any{"id": op, "status": "running"}
					if c.Endpoint.Path == "/v1/organizations/:orgId/volumes" {
						body["volume"] = map[string]any{"id": "v1", "name": "data"}
					}
					return &service.Response{Status: 202, OperationID: op, Body: body}, nil
				})
			})
		}
	})
	path := "/v1/organizations/" + orgID + "/volumes"
	if r := h.do(t, "POST", path, `{}`, withKey(memberHeaders(), "vol-1")); r.status != 202 || r.body["volume"] == nil {
		t.Fatalf("first: %d %v", r.status, r.body)
	}
	r := h.do(t, "POST", path, `{}`, withKey(memberHeaders(), "vol-1"))
	volume, _ := r.body["volume"].(map[string]any)
	if r.status != 202 || r.body["status"] != "succeeded" || volume["id"] != "v1" || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v", r.status, r.body)
	}
	// An operation alone replays as it is now, exactly.
	plain := "/v1/organizations/" + orgID + "/plain"
	h.do(t, "POST", plain, `{}`, withKey(memberHeaders(), "plain-1"))
	r = h.do(t, "POST", plain, `{}`, withKey(memberHeaders(), "plain-1"))
	if r.status != 202 || len(r.body) != 2 || r.body["status"] != "succeeded" {
		t.Fatalf("plain replay: %d %v", r.status, r.body)
	}
}

// C3: a resource of another product is typed by the platform's registry, not "Unknown".
func TestOtherProductsResourcesAreTyped(t *testing.T) {
	const (
		eni = "ezgh:us-west-1:" + orgSlug + ":prj_g7h8i9j0k1l2:eni_p8of8f2lecc6"
		vol = "ezgh:us-west-1:" + orgSlug + ":prj_g7h8i9j0k1l2:vol_m9u62854epts"
		odd = "ezgh:us-west-1:" + orgSlug + ":prj_g7h8i9j0k1l2:zzz_m9u62854epts"
	)
	extra := []manifest.Endpoint{{Method: "POST", Path: "/v1/organizations/:orgId/links", Auth: "caller", Action: "widgets.links.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CreateLink"}}}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/links", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			c.AddResource(eni)
			c.AddResource(vol)
			c.AddResource(odd)
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: 201, Body: map[string]any{"id": "x"}}, nil
			})
		})
	})
	if r := h.do(t, "POST", "/v1/organizations/"+orgID+"/links", `{}`, memberHeaders()); r.status != 201 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	evs := h.events(t)
	got := map[string]string{}
	for _, r := range evs[len(evs)-1].Resources {
		got[r["resourceName"]] = r["type"]
	}
	if got[eni] != "NetworkInterface" || got[vol] != "Volume" || got[odd] != "Unknown" || got["ezgh::"+orgSlug] != "Organization" {
		t.Fatalf("resources: %v", got)
	}
}

// A6 and images' refused CreateImage: a create checked on its planned name names its project, and
// the planned resource only once it's created.
func TestCheckCreateNamesTheProjectNotThePlannedResource(t *testing.T) {
	const (
		project = "ezgh::" + orgSlug + ":prj_g7h8i9j0k1l2"
		planned = "ezgh:us-west-1:" + orgSlug + ":prj_g7h8i9j0k1l2:wgt_4k2m9x0a7qpe"
	)
	extra := []manifest.Endpoint{{Method: "POST", Path: "/v1/organizations/:orgId/planned", Auth: "caller", Action: "widgets.widgets.create", IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CreatePlanned"}}}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/planned", func(c *service.Call) error {
			perm, err := c.CheckCreate([]string{c.Endpoint.Action}, planned)
			if err != nil {
				return err
			}
			if !perm.Allows(c.Endpoint.Action) {
				return httpapi.AccessDenied(c.Endpoint.Action, false)
			}
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				c.AddResource(planned)
				return &service.Response{Status: 201, Body: map[string]any{"id": "x"}}, nil
			})
		})
	})
	path := "/v1/organizations/" + orgID + "/planned"
	if r := h.do(t, "POST", path, `{}`, map[string]string{"Cookie": "better-auth.session_token=denied", "Origin": goodOrigin}); r.status != 403 {
		t.Fatalf("denied: %d %v", r.status, r.body)
	}
	if r := h.do(t, "POST", path, `{}`, memberHeaders()); r.status != 201 {
		t.Fatalf("allowed: %d %v", r.status, r.body)
	}
	evs := h.events(t)
	if len(evs) != 2 {
		t.Fatalf("%d events", len(evs))
	}
	names := func(ev recorded) []string {
		var out []string
		for _, r := range ev.Resources {
			out = append(out, r["resourceName"]+"="+r["type"])
		}
		return out
	}
	refused, created := names(evs[0]), names(evs[1])
	want := []string{"ezgh::" + orgSlug + "=Organization", project + "=Project"}
	if len(refused) != 2 || refused[0] != want[0] || refused[1] != want[1] {
		t.Fatalf("refused: %v", refused)
	}
	if len(created) != 3 || created[2] != planned+"=Widget" {
		t.Fatalf("created: %v", created)
	}
	// The kit checked the planned name.
	h.org.mu.Lock()
	defer h.org.mu.Unlock()
	if last := h.org.queries[len(h.org.queries)-1]; last.Get("resource") != planned {
		t.Fatalf("checked %v", last)
	}
}

// A8: every response carries nosniff and HSTS, the router's own 404 included.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := newHarness(t)
	for _, r := range []result{
		h.do(t, "GET", things, "", memberHeaders()),
		h.do(t, "GET", "/nope", "", nil),
		h.do(t, "GET", things, "", nil),
	} {
		if r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("Strict-Transport-Security") != "max-age=31536000" {
			t.Errorf("%d: %v", r.status, r.header)
		}
	}
}

// The review's cases: a replay answers only a caller allowed the endpoint's own action, waits for
// a first request still in its prepare step, and stays out of Transact's transaction.
func TestReplayGuards(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	entered := make(chan struct{}, 1)
	h := gadgetHarnessPrepared(t, func() error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	// Registered after the harness, so it runs before the server's Close waits on the first request.
	t.Cleanup(unblock)

	// AuthorizeMissing with a read action never replays a delete.
	if r := h.do(t, "DELETE", "/v1/organizations/"+orgID+"/gizmos/"+gadgetA, "", withKey(memberHeaders(), "gz-1")); r.status != 204 {
		t.Fatalf("gizmo delete: %d %v", r.status, r.body)
	}
	if r := h.do(t, "DELETE", "/v1/organizations/"+orgID+"/gizmos/"+gadgetA, "", withKey(memberHeaders(), "gz-1")); r.status != 404 || r.header.Get("Idempotent-Replayed") != "" {
		t.Fatalf("gizmo replay on a read action: %d %v", r.status, r.body)
	}

	// A denied caller who calls Replay anyway gets nothing stored.
	peek := gadgets + gadgetA + "/peek"
	if r := h.do(t, "POST", peek, `{}`, withKey(memberHeaders(), "pk-1")); r.status != 200 {
		t.Fatalf("peek: %d %v", r.status, r.body)
	}
	denied := map[string]string{"Cookie": "better-auth.session_token=denied", "Origin": goodOrigin, "Idempotency-Key": "pk-1"}
	if r := h.do(t, "POST", peek, `{}`, denied); r.status != 409 || code(r) != "not_replayed" {
		t.Fatalf("a denied caller got the stored answer: %d %v", r.status, r.body)
	}

	// Inside Transact, AuthorizeMissing answers 404 as it always did.
	if r := h.do(t, "DELETE", gadgets+gadgetA+"/late", "", withKey(memberHeaders(), "late-1")); r.status != 404 || code(r) != "not_found" {
		t.Fatalf("in the transaction: %d %v", r.status, r.body)
	}

	// A first request in its prepare step holds a claim: a retry is in progress, not refused.
	done := make(chan result, 1)
	go func() {
		done <- h.do(t, "POST", things, `{"name":"x"}`, withKey(memberHeaders(), "busy-1"))
	}()
	<-entered
	if r := h.do(t, "POST", gadgets+gadgetB+"/stop", `{}`, withKey(memberHeaders(), "busy-1")); r.status != 409 || code(r) != "idempotency_in_progress" {
		t.Fatalf("during prepare: %d %v", r.status, r.body)
	}
	unblock()
	if r := <-done; r.status != 201 {
		t.Fatalf("first: %d %v", r.status, r.body)
	}
}

// A replayed DELETE names what the first request deleted, from the stored record: the gadget is
// gone, so nothing else could name it (contracts/trails.md, "The event").
func TestDeleteReplayNamesTheDeletedResource(t *testing.T) {
	h := gadgetHarness(t)
	if r := h.do(t, "DELETE", gadgets+gadgetA, "", withKey(memberHeaders(), "del-named")); r.status != 204 {
		t.Fatalf("first: %d %v", r.status, r.body)
	}
	if r := h.do(t, "DELETE", gadgets+gadgetA, "", withKey(memberHeaders(), "del-named")); r.status != 204 || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %v %v", r.status, r.body, r.header)
	}
	var names [][]string
	for _, ev := range h.events(t) {
		if ev.EventName != "DeleteGadget" || ev.ErrorCode != nil {
			continue
		}
		var n []string
		for _, r := range ev.Resources {
			n = append(n, r["resourceName"]+" "+r["type"])
		}
		names = append(names, n)
	}
	want := "ezgh::" + orgSlug + " Organization," + gadgetName(gadgetA) + " Unknown"
	if len(names) != 2 || strings.Join(names[0], ",") != want || strings.Join(names[1], ",") != want {
		t.Fatalf("resources %q, want both %q", names, want)
	}
}

// Inside TransactPrepared's prepare the key was looked up and claimed already: a target found
// missing there is 404, never 409 for the request's own claim, and an explicit Replay does nothing.
func TestAuthorizeMissingInsidePrepare(t *testing.T) {
	h := gadgetHarness(t)
	if r := h.do(t, "DELETE", gadgets+gadgetA+"/remote", "", withKey(memberHeaders(), "remote-1")); r.status != 404 || code(r) != "not_found" {
		t.Fatalf("keyed: %d %v", r.status, r.body)
	}
	if r := h.do(t, "DELETE", gadgets+gadgetA+"/remote", "", memberHeaders()); r.status != 404 {
		t.Fatalf("no key: %d %v", r.status, r.body)
	}
}
