package ca_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/ca"
)

// signer answers as certificates-api: 201 at once, or 202 and an operation the caller follows.
func signer(t *testing.T, async bool) (*ca.CA, *atomic.Int64) {
	t.Helper()
	var issues atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer bot-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		issued := map[string]any{"certificatePem": "LEAF", "chainPem": []string{"INTERMEDIATE\n"}, "notAfter": "2026-11-13T00:00:00.000Z"}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/organizations/org/projects/prj/certificate-authorities/pca_a/certificates":
			var in map[string]any
			if json.NewDecoder(r.Body).Decode(&in) != nil || r.Header.Get("Idempotency-Key") != "key" || in["profile"] != "tlsServerClient" || in["validityDays"] != float64(7) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if sans := in["sans"].(map[string]any); sans["uri"].([]any)[0] != "spiffe://x/y" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			issues.Add(1)
			if async {
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]any{"operation": map[string]any{"id": "op", "status": "running", "target": map[string]any{"id": ""}}})
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(issued)
		case r.URL.Path == "/v1/organizations/org/operations/op":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "op", "status": "succeeded", "target": map[string]any{"id": "pct_a"}})
		case r.URL.Path == "/v1/organizations/org/projects/prj/private-certificates/pct_a":
			_ = json.NewEncoder(w).Encode(issued)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return &ca.CA{BaseURL: server.URL, BotKey: "bot-key", Organization: "org", Project: "prj", Authority: "pca_a", PollEvery: time.Millisecond}, &issues
}

func TestIssue(t *testing.T) {
	for _, async := range []bool{false, true} {
		c, issues := signer(t, async)
		got, err := c.Issue(context.Background(), "key", "CSR", "tlsServerClient", ca.SANs{URI: []string{"spiffe://x/y"}}, 7)
		if err != nil || got.Chain() != "LEAF\nINTERMEDIATE\n" || !got.NotAfter.Equal(time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)) || issues.Load() != 1 {
			t.Fatalf("async %v: %+v, %v", async, got, err)
		}
	}
}

func TestIssueRefusedIsAnError(t *testing.T) {
	c, _ := signer(t, false)
	if _, err := c.Issue(context.Background(), "another-key", "CSR", "tlsServerClient", ca.SANs{URI: []string{"spiffe://x/y"}}, 7); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("%v", err)
	}
}
