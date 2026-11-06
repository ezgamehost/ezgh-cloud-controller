package service_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/cursor"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

const pollPath = "/v1/organizations/:orgId/things/poll"

// A live-host poll with record: "refusals" (contracts/trails.md, "The run-operation volume
// exception"; contracts/registration.md, "Live endpoints"; Workflows' PollTasks): a successful
// poll leaves no Trails event, a refusal before org-api vouches for the caller leaves none either,
// and a refused poll is recorded with its error code, as a write (readOnly false), so audit-api
// keeps it.
func TestLivePollRecordsOnlyRefusals(t *testing.T) {
	db := testdb.New(t)
	org := &fakeOrgAPI{}
	orgServer := httptest.NewServer(org)
	t.Cleanup(orgServer.Close)
	m := testManifest()
	m.Live = &manifest.LiveHost{Hostname: "widgets-live.ezghcloud.com", Port: 4750}
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{
		Method: "POST", Path: pollPath, Host: manifest.HostLive, Component: manifest.ComponentLive, Auth: "caller",
		Action: "widgets.things.create", IdempotencyException: "computation", TimeoutSeconds: 70,
		Trail: &manifest.Trail{EventName: "PollThings", Record: manifest.RecordRefusals},
	})
	s := service.New(service.Options{
		Manifest: m, DB: db.DB, OrgAPI: &authz.Client{BaseURL: orgServer.URL, InternalToken: "t", PublicBaseURL: "https://widgets-live.ezghcloud.com"},
		Cursors: cursor.NewSigner(strings.Repeat("k", 32)), Origins: httpapi.Origins{List: []string{goodOrigin}},
		Logger: testdb.Logger(), InternalToken: "internal-secret-token", OpenAPIOverlay: stubOverlay(m),
	})
	s.Handle("POST", pollPath, func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		c.JSON(200, map[string]any{"invocations": []any{}})
		return nil
	})
	h, err := s.Handler(manifest.ComponentLive)
	if err != nil {
		t.Fatal(err)
	}
	poll := func(cookie string) int {
		req := httptest.NewRequest("POST", "/v1/organizations/"+orgID+"/things/poll", strings.NewReader(`{}`))
		req.Host = "widgets-live.ezghcloud.com"
		req.Header.Set("Cookie", "better-auth.session_token="+cookie)
		req.Header.Set("Origin", goodOrigin)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	events := func() []recorded {
		rows, err := db.Pool.Query(context.Background(), `SELECT event::text FROM audit_outbox ORDER BY created_at`)
		if err != nil {
			t.Fatal(err)
		}
		texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		out := make([]recorded, 0, len(texts))
		for _, text := range texts {
			var e recorded
			if err := json.Unmarshal([]byte(text), &e); err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
		return out
	}
	for range 3 {
		if code := poll("member"); code != http.StatusOK {
			t.Fatalf("poll: %d", code)
		}
	}
	if ev := events(); len(ev) != 0 {
		t.Fatalf("a successful poll was recorded: %+v", ev)
	}
	if code := poll("expired"); code != http.StatusUnauthorized {
		t.Fatalf("expired poll: %d", code)
	}
	if ev := events(); len(ev) != 0 {
		t.Fatalf("a refusal before org-api vouched for the caller was recorded: %+v", ev)
	}
	if code := poll("denied"); code != http.StatusForbidden {
		t.Fatalf("denied poll: %d", code)
	}
	ev := events()
	if len(ev) != 1 || ev[0].EventName != "PollThings" || ev[0].ErrorCode == nil || *ev[0].ErrorCode != "access_denied" || ev[0].ReadOnly {
		t.Fatalf("refused poll: %+v", ev)
	}
}
