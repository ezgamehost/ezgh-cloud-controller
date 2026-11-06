package service_test

import (
	"testing"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

const starts = "/v1/organizations/" + orgID + "/things/start"
const cancels = "/v1/organizations/" + orgID + "/things/cancel"

func memberHeaders() map[string]string {
	return map[string]string{"Cookie": "better-auth.session_token=member", "Origin": goodOrigin}
}

// record: "refusals" (contracts/trails.md, "The run-operation volume exception"): a successful
// start leaves no event, while a denied one, and one that fails after authorization, are recorded
// with their error codes.
func TestRefusalsOnlyTrailSkipsSuccess(t *testing.T) {
	extra := []manifest.Endpoint{{
		Method: "POST", Path: "/v1/organizations/:orgId/things/start", Auth: "caller", Action: "widgets.things.create",
		IdempotencyKey: true, Trail: &manifest.Trail{EventName: "StartThing", Record: manifest.RecordRefusals},
	}}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/things/start", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			if string(c.Body()) == `{"conflict":true}` {
				return httpapi.Conflict("already_started", "The thing is already started")
			}
			return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: 201, Body: map[string]any{"id": "x"}}, nil
			})
		})
	})
	headers := memberHeaders()
	headers["Idempotency-Key"] = "k-refusals-1"
	for range 2 { // the first call and its replay
		if r := h.do(t, "POST", starts, `{}`, headers); r.status != 201 {
			t.Fatalf("%d %v", r.status, r.body)
		}
	}
	if ev := h.events(t); len(ev) != 0 {
		t.Fatalf("a success was recorded: %+v", ev)
	}
	r := h.do(t, "POST", starts, `{}`, map[string]string{"Cookie": "better-auth.session_token=denied", "Origin": goodOrigin})
	if r.status != 403 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	ev := h.events(t)
	if len(ev) != 1 || ev[0].EventName != "StartThing" || ev[0].ErrorCode == nil || *ev[0].ErrorCode != "access_denied" {
		t.Fatalf("refusal: %+v", ev)
	}
	if r := h.do(t, "POST", starts, `{"conflict":true}`, memberHeaders()); r.status != 409 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	ev = h.events(t)
	if len(ev) != 2 || ev[1].ErrorCode == nil || *ev[1].ErrorCode != "already_started" {
		t.Fatalf("failure after authorization: %+v", ev)
	}
}

// HandOffEvent builds the success event without recording it; Recorded stops the kit from
// recording the call itself.
func TestHandOffEventIsRecordedOnlyOnce(t *testing.T) {
	extra := []manifest.Endpoint{{
		Method: "POST", Path: "/v1/organizations/:orgId/things/cancel", Auth: "caller", Action: "widgets.things.create",
		IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CancelThing"},
	}}
	var handed string
	var beforeAuthorize error
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/things/cancel", func(c *service.Call) error {
			_, beforeAuthorize = c.HandOffEvent(200, map[string]any{"id": "x"})
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			ev, err := c.HandOffEvent(200, map[string]any{"id": "x"})
			if err != nil {
				return err
			}
			handed = ev.EventName
			c.Recorded()
			c.JSON(200, map[string]any{"id": "x"})
			return nil
		})
	})
	if r := h.do(t, "POST", cancels, `{}`, memberHeaders()); r.status != 200 || handed != "CancelThing" {
		t.Fatalf("%d %q", r.status, handed)
	}
	if beforeAuthorize == nil {
		t.Error("HandOffEvent built an event before org-api vouched for the caller")
	}
	if ev := h.events(t); len(ev) != 0 {
		t.Fatalf("the kit recorded a handed-off call: %+v", ev)
	}
}

