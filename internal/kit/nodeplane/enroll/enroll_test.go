package enroll_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/enroll"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/signals"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

const (
	cn       = "x-node"
	platform = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	action   = "x.enrollments.create"
)

func csrText(t *testing.T, name string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func TestReadCSRHoldsTheCommonName(t *testing.T) {
	text := csrText(t, cn)
	first, ok := enroll.ReadCSR(text, cn)
	if !ok || first == "" {
		t.Fatal("refused its own CN")
	}
	if _, ok := enroll.ReadCSR(text, "another"); ok {
		t.Error("took another CN")
	}
	if second, ok := enroll.ReadCSR(text, ""); !ok || second != first {
		t.Errorf("%q, %v", second, ok)
	}
}

// store is a product's rows, in memory.
type store struct {
	mu      sync.Mutex
	rows    map[string]*enroll.Row
	byKey   map[string]string
	unknown bool
}

func (s *store) Row(_ context.Context, instance, key, _ string) (enroll.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unknown {
		return enroll.Row{}, enroll.ErrUnknown
	}
	if id, ok := s.byKey[instance+key]; ok {
		return *s.rows[id], nil
	}
	id := fmt.Sprintf("row-%d", len(s.rows))
	s.rows[id], s.byKey[instance+key] = &enroll.Row{ID: id}, id
	return *s.rows[id], nil
}

func (s *store) Get(_ context.Context, id string) (enroll.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.rows[id], nil
}

// orgAPI answers the one check enrollment makes, as a key with source.
func orgAPI(source string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"organization": map[string]any{"id": platform, "name": "EZGH", "slug": "org_aaaaaaaaaaaa", "resourceName": "ezgh::org_aaaaaaaaaaaa", "isRoot": false},
			"principal":    map[string]any{"type": "bot", "id": "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f71"},
			"apiKeyId":     "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f72",
			"userIdentity": map[string]any{"type": "Bot", "principalId": "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f71", "userName": "node", "isRoot": false,
				"accessKeyId": "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f72", "invokedBy": nil,
				"sessionContext": map[string]any{"credential": "apiKey", "sessionIssuedAt": nil, "ssoProviderId": nil, "mfaAuthenticated": false,
					"sourceIdentity": map[string]any{"type": source, "principalId": "ins_aaaaaaaaaaaa", "userName": "node"}}},
			"resource": "", "allowed": []string{action},
		})
	})
}

func harness(t *testing.T, source string, s *store) (string, *redis.Client, string) {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	org := httptest.NewServer(orgAPI(source))
	t.Cleanup(org.Close)
	stream := fmt.Sprintf("test:signals:%d", time.Now().UnixNano())
	h := &enroll.Handler{OrgAPI: &authz.Client{BaseURL: org.URL, InternalToken: "t"}, Redis: client, Signals: &signals.Publisher{Redis: client, Stream: stream},
		Store: s, Prefix: "test", Action: action, PlatformOrganization: platform, CN: cn, Logger: testdb.Logger(), Wait: 3 * time.Second,
		Respond: func(certificate string, notAfter time.Time) any {
			return map[string]any{"chainPem": certificate, "notAfter": notAfter}
		}}
	server := httptest.NewServer(h.Routes())
	t.Cleanup(server.Close)
	return server.URL, client, stream
}

func post(t *testing.T, url, csr string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"csr": csr})
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/enroll", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer ezgh_key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The request asks the controller and waits: what it issues is the answer, through Respond.
func TestEnrollWaitsForTheController(t *testing.T) {
	s := &store{rows: map[string]*enroll.Row{}, byKey: map[string]string{}}
	url, client, stream := harness(t, "ComputeInstance", s)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	controller := &signals.Consumer{Redis: client, Stream: signals.IssueStream(stream), Group: "test-controller", Name: "a", Logger: testdb.Logger(),
		Handle: func(ctx context.Context, sig signals.Signal) error {
			var body struct{ Row string }
			if sig.Kind != signals.KindEnroll || sig.Node != "ins_aaaaaaaaaaaa" || json.Unmarshal(sig.Body, &body) != nil {
				t.Errorf("%+v", sig)
				return nil
			}
			s.mu.Lock()
			certificate, notAfter := "CHAIN", time.Now().Add(time.Hour)
			s.rows[body.Row].CertificatePEM, s.rows[body.Row].NotAfter = &certificate, &notAfter
			s.mu.Unlock()
			_, err := client.Publish(ctx, enroll.IssuedChannel("test", body.Row), "")
			return err
		}}
	go controller.Run(ctx)
	csr := csrText(t, cn)
	status, out := post(t, url, csr)
	if status != http.StatusOK || out["chainPem"] != "CHAIN" {
		t.Fatalf("%d %v", status, out)
	}
	// The same key again is answered from the row.
	if status, out = post(t, url, csr); status != http.StatusOK || out["chainPem"] != "CHAIN" || len(s.rows) != 1 {
		t.Fatalf("%d %v %d", status, out, len(s.rows))
	}
}

// A customer's Bot key with an instance source isn't a product's node key: only ComputeInstance.
func TestOnlyAComputeInstanceEnrolls(t *testing.T) {
	s := &store{rows: map[string]*enroll.Row{}, byKey: map[string]string{}}
	url, _, _ := harness(t, "KubernetesPod", s)
	if status, _ := post(t, url, csrText(t, cn)); status != http.StatusForbidden || len(s.rows) != 0 {
		t.Fatalf("%d, %d rows", status, len(s.rows))
	}
}

func TestUnknownInstanceAndBadCSRRefused(t *testing.T) {
	s := &store{rows: map[string]*enroll.Row{}, byKey: map[string]string{}, unknown: true}
	url, _, _ := harness(t, "ComputeInstance", s)
	if status, _ := post(t, url, csrText(t, cn)); status != http.StatusForbidden {
		t.Errorf("unknown: %d", status)
	}
	if status, _ := post(t, url, csrText(t, "other")); status != http.StatusBadRequest {
		t.Errorf("another CN: %d", status)
	}
}
