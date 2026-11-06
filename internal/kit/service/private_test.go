package service_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type watchedBody struct {
	io.Reader
	reads int
}

func (b *watchedBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (*watchedBody) Close() error                 { return nil }

type eventWriter struct {
	*httptest.ResponseRecorder
	before func()
}

func (w eventWriter) WriteHeader(n int) { w.before(); w.ResponseRecorder.WriteHeader(n) }
func privateService(t *testing.T, logs *bytes.Buffer, handler service.Handler) (*harness, http.Handler) {
	t.Helper()
	h := &harness{db: testdb.New(t), org: &fakeOrgAPI{}}
	upstream := httptest.NewServer(h.org)
	t.Cleanup(upstream.Close)
	m := testManifest()
	m.Endpoints[3].IdempotencyKey = false
	m.Endpoints[3].IdempotencyException = "computation"
	s := service.New(service.Options{Manifest: m, DB: h.db.DB, InternalToken: "internal-secret-token", OrgAPI: &authz.Client{BaseURL: upstream.URL}, Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: slog.New(slog.NewJSONHandler(logs, nil)), OpenAPIOverlay: stubOverlay(m)})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
	s.HandlePrivate("POST", "/v1/organizations/:orgId/things", handler)
	out, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h, out
}
func TestPrivateStreamingAndSafeAudit(t *testing.T) {
	for _, mode := range []string{"success", "failure", "panic", "denied", "anonymous", "origin", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			payload := strings.Repeat("private-document", 100000)
			body := &watchedBody{Reader: strings.NewReader(payload)}
			h, handler := privateService(t, &logs, func(c *service.Call) error {
				if body.reads != 0 || len(c.Body()) != 0 {
					t.Error("kit read private body before authorization")
				}
				if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
					return err
				}
				raw, err := io.ReadAll(c.R.Body)
				if err != nil || string(raw) != payload {
					t.Error("body not preserved")
				}
				if mode == "failure" {
					return httpapi.Internal(errors.New("private-error-cause"))
				}
				if mode == "panic" {
					panic("private-panic-value")
				}
				c.SetRequestParameters(map[string]any{"pages": 2, "seed": "private-seed"})
				return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
					return &service.Response{Status: 200, Body: map[string]any{"ok": true}}, nil
				})
			})
			path := things + "?filename=private-query"
			if mode == "malformed" {
				path = "/v1/organizations/private-path/things"
			}
			req := httptest.NewRequest("POST", path, nil)
			req.Body = body
			req.Header.Set("Content-Type", "multipart/form-data; boundary=example")
			req.Header.Set("Cookie", "better-auth.session_token=member")
			req.Header.Set("Origin", goodOrigin)
			if mode == "denied" {
				req.Header.Set("Cookie", "better-auth.session_token=denied")
			}
			if mode == "anonymous" {
				req.Header.Del("Cookie")
			}
			if mode == "origin" {
				req.Header.Del("Origin")
			}
			recorded := mode == "success" || mode == "failure" || mode == "panic" || mode == "denied"
			rec := httptest.NewRecorder()
			handler.ServeHTTP(eventWriter{rec, func() {
				if recorded && len(h.events(t)) != 1 {
					t.Error("response preceded durable audit event")
				}
			}}, req)
			want := map[string]int{"success": 200, "failure": 500, "panic": 500, "denied": 403, "anonymous": 401, "origin": 403, "malformed": 404}[mode]
			if rec.Code != want {
				t.Fatalf("status %d want %d: %s", rec.Code, want, rec.Body)
			}
			if mode == "denied" || !recorded {
				if body.reads != 0 {
					t.Error("read unauthorized body")
				}
			}
			ev := h.events(t)
			if recorded {
				if len(ev) != 1 {
					t.Fatalf("events %v", ev)
				}
				if mode == "success" {
					if ev[0].Request["pages"] != float64(2) || ev[0].Request["seed"] != "HIDDEN_DUE_TO_SECURITY_REASONS" {
						t.Errorf("safe metadata %v", ev[0].Request)
					}
				} else if len(ev[0].Request) != 0 {
					t.Errorf("private parameters %v", ev[0].Request)
				}
			} else if len(ev) != 0 {
				t.Error("unattributable event")
			}
			var all string
			if err := h.db.Pool.QueryRow(context.Background(), "SELECT coalesce(string_agg(event::text, ''), '') FROM audit_outbox").Scan(&all); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"private-document", "private-query", "private-path", "private-error-cause", "private-panic-value", "private-seed"} {
				if strings.Contains(logs.String()+all, secret) {
					t.Errorf("leaked %q", secret)
				}
			}
		})
	}
}
func TestPrivateAuditFailureClosesResponse(t *testing.T) {
	var logs bytes.Buffer
	h, handler := privateService(t, &logs, func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		return httpapi.InvalidRequest("Safe failure")
	})
	if _, err := h.db.Pool.Exec(context.Background(), "DROP TABLE audit_outbox"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", things, nil)
	req.Header.Set("Cookie", "better-auth.session_token=member")
	req.Header.Set("Origin", goodOrigin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("audit unavailable: %d %s", rec.Code, rec.Body)
	}
}

