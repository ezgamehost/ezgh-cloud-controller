package service_test

import (
	"bytes"
	"context"
	"crypto/sha256"
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

func streamService(t *testing.T, logs *bytes.Buffer, handler service.Handler) (*harness, http.Handler) {
	t.Helper()
	h := &harness{db: testdb.New(t), org: &fakeOrgAPI{}}
	upstream := httptest.NewServer(h.org)
	t.Cleanup(upstream.Close)
	m := testManifest()
	s := service.New(service.Options{Manifest: m, DB: h.db.DB, OrgAPI: &authz.Client{BaseURL: upstream.URL}, Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: slog.New(slog.NewJSONHandler(logs, nil)), OpenAPIOverlay: stubOverlay(m)})
	s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
	s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
	s.HandleStream("POST", "/v1/organizations/:orgId/things", handler)
	out, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h, out
}
func streamRequest(body string) *http.Request {
	r := httptest.NewRequest("POST", things+"?filename=private-query", strings.NewReader(body))
	r.Header.Set("Cookie", "better-auth.session_token=member")
	r.Header.Set("Origin", goodOrigin)
	r.Header.Set("Idempotency-Key", "upload-key")
	return r
}
func TestStreamMetadataIdempotency(t *testing.T) {
	var logs bytes.Buffer
	changes := 0
	h, handler := streamService(t, &logs, func(c *service.Call) error {
		if len(c.Body()) != 0 {
			t.Error("buffered document")
		}
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		digest := sha256.New()
		size, err := io.Copy(digest, c.R.Body)
		if err != nil {
			return err
		}
		metadata := map[string]any{"sha256": fmt.Sprintf("%x", digest.Sum(nil)), "size": size, "name": "private-filename", "mime": "application/pdf"}
		if err := c.SetIdempotencyInput(metadata); err != nil {
			return err
		}
		metadata["sha256"] = "mutated-after-setting"
		c.SetRequestParameters(map[string]any{"size": size})
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			changes++
			return &service.Response{Status: 201, Body: map[string]any{"id": "safe-upload-id", "name": "private-filename", "file": map[string]any{"name": "private-nested-filename"}}}, nil
		})
	})
	for i, body := range []string{"private-document", "private-document", "altered-document"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, streamRequest(body))
		want := 201
		if i == 2 {
			want = 409
		}
		if rec.Code != want {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
		if i == 1 && rec.Header().Get("Idempotent-Replayed") != "true" {
			t.Error("missing replay header")
		}
	}
	if changes != 1 {
		t.Fatalf("mutations %d", changes)
	}
	var persisted string
	if err := h.db.Pool.QueryRow(context.Background(), `SELECT string_agg(event::text,'') FROM audit_outbox`).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-document", "private-filename", "private-query", "altered-document", "private-nested-filename"} {
		if strings.Contains(persisted+logs.String(), secret) {
			t.Errorf("leaked %s", secret)
		}
	}
}
func TestStreamRequiresExplicitIdempotencyInput(t *testing.T) {
	var logs bytes.Buffer
	_, handler := streamService(t, &logs, func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			t.Error("mutation without fingerprint")
			return &service.Response{Status: 201}, nil
		})
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, streamRequest("document"))
	if rec.Code != 500 {
		t.Fatalf("status %d", rec.Code)
	}
}

