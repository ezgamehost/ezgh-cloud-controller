package service_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/cursor"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// A request whose context ends after Transact's fn, just before the commit (database/sql has
// rolled back, and Commit says only sql.ErrTxDone), is answered as one whose context ended at a
// statement: a cancellation as today's cancellations are, a deadline as 503 unavailable. Nothing
// is committed. The call is an internal one without an Idempotency-Key, so the commit follows fn
// directly (no Trails or idempotency statement in between).
func TestTransactContextEndedBeforeCommit(t *testing.T) {
	for _, end := range []struct {
		name    string
		timeout bool
	}{{"cancelled", false}, {"deadline", true}} {
		t.Run(end.name, func(t *testing.T) {
			got := map[string]result{}
			for _, at := range []string{"statement", "commit"} {
				h := &harness{db: testdb.New(t), org: &fakeOrgAPI{}}
				var logs bytes.Buffer
				var mu sync.Mutex
				var cancel context.CancelFunc
				orgServer := httptest.NewServer(h.org)
				t.Cleanup(orgServer.Close)
				s := service.New(service.Options{
					Manifest: testManifest(), DB: h.db.DB, OrgAPI: &authz.Client{BaseURL: orgServer.URL, InternalToken: "t"},
					Cursors: cursor.NewSigner(strings.Repeat("k", 32)), Origins: httpapi.Origins{List: []string{goodOrigin}},
					Logger: slog.New(slog.NewJSONHandler(&logs, nil)), InternalToken: "internal-secret-token", OpenAPIOverlay: stubOverlay(testManifest()),
				})
				s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
				s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
				s.Handle("POST", "/v1/organizations/:orgId/things", func(c *service.Call) error {
					if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
						return err
					}
					return c.Transact(func(tx *kitdb.Tx) (*service.Response, error) {
						if _, err := tx.ExecContext(c.Ctx, `INSERT INTO organization_closures (organization_id, complete_through) VALUES ($1, now())`, orgID); err != nil {
							return nil, err
						}
						if !end.timeout {
							mu.Lock()
							cancel()
							mu.Unlock()
						}
						<-c.Ctx.Done()
						// Wait, bounded, for database/sql to roll the transaction back.
						for deadline := time.Now().Add(10 * time.Second); ; {
							if _, err := tx.ExecContext(context.Background(), `SELECT 1`); errors.Is(err, sql.ErrTxDone) {
								break
							}
							if time.Now().After(deadline) {
								t.Error("the transaction wasn't rolled back after the request's context ended")
								break
							}
							time.Sleep(time.Millisecond)
						}
						if at == "statement" {
							_, err := tx.ExecContext(c.Ctx, `SELECT 1`)
							return nil, err
						}
						return &service.Response{Status: 201, Body: map[string]any{"ok": true}}, nil
					})
				})
				handler, err := s.Handler()
				if err != nil {
					t.Fatal(err)
				}
				h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx, stop := context.WithCancel(r.Context())
					if end.timeout {
						ctx, stop = context.WithTimeout(r.Context(), 500*time.Millisecond)
					}
					defer stop()
					mu.Lock()
					cancel = stop
					mu.Unlock()
					handler.ServeHTTP(w, r.WithContext(ctx))
				}))
				t.Cleanup(h.server.Close)
				// An internal call: Transact records no Trails event in the transaction.
				r := h.do(t, "POST", things, `{}`, map[string]string{"Cookie": "better-auth.session_token=member", "Origin": goodOrigin, "X-Internal-Token": "internal-secret-token"})
				got[at] = r
				var n int64
				if err := h.db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM organization_closures`).Scan(&n); err != nil || n != 0 {
					t.Errorf("%s: %d organization_closures (%v), want none committed", at, n, err)
				}
				want := "context canceled"
				if end.timeout {
					want = "context deadline exceeded"
				}
				if r.status >= 500 && !strings.Contains(logs.String(), want) {
					t.Errorf("%s: the logged failure doesn't name %q: %s", at, want, logs.String())
				}
			}
			if got["commit"].status != got["statement"].status || code(got["commit"]) != code(got["statement"]) {
				t.Errorf("ended before the commit: %d %s; at a statement: %d %s", got["commit"].status, code(got["commit"]),
					got["statement"].status, code(got["statement"]))
			}
			if end.timeout && (got["commit"].status != 503 || code(got["commit"]) != "unavailable") {
				t.Errorf("deadline before the commit: %d %s, want 503 unavailable", got["commit"].status, code(got["commit"]))
			}
		})
	}
}