// A success event may be inserted before COMMIT fails; the failure still needs its own event.
func TestPrivateCommitFailureIsRecorded(t *testing.T) {
	var logs bytes.Buffer
	h, handler := privateService(t, &logs, func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			return &service.Response{Status: 200, Body: map[string]any{"ok": true}}, nil
		})
	})
	_, err := h.db.Pool.Exec(context.Background(), `CREATE FUNCTION reject_success() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event->>'errorCode' IS NULL THEN RAISE EXCEPTION 'private-commit-content'; END IF; RETURN NEW; END $$;
 CREATE CONSTRAINT TRIGGER reject_success AFTER INSERT ON audit_outbox DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_success()`)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", things, nil)
	req.Header.Set("Cookie", "better-auth.session_token=member")
	req.Header.Set("Origin", goodOrigin)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(eventWriter{rec, func() {
		if len(h.events(t)) != 1 {
			t.Error("failed commit not audited before response")
		}
	}}, req)
	if rec.Code != 500 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	ev := h.events(t)
	if len(ev) != 1 || ev[0].ErrorCode == nil {
		t.Fatalf("events: %+v", ev)
	}
	if strings.Contains(logs.String(), "private-commit-content") {
		t.Error("commit cause leaked")
	}
}

func TestPrivateRejectsPersistedIdempotency(t *testing.T) {
	m := testManifest()
	s := service.New(service.Options{Manifest: m, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
	s.HandlePrivate("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	if _, err := s.Handler(); err == nil || !strings.Contains(err.Error(), "idempotency") {
		t.Fatalf("private response persistence allowed: %v", err)
	}
}

func TestValidInternalCallsAreNotRecorded(t *testing.T) {
	for _, private := range []bool{false, true} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("private=%t/failure=%t", private, failure), func(t *testing.T) {
				var h *harness
				if private {
					var logs bytes.Buffer
					var handler http.Handler
					h, handler = privateService(t, &logs, func(c *service.Call) error {
						if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
							return err
						}
						if failure {
							return httpapi.InvalidRequest("Safe failure")
						}
						return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
							return &service.Response{Status: 201, Body: map[string]any{"ok": true}}, nil
						})
					})
					h.server = httptest.NewServer(handler)
					t.Cleanup(h.server.Close)
				} else {
					h = newHarness(t)
				}
				count := 0
				for _, token := range []string{"internal-secret-token", "wrong", ""} {
					body := `{"name":"hello"}`
					if failure {
						body = `{"unknown":"x"}`
					}
					headers := map[string]string{"Cookie": "better-auth.session_token=member", "Origin": goodOrigin, "X-Internal-Token": token}
					res := h.do(t, "POST", things, body, headers)
					expected := 201
					if failure {
						expected = 400
					}
					if res.status != expected {
						t.Fatalf("status %d want %d", res.status, expected)
					}
					if token != "internal-secret-token" {
						count++
					}
					if events := h.events(t); len(events) != count {
						t.Errorf("token %q: recorded %d events want %d", token, len(events), count)
					}
				}
				// The internal marker suppresses duplicate audit only; customer authorization still applies.
				res := h.do(t, "POST", things, `{}`, map[string]string{"X-Internal-Token": "internal-secret-token"})
				if res.status != 401 {
					t.Fatalf("internal token bypassed customer authentication: %d", res.status)
				}
			})
		}
	}
}
