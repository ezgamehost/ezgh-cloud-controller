package trails_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

func same(t *testing.T, name string, got, want any) {
	t.Helper()
	g, err1 := canonical.Marshal(got)
	w, err2 := canonical.Marshal(want)
	if err1 != nil || err2 != nil || string(g) != string(w) {
		t.Errorf("%s:\n got %s\nwant %s", name, g, w)
	}
}

// contracts/vectors/trails-recording.json.
func TestRecordingVectors(t *testing.T) {
	var file struct {
		Hidden            string `json:"hidden"`
		RequestParameters []struct {
			Name  string `json:"name"`
			Input struct {
				Path  map[string]string `json:"path"`
				Query map[string]string `json:"query"`
				Body  json.RawMessage   `json:"body"`
			} `json:"input"`
			Sensitive []string        `json:"sensitive"`
			Expected  json.RawMessage `json:"expected"`
		} `json:"requestParameters"`
		DefaultRequestParameters []struct {
			Name  string `json:"name"`
			Input struct {
				Path    map[string]string `json:"path"`
				Query   map[string]string `json:"query"`
				Body    json.RawMessage   `json:"body"`
				Refused bool              `json:"refused"`
			} `json:"input"`
			Sensitive []string        `json:"sensitive"`
			Expected  json.RawMessage `json:"expected"`
		} `json:"defaultRequestParameters"`
		ResponseElements []struct {
			Name     string          `json:"name"`
			Body     json.RawMessage `json:"body"`
			Extra    []string        `json:"extra"`
			Expected json.RawMessage `json:"expected"`
		} `json:"responseElements"`
	}
	vectors.Load(t, "trails-recording.json", &file)
	if file.Hidden != trails.Hidden {
		t.Fatalf("hidden is %q", file.Hidden)
	}
	decode := func(raw json.RawMessage) any {
		if len(raw) == 0 {
			return nil
		}
		v, err := canonical.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range file.RequestParameters {
		query := map[string]any{}
		for k, v := range c.Input.Query {
			query[k] = v
		}
		got := trails.RequestParameters(c.Input.Path, query, decode(c.Input.Body), c.Sensitive)
		var gotAny any
		if got != nil {
			gotAny = got
		}
		same(t, c.Name, gotAny, decode(c.Expected))
	}
	for _, c := range file.DefaultRequestParameters {
		query := map[string]any{}
		for k, v := range c.Input.Query {
			query[k] = v
		}
		body := decode(c.Input.Body)
		if c.Input.Refused { // the service package leaves a refused call's body out
			body = nil
		}
		got := trails.DefaultRequestParameters(c.Input.Path, query, body, c.Sensitive)
		var gotAny any
		if got != nil {
			gotAny = got
		}
		same(t, "default: "+c.Name, gotAny, decode(c.Expected))
	}
	for _, c := range file.ResponseElements {
		got := trails.ResponseElements(decode(c.Body), c.Extra)
		var gotAny any
		if got != nil {
			gotAny = got
		}
		same(t, c.Name, gotAny, decode(c.Expected))
	}
}

// An oversized event keeps its identity and loses its parameters, never the event.
func TestFitTruncates(t *testing.T) {
	big := strings.Repeat("x", 70*1024)
	ua := strings.Repeat("u", 600)
	e := &trails.Event{
		EventVersion: "1.0", EventName: "CreateWidget", UserIdentity: trails.ServiceIdentity("organizations.ezghcloud.com"),
		RequestParameters: map[string]any{"name": big}, ResponseElements: map[string]any{"id": "x"}, UserAgent: &ua,
		Resources: []trails.Resource{},
	}
	e.Fit()
	if e.RequestParameters["truncated"] != true || e.ResponseElements["truncated"] != true || len(*e.UserAgent) != 512 {
		t.Errorf("not truncated: %v %v %d", e.RequestParameters["truncated"], e.ResponseElements, len(*e.UserAgent))
	}
}

// A project ingest key's identity is exactly the contract example's (contracts/trails.md, "Ingest
// keys"): the key as principal and access key, nothing else set.
func TestIngestKeyIdentity(t *testing.T) {
	var event struct {
		UserIdentity json.RawMessage `json:"userIdentity"`
	}
	if err := json.Unmarshal(vectors.File(t, "examples/obs-auto-create.trail-event.json"), &event); err != nil {
		t.Fatal(err)
	}
	want, _ := canonical.Decode(event.UserIdentity)
	got, err := canonical.Decode(trails.IngestKeyIdentity("0199a3c4-2f30-7b42-8c5d-6e7f8091a2b3"))
	if err != nil {
		t.Fatal(err)
	}
	same(t, "IngestKeyIdentity", got, want)
}

// contracts/vectors/trails-recording.json's s3AccessKey identities: the key's owner, as principal-batch
// names them, with the key's oak_ ID as the access key and no session; no identity (an error) for any
// other owner or an ID that isn't one (contracts/trails.md, "S3 access keys").
func TestS3AccessKeyIdentity(t *testing.T) {
	var file struct {
		S3AccessKey struct {
			Identities []struct {
				Name     string          `json:"name"`
				Owner    json.RawMessage `json:"owner"`
				Key      string          `json:"key"`
				Expected json.RawMessage `json:"expected"`
			} `json:"identities"`
		} `json:"s3AccessKey"`
	}
	vectors.Load(t, "trails-recording.json", &file)
	if len(file.S3AccessKey.Identities) < 8 {
		t.Fatalf("only %d cases", len(file.S3AccessKey.Identities))
	}
	for _, c := range file.S3AccessKey.Identities {
		got, err := trails.S3AccessKeyIdentity(c.Owner, c.Key)
		if string(c.Expected) == "null" {
			if err == nil {
				t.Errorf("%s: got %s, want an error", c.Name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		g, _ := canonical.Decode(got)
		w, _ := canonical.Decode(c.Expected)
		same(t, c.Name, g, w)
	}
}

// contracts/vectors/trails-recording.json's acmeBinding identities: the binding owner, as
// principal-batch names them, with the binding's eab_ slug as the access key and no session; no
// identity (an error) for any other owner or a slug that isn't one (contracts/trails.md, "ACME
// bindings").
func TestAcmeBindingIdentity(t *testing.T) {
	var file struct {
		AcmeBinding struct {
			Identities []struct {
				Name     string          `json:"name"`
				Owner    json.RawMessage `json:"owner"`
				Binding  string          `json:"binding"`
				Expected json.RawMessage `json:"expected"`
			} `json:"identities"`
		} `json:"acmeBinding"`
	}
	vectors.Load(t, "trails-recording.json", &file)
	if len(file.AcmeBinding.Identities) < 8 {
		t.Fatalf("only %d cases", len(file.AcmeBinding.Identities))
	}
	for _, c := range file.AcmeBinding.Identities {
		got, err := trails.AcmeBindingIdentity(c.Owner, c.Binding)
		if string(c.Expected) == "null" {
			if err == nil {
				t.Errorf("%s: got %s, want an error", c.Name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		g, _ := canonical.Decode(got)
		w, _ := canonical.Decode(c.Expected)
		same(t, c.Name, g, w)
	}

	// The contract's example: the owner from the principal-batch answer, the binding from the
	// event's resources.
	var answer struct {
		UserIdentity json.RawMessage `json:"userIdentity"`
	}
	if err := json.Unmarshal(vectors.File(t, "examples/acme-owner.principal-batch-response.json"), &answer); err != nil {
		t.Fatal(err)
	}
	var event struct {
		UserIdentity json.RawMessage `json:"userIdentity"`
	}
	if err := json.Unmarshal(vectors.File(t, "examples/acme-issue.trail-event.json"), &event); err != nil {
		t.Fatal(err)
	}
	got, err := trails.AcmeBindingIdentity(answer.UserIdentity, "eab_3n8q1w5z0k2r")
	if err != nil {
		t.Fatal(err)
	}
	g, _ := canonical.Decode(got)
	w, _ := canonical.Decode(event.UserIdentity)
	same(t, "acme-issue.trail-event.json", g, w)

	// An owner that isn't an identity at all.
	for _, owner := range []string{``, `null`, `[]`, `{"type":"User"`} {
		if _, err := trails.AcmeBindingIdentity(json.RawMessage(owner), "eab_0123456789ab"); err == nil {
			t.Errorf("owner %q: no error", owner)
		}
	}
}
