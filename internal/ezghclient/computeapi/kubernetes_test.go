package computeapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

func TestTerminateMember(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/organizations/o/projects/p/autoscaling-groups/asg_a/members/ins_a/terminate" ||
			string(body) != `{"decrementDesired":true}` || r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("%s %s %s key=%q", r.Method, r.URL.Path, body, r.Header.Get("Idempotency-Key"))
		}
		_, _ = w.Write([]byte(`{"instanceId":"ins_a","state":"retiring"}`))
	}))
	defer srv.Close()
	c := &Client{API: &apiclient.Client{Service: "Compute", BaseURL: srv.URL, HTTP: srv.Client()}}
	m, err := c.TerminateMember(context.Background(), "o", "p", "asg_a", "ins_a", true)
	if err != nil || m.State != "retiring" {
		t.Fatal(m, err)
	}
}
