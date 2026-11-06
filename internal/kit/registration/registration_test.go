package registration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/registration"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

func widgets(t *testing.T) *manifest.Manifest {
	m, err := manifest.Parse(vectors.File(t, "examples/widgets.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// 5xx is retried, 4xx fails at once, and the rk_ credential is sent.
func TestRegisterRetriesOutagesOnly(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != "PUT" || r.URL.Path != "/internal/v1/registrations/widgets" || r.Header.Get("Authorization") != "Bearer rk_3k9d0a1m2x7q4b8c.x" || len(body) == 0 {
			w.WriteHeader(400)
			return
		}
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"namespace":"widgets","eventSource":"widgets.ezghcloud.com","revision":1,"digest":"sha256:00","dryRun":false,"changes":[{"kind":"namespace","id":"widgets","change":"added"}],"registeredAt":"x","updatedAt":"x"}`))
	}))
	defer server.Close()
	c := &registration.Client{OrgAPIURL: server.URL, Credential: "rk_3k9d0a1m2x7q4b8c.x", Logger: testdb.Logger(), Deadline: 20 * time.Second}
	r, err := c.Register(context.Background(), widgets(t), false)
	if err != nil || r.Revision != 1 || len(r.Changes) != 1 || calls.Load() != 2 {
		t.Fatalf("%+v %v after %d calls", r, err, calls.Load())
	}
}

func TestRegisterFailsFastOn4xx(t *testing.T) {
	for _, status := range []int{404, 409, 401} {
		var calls atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
		}))
		c := &registration.Client{OrgAPIURL: server.URL, InternalToken: "t", Logger: testdb.Logger()}
		_, err := c.Register(context.Background(), widgets(t), true)
		if err == nil || calls.Load() != 1 || registration.NotFound(err) != (status == 404) {
			t.Errorf("%d: %v after %d calls", status, err, calls.Load())
		}
		server.Close()
	}
}

// The quotas registration follows org-api's: the manifest's quotas and INTERNAL_URL, with the qk_
// credential; 5xx retried, 4xx failing at once. It's the body contracts/examples has.
func TestRegisterQuotas(t *testing.T) {
	var calls atomic.Int64
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != "PUT" || r.URL.Path != "/internal/v1/registrations/widgets" || r.Header.Get("Authorization") != "Bearer qk_3k9d0a1m2x7q4b8c.x" {
			w.WriteHeader(403)
			return
		}
		if n == 1 {
			w.WriteHeader(503)
			return
		}
		got = body
		_, _ = w.Write([]byte(`{"namespace":"widgets","revision":1,"digest":"sha256:00","dryRun":false,"changes":[{"kind":"namespace","id":"widgets","change":"added"}],"registeredAt":"x","updatedAt":"x"}`))
	}))
	defer server.Close()
	c := &registration.QuotasClient{QuotasAPIURL: server.URL, Credential: "qk_3k9d0a1m2x7q4b8c.x", InternalURL: "http://widgets-api.widgets.svc.cluster.local:4700", Logger: testdb.Logger(), Deadline: 20 * time.Second}
	r, err := c.Register(context.Background(), widgets(t), false)
	if err != nil || r.Revision != 1 || calls.Load() != 2 {
		t.Fatalf("%+v %v after %d calls", r, err, calls.Load())
	}
	want, _ := canonical.Decode(vectors.File(t, "examples/widgets.quotas-registration-request.json"))
	sent, _ := canonical.Decode(got)
	a, _ := canonical.Marshal(want)
	b, _ := canonical.Marshal(sent)
	if string(a) != string(b) {
		t.Errorf("sent\n%s\nwant\n%s", b, a)
	}
	c.Credential = "qk_wrong.x"
	if _, err := c.Register(context.Background(), widgets(t), true); err == nil {
		t.Error("a 403 didn't fail")
	}
	c.InternalURL = ""
	if _, err := c.Register(context.Background(), widgets(t), false); err == nil {
		t.Error("registered allocation quotas without INTERNAL_URL")
	}
}

// An ingest action is registered with "ingest": true, and the namespace's org-api operations as
// its manifest declares them, exactly as contracts/examples/obs.registration-request.json; widgets,
// with neither, never sends the fields.
func TestRegisterSendsIngestActions(t *testing.T) {
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"namespace":"obs","eventSource":"obs.ezghcloud.com","revision":1,"digest":"sha256:00","dryRun":true,"changes":[],"registeredAt":"x","updatedAt":"x"}`))
	}))
	defer server.Close()
	m, err := manifest.Parse(vectors.File(t, "examples/obs.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &registration.Client{OrgAPIURL: server.URL, Credential: "rk_3k9d0a1m2x7q4b8c.x", Logger: testdb.Logger()}
	if _, err := c.Register(context.Background(), m, true); err != nil {
		t.Fatal(err)
	}
	want, _ := canonical.Decode(vectors.File(t, "examples/obs.registration-request.json"))
	sent, _ := canonical.Decode(got)
	a, _ := canonical.Marshal(want)
	b, _ := canonical.Marshal(sent)
	if string(a) != string(b) {
		t.Errorf("sent\n%s\nwant\n%s", b, a)
	}
	body, _ := json.Marshal(widgets(t).Registration())
	if strings.Contains(string(body), `"ingest"`) || strings.Contains(string(body), `"orgApi"`) {
		t.Errorf("widgets sent an ingest or orgApi field: %s", body)
	}
	if !strings.Contains(string(got), `"orgApi":{"operations":["ingestKeys.resolve","ingestKeys.events","ingestCredentials.resolve"]}`) {
		t.Errorf("obs didn't send its org-api operations: %s", got)
	}
}

// A manifest's scopes go in the registration body as the manifest has them, exactly as
// contracts/examples/mcpcentral.registration-request.json, and a scope change comes back as kind
// "scope".
func TestRegisterSendsScopes(t *testing.T) {
	var got []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"namespace":"mcpcentral","eventSource":"mcpcentral.ezghcloud.com","revision":2,"digest":"sha256:00","dryRun":false,"changes":[{"kind":"scope","id":"mcpcentral:portal","change":"added"}],"registeredAt":"x","updatedAt":"x"}`))
	}))
	defer server.Close()
	m, err := manifest.Parse(vectors.File(t, "examples/mcpcentral.service-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := &registration.Client{OrgAPIURL: server.URL, Credential: "rk_3k9d0a1m2x7q4b8c.x", Logger: testdb.Logger()}
	result, err := c.Register(context.Background(), m, false)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := canonical.Decode(vectors.File(t, "examples/mcpcentral.registration-request.json"))
	sent, _ := canonical.Decode(got)
	a, _ := canonical.Marshal(want)
	b, _ := canonical.Marshal(sent)
	if string(a) != string(b) || !strings.Contains(string(got), `"scopes":[{"name":"mcpcentral:connect"`) {
		t.Errorf("sent\n%s\nwant\n%s", b, a)
	}
	if len(result.Changes) != 1 || result.Changes[0].Kind != "scope" {
		t.Errorf("%+v", result)
	}
}
