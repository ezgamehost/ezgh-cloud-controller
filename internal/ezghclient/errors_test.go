package ezghclient

import (
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

func TestDeniedActionParsed(t *testing.T) {
	for msg, want := range map[string]string{
		"This API key isn't allowed to lb.loadBalancers.create":    "lb.loadBalancers.create",
		"You're not allowed to certificates.certificates.use.":     "certificates.certificates.use",
		"This API key isn't allowed to do that":                    "",
		"Access denied":                                            "",
		"This API key isn't allowed to compute.instances.get here": "",
	} {
		p, ok := AsPermission(&apiclient.Error{Status: 403, Code: "access_denied", Message: msg})
		if !ok || p.Action != want {
			t.Errorf("%q: got %+v, want action %q", msg, p, want)
		}
	}
	if _, ok := AsPermission(&apiclient.Error{Status: 404}); ok {
		t.Error("404 isn't a permission error")
	}
}
