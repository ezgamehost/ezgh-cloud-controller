package authz_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// [being built] Delegated calls (contracts/identity-and-authz.md): a batch answer names the
// credential org-api resolved, { kind, id }, additively; absent for one that isn't delegable.
func TestBatchAnswerCredential(t *testing.T) {
	for _, c := range []struct {
		name, extra string
		want        *authz.CredentialRef
	}{
		{"session", `,"credential":{"kind":"session","id":"aB3dE5fG7hJ9kL1mN3pQ5rS7"}`, &authz.CredentialRef{Kind: "session", ID: "aB3dE5fG7hJ9kL1mN3pQ5rS7"}},
		{"api key", `,"credential":{"kind":"apiKey","id":"0199a3c4-1e2f-7a31-9b4c-5d6e7f809102"}`, &authz.CredentialRef{Kind: "apiKey", ID: "0199a3c4-1e2f-7a31-9b4c-5d6e7f809102"}},
		{"absent", ``, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req := readBatch(t, r)
				check, _ := json.Marshal(req.Checks[0])
				_, _ = io.WriteString(w, `{`+testIdentity+c.extra+`,"results":[`+strings.TrimSuffix(string(check), "}")+`,"allowed":true}]}`)
			}))
			defer server.Close()
			client := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
			answer, herr := client.CheckBatch(context.Background(), authz.Credential{Header: "Authorization", Value: testAPIKey}, testOrg, checks(1))
			if herr != nil {
				t.Fatal(herr)
			}
			if (answer.Credential == nil) != (c.want == nil) || (c.want != nil && *answer.Credential != *c.want) {
				t.Errorf("Credential = %+v, want %+v", answer.Credential, c.want)
			}
		})
	}
	// Split calls keep it (from the first successful call, as the rest of the identity).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readBatch(t, r)
		var results []string
		for _, c := range req.Checks {
			check, _ := json.Marshal(c)
			results = append(results, strings.TrimSuffix(string(check), "}")+`,"allowed":true}`)
		}
		_, _ = io.WriteString(w, `{`+testIdentity+`,"credential":{"kind":"oauth","id":"t1"},"results":[`+strings.Join(results, ",")+`]}`)
	}))
	defer server.Close()
	client := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
	answer, herr := client.CheckBatch(context.Background(), authz.Credential{Header: "Authorization", Value: testAPIKey}, testOrg, checks(120))
	if herr != nil || answer.Credential == nil || *answer.Credential != (authz.CredentialRef{Kind: "oauth", ID: "t1"}) {
		t.Errorf("split answer credential = %+v, %v", answer, herr)
	}
	// It encodes back as given, and not at all when absent.
	raw, _ := json.Marshal(authz.BatchAnswer{Credential: &authz.CredentialRef{Kind: "session", ID: "s"}})
	if !strings.Contains(string(raw), `"credential":{"kind":"session","id":"s"}`) {
		t.Errorf("encoded %s", raw)
	}
	raw, _ = json.Marshal(authz.BatchAnswer{})
	if strings.Contains(string(raw), "credential") {
		t.Errorf("encoded %s", raw)
	}
}

// Every org-api call sends the request's ID (contracts/api-conventions.md, X-Request-Id
// propagation), and none when the context carries none.
func TestOrgAPIRequestID(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Request-Id"))
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := &authz.Client{BaseURL: server.URL, ServiceCredential: testServiceCredential}
	cred := authz.Credential{Header: "Authorization", Value: testAPIKey}
	calls := map[string]func(ctx context.Context){
		"Check": func(ctx context.Context) {
			_, _ = client.Check(ctx, cred, testOrg, []string{"widgets.widgets.get"}, "")
		},
		"CheckBatch": func(ctx context.Context) { _, _ = client.CheckBatch(ctx, cred, testOrg, checks(1)) },
		"CheckPrincipalBatch": func(ctx context.Context) {
			_, _ = client.CheckPrincipalBatch(ctx, testOrg, authz.Principal{Type: "user", ID: "u"}, checks(1))
		},
	}
	for name, call := range calls {
		got = nil
		call(httpapi.ContextWithRequestID(context.Background(), "req-0199a3c2"))
		call(context.Background())
		if len(got) != 2 || got[0] != "req-0199a3c2" || got[1] != "" {
			t.Errorf("%s sent X-Request-Id %q, want the context's, then none", name, got)
		}
	}
}
