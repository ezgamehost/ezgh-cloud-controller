package service_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// An ingest-host endpoint is served by the product's ingest server (manifest.CheckIngestRoutes),
// never by Service: Handler builds without a handler for it, never routes it, and refuses one.
func TestIngestEndpointsAreNotServedHere(t *testing.T) {
	db := testdb.New(t)
	m := testManifest()
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "POST", Path: "/v1/logs", Host: manifest.HostIngest, Auth: manifest.AuthIngestKey, Action: "widgets.logs.ingest"})
	build := func(withIngestHandler bool) (http.Handler, error) {
		s := service.New(service.Options{Manifest: m, DB: db.DB, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
		s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
		if withIngestHandler {
			s.Handle("POST", "/v1/logs", func(*service.Call) error { return nil })
		}
		return s.Handler()
	}
	handler, err := build(false)
	if err != nil {
		t.Fatalf("an ingest endpoint needed a handler: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/logs", strings.NewReader("{}")))
	if rec.Code != 404 {
		t.Errorf("the API host served the ingest endpoint: %d", rec.Code)
	}
	if _, err := build(true); err == nil || !strings.Contains(err.Error(), "POST /v1/logs is an ingest-host endpoint: the ingest server serves it, not this API") {
		t.Fatalf("got %v", err)
	}
}