// A handed-off write that also stores its Idempotency-Key (Workflows' CancelRun and RetryRun): the
// handler hands the event to the other side in TransactPrepared's prepare (which a replay never
// runs), the other side commits it, and Recorded stops the kit from inserting it again in the
// transaction that stores the key. A replay is its own call, recorded by the kit as any replay
// (contracts/api-conventions.md, "Idempotency keys": each call, replayed or not, is its own event).
// A failure after the hand-off records nothing more: the other side's event already stands.
func TestHandOffWithIdempotencyKeyRecordsOnce(t *testing.T) {
	extra := []manifest.Endpoint{{
		Method: "POST", Path: "/v1/organizations/:orgId/things/cancel", Auth: "caller", Action: "widgets.things.create",
		IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CancelThing"},
	}}
	prepared := 0
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/things/cancel", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			return c.TransactPrepared(func() error {
				prepared++
				ev, err := c.HandOffEvent(200, map[string]any{"id": "x"})
				if err != nil {
					return err
				}
				// The other side (Workflows' engine) commits the event with its own change.
				if err := trails.InsertAlone(c.Ctx, c.Service.DB, ev); err != nil {
					return err
				}
				c.Recorded()
				return nil
			}, func(tx *kitdb.Tx) (*service.Response, error) {
				if string(c.Body()) == `{"fail":true}` {
					return nil, httpapi.Conflict("store_failed", "The key couldn't be stored")
				}
				return &service.Response{Status: 200, Body: map[string]any{"id": "x"}}, nil
			})
		})
	})
	headers := memberHeaders()
	headers["Idempotency-Key"] = "k-handoff-1"
	if r := h.do(t, "POST", cancels, `{}`, headers); r.status != 200 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	if ev := h.events(t); len(ev) != 1 || ev[0].EventName != "CancelThing" || ev[0].ErrorCode != nil {
		t.Fatalf("the handed-off call was recorded %d times: %+v", len(ev), ev)
	}
	r := h.do(t, "POST", cancels, `{}`, headers)
	if r.status != 200 || r.header.Get("Idempotent-Replayed") != "true" || prepared != 1 {
		t.Fatalf("replay: %d %v %v, prepared %d", r.status, r.body, r.header, prepared)
	}
	if ev := h.events(t); len(ev) != 2 || ev[1].EventName != "CancelThing" || ev[1].ErrorCode != nil {
		t.Fatalf("the replay should be one event of its own: %+v", ev)
	}
	failing := memberHeaders()
	failing["Idempotency-Key"] = "k-handoff-2"
	if r := h.do(t, "POST", cancels, `{"fail":true}`, failing); r.status != 409 || prepared != 2 {
		t.Fatalf("%d %v, prepared %d", r.status, r.body, prepared)
	}
	if ev := h.events(t); len(ev) != 3 || ev[2].ErrorCode != nil {
		t.Fatalf("a failure after the hand-off was recorded too: %+v", ev)
	}
}

// A4's pattern without an Idempotency-Key (TransactPrepared still runs prepare, then a transaction
// with nothing to store), and a handler that fails after the hand-off outside any transaction (the
// ambiguous engine commit answered 503): the other side's event is the only one either way.
func TestHandOffWithoutKeyAndFailureAfterHandOff(t *testing.T) {
	extra := []manifest.Endpoint{{
		Method: "POST", Path: "/v1/organizations/:orgId/things/cancel", Auth: "caller", Action: "widgets.things.create",
		IdempotencyKey: true, Trail: &manifest.Trail{EventName: "CancelThing"},
	}}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/things/cancel", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			handOff := func() error {
				ev, err := c.HandOffEvent(200, map[string]any{"id": "x"})
				if err != nil {
					return err
				}
				if err := trails.InsertAlone(c.Ctx, c.Service.DB, ev); err != nil {
					return err
				}
				c.Recorded()
				return nil
			}
			if string(c.Body()) == `{"unavailable":true}` {
				if err := handOff(); err != nil {
					return err
				}
				return httpapi.Unavailable("The engine's answer was lost", nil)
			}
			return c.TransactPrepared(handOff, func(tx *kitdb.Tx) (*service.Response, error) {
				return &service.Response{Status: 200, Body: map[string]any{"id": "x"}}, nil
			})
		})
	})
	if r := h.do(t, "POST", cancels, `{}`, memberHeaders()); r.status != 200 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	if ev := h.events(t); len(ev) != 1 || ev[0].ErrorCode != nil {
		t.Fatalf("without a key: %+v", ev)
	}
	if r := h.do(t, "POST", cancels, `{"unavailable":true}`, memberHeaders()); r.status != 503 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	if ev := h.events(t); len(ev) != 2 || ev[1].ErrorCode != nil {
		t.Fatalf("a failure after the hand-off was recorded: %+v", ev)
	}
}