// A stream route's missing target, found before the body is read, is 404: AuthorizeMissing can't
// replay a keyed retry without the fingerprint the body gives, so it doesn't try.
func TestStreamAuthorizeMissingBeforeBody(t *testing.T) {
	var logs bytes.Buffer
	_, handler := streamService(t, &logs, func(c *service.Call) error {
		return c.AuthorizeMissing(c.Endpoint.Action, "Thing")
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, streamRequest("document"))
	if rec.Code != 404 || rec.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
}

func TestStreamAuditBeforeBytesAndAbort(t *testing.T) {
	for _, mode := range []string{"success", "error", "panic", "audit-failure"} {
		t.Run(mode, func(t *testing.T) {
			var logs bytes.Buffer
			var h *harness
			var handler http.Handler
			h, handler = privateService(t, &logs, func(c *service.Call) error {
				if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
					return err
				}
				return c.Stream(200, http.Header{"Content-Type": []string{"application/octet-stream"}}, func(w io.Writer) error {
					if len(h.events(t)) != 1 {
						t.Error("stream before audit")
					}
					_, _ = io.WriteString(w, "private-download")
					if mode == "error" {
						return errors.New("private-stream-error")
					}
					if mode == "panic" {
						panic("private-stream-panic")
					}
					return nil
				})
			})
			if mode == "audit-failure" {
				if _, err := h.db.Pool.Exec(context.Background(), "DROP TABLE audit_outbox"); err != nil {
					t.Fatal(err)
				}
			}
			rec := httptest.NewRecorder()
			aborted := false
			func() {
				defer func() {
					if p := recover(); p != nil {
						if p != http.ErrAbortHandler {
							t.Fatalf("panic %v", p)
						}
						aborted = true
					}
				}()
				handler.ServeHTTP(rec, streamRequest(""))
			}()
			if aborted != (mode == "error" || mode == "panic") {
				t.Errorf("aborted %v", aborted)
			}
			if mode == "audit-failure" {
				if rec.Code != 503 || strings.Contains(rec.Body.String(), "private-download") {
					t.Fatalf("audit failure response %d %s", rec.Code, rec.Body)
				}
			} else if rec.Body.String() != "private-download" {
				t.Errorf("stream body %s", rec.Body)
			}
			if strings.Contains(logs.String(), "private-") {
				t.Fatalf("private content in logs %s", logs.String())
			}
		})
	}
}

func TestStreamTransportAbortsWithoutErrorBody(t *testing.T) {
	var logs bytes.Buffer
	_, handler := privateService(t, &logs, func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		return c.Stream(200, nil, func(w io.Writer) error {
			_, _ = io.WriteString(w, "private-download")
			if err := http.NewResponseController(c.W).Flush(); err != nil {
				return err
			}
			return errors.New("private-transport-error")
		})
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	req := streamRequest("")
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(server.URL, "http://")
	req.RequestURI = ""
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Error("failed stream completed normally")
	}
	if string(b) != "private-download" {
		t.Errorf("unexpected appended response %q", b)
	}
	if strings.Contains(logs.String(), "private-transport-error") {
		t.Error("logged writer error")
	}
}

func TestStreamPreservesPrivateAuthorizationChecks(t *testing.T) {
	var logs bytes.Buffer
	h, handler := streamService(t, &logs, func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		t.Error("unauthorized handler proceeded")
		return nil
	})
	for _, mode := range []string{"anonymous", "origin", "denied", "malformed"} {
		body := &watchedBody{Reader: strings.NewReader("private-document")}
		req := streamRequest("")
		req.Body = body
		want := 403
		switch mode {
		case "anonymous":
			req.Header.Del("Cookie")
			want = 401
		case "origin":
			req.Header.Del("Origin")
		case "denied":
			req.Header.Set("Cookie", "better-auth.session_token=denied")
		case "malformed":
			req.URL.Path = "/v1/organizations/private-id/things"
			want = 404
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != want || body.reads != 0 {
			t.Errorf("%s: status %d reads %d", mode, rec.Code, body.reads)
		}
	}
	events := h.events(t)
	if len(events) != 1 || len(events[0].Request) != 0 {
		t.Errorf("audit events %+v", events)
	}
}

func TestPrivateResponseIdentifiersExcludeDisplayNames(t *testing.T) {
	var logs bytes.Buffer
	h, handler := privateService(t, &logs, func(c *service.Call) error {
		if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
			return err
		}
		return c.Transact(func(*kitdb.Tx) (*service.Response, error) {
			return &service.Response{Status: 201, Body: map[string]any{"file": map[string]any{"id": "safe-id", "name": "private-filename", "content": "private-result"}}}, nil
		})
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, streamRequest(""))
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), "private-filename") {
		t.Fatalf("customer response %d %s", rec.Code, rec.Body)
	}
	ev := h.events(t)
	if len(ev) != 1 {
		t.Fatal(ev)
	}
	nested, ok := ev[0].Response["file"].(map[string]any)
	if !ok || nested["id"] != "safe-id" || len(nested) != 1 {
		t.Fatalf("private response identifiers %+v", ev[0].Response)
	}
}
