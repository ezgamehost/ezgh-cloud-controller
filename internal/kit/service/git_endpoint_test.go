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

// A Git-host endpoint is served by the product's own Git server (manifest.CheckGitRoutes), never
// by Service: Handler builds without a handler for it, never routes it, and refuses one.
func TestGitEndpointsAreNotServedHere(t *testing.T) {
	db := testdb.New(t)
	m := testManifest()
	m.Git = &manifest.GitHost{Domain: "widgets.ezghcloud.com", Port: 4701}
	const path = "/<repo>.git/git-upload-pack"
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "POST", Path: path, Host: manifest.HostGit, Auth: manifest.AuthGit, Action: "widgets.images.pull"})
	build := func(withGitHandler bool) (http.Handler, error) {
		s := service.New(service.Options{Manifest: m, DB: db.DB, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
		s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
		if withGitHandler {
			s.Handle("POST", path, func(*service.Call) error { return nil })
		}
		return s.Handler()
	}
	handler, err := build(false)
	if err != nil {
		t.Fatalf("a Git endpoint needed a handler: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/x.git/git-upload-pack", nil))
	if rec.Code != 404 {
		t.Errorf("the API host served the Git endpoint: %d", rec.Code)
	}
	if _, err := build(true); err == nil || !strings.Contains(err.Error(), "is a Git-host endpoint: the Git server serves it, not this API") {
		t.Fatalf("got %v", err)
	}
}