// record: "refusals" on a computation POST (contracts/trails.md, "The run-operation volume
// exception": EZGH Inference's CreateEmbeddings): a success leaves no event; a denial, a failure
// after authorization and the OpenAI surface's monthly limit (insufficient_quota on the wire,
// quota_exceeded in Trails) are recorded with readOnly false, so audit-api always keeps them: they
// are the only record of the call.
func TestComputationRefusalsOnlyRecordsDenialsNotSuccesses(t *testing.T) {
	const path = "/v1/organizations/:orgId/projects/:projectId/openai/v1/embeddings"
	extra := []manifest.Endpoint{{
		Method: "POST", Path: path, Auth: "caller", Action: "widgets.models.invoke", Credentials: manifest.CredentialsBearer,
		ErrorModel: manifest.ErrorModelOpenAI, IdempotencyException: "computation",
		Trail: &manifest.Trail{EventName: "CreateEmbeddings", Sensitive: []string{"input", "user"}, Record: manifest.RecordRefusals},
	}}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", path, func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			switch string(c.Body()) {
			case `{"model":"monthly"}`:
				return &httpapi.Error{Status: 429, Code: httpapi.OpenAIInsufficientQuota, Message: "This organization used its monthly tokens"}
			case `{"model":"broken"}`:
				return &httpapi.Error{Status: 502, Code: "inference_failed", Message: "The model backend failed"}
			case `{"model":"busy"}`:
				return httpapi.TooManyRequests("Slow down", 2)
			case `{"input":[]}`:
				return httpapi.Invalid([]httpapi.Issue{{Path: "input", Message: "must not be empty"}})
			}
			c.JSON(200, map[string]any{"object": "list", "data": []any{}})
			return nil
		})
	})
	url := "/v1/organizations/" + orgID + "/projects/" + projectID + "/openai/v1/embeddings"
	bearer := map[string]string{"Authorization": "Bearer " + apiKey}
	for range 3 {
		if r := h.do(t, "POST", url, `{"model":"m","input":"secret text"}`, bearer); r.status != 200 {
			t.Fatalf("%d %v", r.status, r.body)
		}
	}
	if ev := h.events(t); len(ev) != 0 {
		t.Fatalf("a success was recorded: %+v", ev)
	}
	// Refusals before org-api vouches stay unrecorded.
	if r := h.do(t, "POST", url, `{}`, nil); r.status != 401 {
		t.Fatalf("%d %v", r.status, r.body)
	}
	for _, c := range []struct {
		body    string
		headers map[string]string
		status  int
		code    string
	}{
		{`{"model":"m","input":"secret text"}`, map[string]string{"Authorization": "Bearer " + deniedToken}, 403, "access_denied"},
		{`{"model":"monthly"}`, bearer, 429, "quota_exceeded"},
		{`{"model":"broken"}`, bearer, 502, "inference_failed"},
		{`{"model":"busy"}`, bearer, 429, "too_many_requests"},
		{`{"input":[]}`, bearer, 400, "invalid_request"},
	} {
		r := h.do(t, "POST", url, c.body, c.headers)
		if r.status != c.status {
			t.Fatalf("%s: %d %v", c.code, r.status, r.body)
		}
		ev := h.events(t)
		last := ev[len(ev)-1]
		if last.EventName != "CreateEmbeddings" || last.ErrorCode == nil || *last.ErrorCode != c.code || last.ReadOnly || last.Response != nil {
			t.Fatalf("%s: %+v", c.code, last)
		}
		if input, ok := last.Request["input"]; ok && input != "HIDDEN_DUE_TO_SECURITY_REASONS" {
			t.Fatalf("the input was recorded: %v", last.Request)
		}
	}
	if ev := h.events(t); len(ev) != 5 {
		t.Fatalf("events %+v", ev)
	}
	// The client still sees OpenAI's own code.
	if r := h.do(t, "POST", url, `{"model":"monthly"}`, bearer); r.body["error"].(map[string]any)["code"] != "insufficient_quota" {
		t.Fatalf("%v", r.body)
	}
}

// insufficient_quota is recorded as quota_exceeded only on the OpenAI surface: elsewhere it's a
// product's own code like any other, recorded as answered.
func TestInsufficientQuotaIsOnlyMappedOnOpenAIEndpoints(t *testing.T) {
	extra := []manifest.Endpoint{{
		Method: "POST", Path: "/v1/organizations/:orgId/things/start", Auth: "caller", Action: "widgets.things.create",
		IdempotencyKey: true, Trail: &manifest.Trail{EventName: "StartThing"},
	}}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", "/v1/organizations/:orgId/things/start", func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			return &httpapi.Error{Status: 429, Code: httpapi.OpenAIInsufficientQuota, Message: "Out of quota"}
		})
	})
	headers := memberHeaders()
	headers["Idempotency-Key"] = "k-insufficient-1"
	r := h.do(t, "POST", starts, `{}`, headers)
	if r.status != 429 || code(r) != "insufficient_quota" || r.header.Get("x-should-retry") != "" {
		t.Fatalf("%d %v %v", r.status, r.body, r.header)
	}
	ev := h.events(t)
	if len(ev) != 1 || ev[0].ErrorCode == nil || *ev[0].ErrorCode != "insufficient_quota" {
		t.Fatalf("%+v", ev)
	}
}
