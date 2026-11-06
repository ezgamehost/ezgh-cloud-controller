package service_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/cursor"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// Receipt-handle endpoints (contracts/registration.md, "Receipt-handle endpoints"): the live
// component hands the body to the handler, which verifies the product-signed handles in it. The kit
// reads no credential, never asks org-api, never checks Origin, answers CORS for any origin without
// credentials, and records nothing.

type receiptSeen struct {
	body           string
	organizationID string
}

func receiptService(t *testing.T, opts service.Options) (http.Handler, *fakeOrgAPI, *[]receiptSeen) {
	t.Helper()
	org := &fakeOrgAPI{}
	orgServer := httptest.NewServer(org)
	t.Cleanup(orgServer.Close)
	m := testManifest()
	m.Live = &manifest.LiveHost{Hostname: "widgets-live.ezghcloud.com", Port: 4750}
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "POST", Path: "/v1/receipts/ack", Host: manifest.HostLive, Component: manifest.ComponentLive, Auth: manifest.AuthReceiptHandle},
		manifest.Endpoint{Method: "POST", Path: "/v1/receipts/visibility", Host: manifest.HostLive, Component: manifest.ComponentLive, Auth: manifest.AuthReceiptHandle},
	)
	opts.Manifest = m
	opts.OrgAPI = &authz.Client{BaseURL: orgServer.URL, InternalToken: "t"}
	opts.Origins = httpapi.Origins{List: []string{goodOrigin}}
	opts.Logger = testdb.Logger()
	opts.OpenAPIOverlay = stubOverlay(m)
	s := service.New(opts)
	var seen []receiptSeen
	s.Handle("POST", "/v1/receipts/ack", func(c *service.Call) error {
		seen = append(seen, receiptSeen{body: string(c.Body()), organizationID: c.OrganizationID})
		c.JSON(200, map[string]any{"results": []any{map[string]any{"ok": true}}})
		return nil
	})
	s.Handle("POST", "/v1/receipts/visibility", func(c *service.Call) error {
		seen = append(seen, receiptSeen{body: string(c.Body()), organizationID: c.OrganizationID})
		return httpapi.InvalidRequest("A receipt handle is malformed")
	})
	h, err := s.Handler(manifest.ComponentLive)
	if err != nil {
		t.Fatal(err)
	}
	return h, org, &seen
}

func receiptDo(h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "widgets-live.ezghcloud.com"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestReceiptEndpointReadsNoCredential(t *testing.T) {
	h, org, seen := receiptService(t, service.Options{})
	for name, headers := range map[string]map[string]string{
		"nothing":                    {},
		"a cookie from another site": {"Cookie": "better-auth.session_token=member", "Origin": "https://evil.example"},
		"a bearer":                   {"Authorization": "Bearer ezgh_live_not_checked"},
		"a malformed bearer":         {"Authorization": "Bearer "},
		"Basic":                      {"Authorization": "Basic dTpw"},
	} {
		rec := receiptDo(h, "POST", "/v1/receipts/ack", `{"receiptHandles":["k1.abc.def"]}`, headers)
		if rec.Code != 200 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if len(*seen) != 5 {
		t.Fatalf("handler ran %d times", len(*seen))
	}
	for _, s := range *seen {
		if s.body != `{"receiptHandles":["k1.abc.def"]}` || s.organizationID != "" {
			t.Errorf("%+v", s)
		}
	}
	if org.calls != 0 {
		t.Errorf("asked org-api %d times: %v", org.calls, org.forwarded)
	}
	// The handler's own refusal is the platform's error, unchanged.
	rec := receiptDo(h, "POST", "/v1/receipts/visibility", `{"entries":[]}`, nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), `"invalid_request"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestReceiptEndpointBodyCap(t *testing.T) {
	h, _, seen := receiptService(t, service.Options{})
	rec := receiptDo(h, "POST", "/v1/receipts/ack", strings.Repeat("a", httpapi.MaxBodyBytes+1), nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(*seen) != 0 {
		t.Fatalf("handler ran: %v", *seen)
	}
	if rec := receiptDo(h, "POST", "/v1/receipts/ack", strings.Repeat("a", httpapi.MaxBodyBytes), nil); rec.Code != 200 {
		t.Fatalf("at the cap: %d %s", rec.Code, rec.Body)
	}
}

func TestReceiptEndpointCORS(t *testing.T) {
	h, org, seen := receiptService(t, service.Options{})
	for _, origin := range []string{"https://evil.example", goodOrigin} {
		pre := receiptDo(h, "OPTIONS", "/v1/receipts/ack", "", map[string]string{"Origin": origin, "Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type"})
		hd := pre.Header()
		if pre.Code != http.StatusNoContent || hd.Get("Access-Control-Allow-Origin") != "*" || hd.Get("Access-Control-Allow-Credentials") != "" ||
			!strings.Contains(hd.Get("Access-Control-Allow-Methods"), "POST") || hd.Get("Access-Control-Allow-Headers") != "Content-Type" || hd.Get("Access-Control-Max-Age") != "600" {
			t.Fatalf("%s preflight: %d %v", origin, pre.Code, hd)
		}
		rec := receiptDo(h, "POST", "/v1/receipts/ack", `{}`, map[string]string{"Origin": origin, "Content-Type": "application/json"})
		if rec.Code != 200 || rec.Header().Get("Access-Control-Allow-Origin") != "*" || rec.Header().Get("Access-Control-Allow-Credentials") != "" ||
			!strings.Contains(rec.Header().Get("Access-Control-Expose-Headers"), "X-Request-Id") {
			t.Fatalf("%s POST: %d %v", origin, rec.Code, rec.Header())
		}
	}
	// A refusal answers CORS too, so a browser page can read it.
	if rec := receiptDo(h, "POST", "/v1/receipts/visibility", `{}`, map[string]string{"Origin": "https://evil.example"}); rec.Code != 400 || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("refusal: %d %v", rec.Code, rec.Header())
	}
	if len(*seen) != 3 || org.calls != 0 {
		t.Fatalf("handler ran %d times, org-api asked %d times", len(*seen), org.calls)
	}
	// Every other route keeps the platform's credentialed allowlist.
	rec := receiptDo(h, "OPTIONS", "/v1/organizations/"+orgID+"/things", "", map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"})
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("another route answered an unlisted origin: %v", rec.Header())
	}
	rec = receiptDo(h, "OPTIONS", "/v1/organizations/"+orgID+"/things", "", map[string]string{"Origin": goodOrigin, "Access-Control-Request-Method": "POST"})
	if rec.Header().Get("Access-Control-Allow-Origin") != goodOrigin || rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("another route lost the platform CORS: %v", rec.Header())
	}
}

func TestReceiptEndpointNotRecorded(t *testing.T) {
	db := testdb.New(t)
	h, _, seen := receiptService(t, service.Options{DB: db.DB, Cursors: cursor.NewSigner(strings.Repeat("k", 32)), InternalToken: "internal-secret-token"})
	headers := map[string]string{"Cookie": "better-auth.session_token=member", "Origin": goodOrigin, "Content-Type": "application/json"}
	if rec := receiptDo(h, "POST", "/v1/receipts/ack", `{"receiptHandles":["h"]}`, headers); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := receiptDo(h, "POST", "/v1/receipts/visibility", `{"entries":[]}`, headers); rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(*seen) != 2 {
		t.Fatalf("handler ran %d times", len(*seen))
	}
	var n int
	if err := db.Pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_outbox`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d Trails rows for receipt calls", n)
	}
}
