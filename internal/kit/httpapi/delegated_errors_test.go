package httpapi_test

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// [being built] Delegated calls' error codes (contracts/api-conventions.md, "Error codes").
func TestDelegatedCallErrors(t *testing.T) {
	cause := errors.New("credential wk_0123456789abcdef has no namespace")
	for _, c := range []struct {
		err          *httpapi.Error
		status       int
		code, issues string
	}{
		{httpapi.CallerUnauthenticated(), 401, "caller_unauthenticated", ""},
		{httpapi.DelegationForbidden(cause), 403, "delegation_forbidden", ""},
		{httpapi.InvalidCallerReference("v must be 1"), 400, "invalid_caller_reference", `[{"path":"X-Ezgh-Caller","message":"v must be 1"}]`},
	} {
		if c.err.Status != c.status || c.err.Code != c.code || c.err.Message == "" {
			t.Errorf("%+v", c.err)
		}
		w := httptest.NewRecorder()
		httpapi.WriteError(w, c.err)
		var body struct {
			Error map[string]json.RawMessage `json:"error"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != c.status || string(body.Error["code"]) != `"`+c.code+`"` || string(body.Error["issues"]) != c.issues {
			t.Errorf("%s: wrote %d %s", c.code, w.Code, w.Body.String())
		}
	}
	if err := httpapi.DelegationForbidden(cause); !errors.Is(err, cause) {
		t.Error("the cause is kept for the log")
	}
}
