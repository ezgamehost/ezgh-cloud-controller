package service_test

import (
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

const projectID = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f71"

// errorModel openai (contracts/api-conventions.md, "OpenAI-compatible surface"): every failure the
// kit or the handler raises is the OpenAI API's body, the endpoint takes bearer credentials only,
// and the rest of the API keeps the platform's body.
func TestOpenAIEndpointAnswersKitFailuresInOpenAIShape(t *testing.T) {
	const path = "/v1/organizations/:orgId/projects/:projectId/openai/v1/embeddings"
	extra := []manifest.Endpoint{{
		Method: "POST", Path: path, Auth: "caller", Action: "widgets.models.invoke", Credentials: manifest.CredentialsBearer,
		ErrorModel: manifest.ErrorModelOpenAI, IdempotencyException: "computation", Trail: &manifest.Trail{EventName: "CreateEmbeddings", Sensitive: []string{"input"}},
	}}
	h := newHarnessWith(t, nil, extra, func(s *service.Service) {
		s.Handle("POST", path, func(c *service.Call) error {
			if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
				return err
			}
			switch body := string(c.Body()); {
			case strings.Contains(body, "retired"):
				return &httpapi.Error{Status: 410, Code: "model_retired", Message: "qwen3-embedding-0.6b:0.9.0 is retired", Extra: map[string]any{"param": "model"}}
			case strings.Contains(body, "monthly"):
				return &httpapi.Error{Status: 429, Code: httpapi.OpenAIInsufficientQuota, Message: "This organization used its monthly tokens"}
			}
			c.JSON(200, map[string]any{"object": "list", "data": []any{}})
			return nil
		})
	})
	url := "/v1/organizations/" + orgID + "/projects/" + projectID + "/openai/v1/embeddings"
	bearer := map[string]string{"Authorization": "Bearer " + apiKey}
	for _, c := range []struct {
		name, target, body string
		headers            map[string]string
		status             int
		typ, code          string
		param              any
		retryAfter         string
	}{
		{"no credential", url, `{}`, nil, 401, "authentication_error", "unauthenticated", nil, ""},
		{"a session cookie only: the endpoint is bearer-only", url, `{}`, member, 401, "authentication_error", "unauthenticated", nil, ""},
		{"a malformed project ID", "/v1/organizations/" + orgID + "/projects/not-a-uuid/openai/v1/embeddings", `{}`, bearer, 404, "not_found_error", "not_found", nil, ""},
		{"a malformed organization ID", "/v1/organizations/nope/projects/" + projectID + "/openai/v1/embeddings", `{}`, bearer, 404, "not_found_error", "not_found", nil, ""},
		{"a body over the limit", url, `{"input":"` + strings.Repeat("a", httpapi.MaxBodyBytes) + `"}`, bearer, 413, "invalid_request_error", "payload_too_large", nil, ""},
		{"denied", url, `{}`, map[string]string{"Authorization": "Bearer " + deniedToken}, 403, "permission_error", "access_denied", nil, ""},
		{"the handler's own error", url, `{"model":"retired"}`, bearer, 410, "invalid_request_error", "model_retired", "model", ""},
		{"the monthly limit: no Retry-After", url, `{"model":"monthly"}`, bearer, 429, "insufficient_quota", "insufficient_quota", nil, ""},
	} {
		r := h.do(t, "POST", c.target, c.body, c.headers)
		e, _ := r.body["error"].(map[string]any)
		if r.status != c.status || e == nil || e["type"] != c.typ || e["code"] != c.code || e["param"] != c.param {
			t.Errorf("%s: %d %v", c.name, r.status, r.body)
			continue
		}
		if _, ok := e["message"].(string); !ok {
			t.Errorf("%s: no message: %v", c.name, e)
		}
		if _, ok := e["param"]; !ok {
			t.Errorf("%s: no param: %v", c.name, e)
		}
		if got := r.header.Get("Retry-After"); got != c.retryAfter {
			t.Errorf("%s: Retry-After %q", c.name, got)
		}
		if r.header.Get("X-Request-Id") == "" {
			t.Errorf("%s: no X-Request-Id", c.name)
		}
	}
	if r := h.do(t, "POST", url, `{"model":"monthly"}`, bearer); r.header.Get("x-should-retry") != "false" {
		t.Errorf("the monthly limit: x-should-retry %q", r.header.Get("x-should-retry"))
	}
	if r := h.do(t, "POST", url, `{"model":"m","input":"hi"}`, bearer); r.status != 200 || r.body["object"] != "list" {
		t.Errorf("bearer: %d %v", r.status, r.body)
	}
	// The rest of the API keeps the platform's body.
	if r := h.do(t, "GET", things, "", nil); r.status != 401 || code(r) != "unauthenticated" || r.body["error"].(map[string]any)["type"] != nil {
		t.Errorf("things: %d %v", r.status, r.body)
	}
	// The router's own answers under an OpenAI base are OpenAI's body too: an unknown path (a
	// client calling chat/completions before it exists) and a method the path doesn't take.
	base := "/v1/organizations/" + orgID + "/projects/" + projectID + "/openai/v1"
	for _, c := range []struct{ method, target string }{{"POST", base + "/chat/completions"}, {"GET", url}, {"POST", base + "/"}} {
		r := h.do(t, c.method, c.target, `{}`, bearer)
		e, _ := r.body["error"].(map[string]any)
		if r.status != 404 || e == nil || e["type"] != "not_found_error" || e["code"] != "not_found" || r.header.Get("X-Request-Id") == "" {
			t.Errorf("%s %s: %d %v", c.method, c.target, r.status, r.body)
		}
		if _, ok := e["param"]; !ok {
			t.Errorf("%s %s: no param", c.method, c.target)
		}
	}
	// Elsewhere the router keeps the platform's body.
	for _, target := range []string{"/nope", "/v1/organizations/" + orgID + "/projects/" + projectID + "/openai/v2/embeddings"} {
		if r := h.do(t, "POST", target, `{}`, bearer); r.status != 404 || code(r) != "not_found" || r.body["error"].(map[string]any)["type"] != nil {
			t.Errorf("%s: %d %v", target, r.status, r.body)
		}
	}
}
