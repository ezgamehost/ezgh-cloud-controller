package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"text/template"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/yaml"
)

type bundle struct{}

func (bundle) Config(key string) string { return map[string]string{"ezgh.caBundle": "Q0E="}[key] }
func (bundle) Image(string) string      { return "" }

// The bootstrap manifest renders with the add-on functions, and the readiness webhook is the one
// the lb subcommand serves.
func TestReadinessWebhookManifest(t *testing.T) {
	src, err := os.ReadFile("bootstrap/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := template.Must(template.New("m").Parse(string(src))).Execute(&out, bundle{}); err != nil {
		t.Fatal(err)
	}
	for _, doc := range strings.Split(out.String(), "\n---\n") {
		if !strings.Contains(doc, "kind: MutatingWebhookConfiguration") || !strings.Contains(doc, "ezgh-lb-readiness") {
			continue
		}
		var cfg admissionregistrationv1.MutatingWebhookConfiguration
		if err := yaml.UnmarshalStrict([]byte(doc), &cfg); err != nil {
			t.Fatal(err)
		}
		w := cfg.Webhooks[0]
		if *w.ClientConfig.URL != "https://127.0.0.1:9446/mutate-pods" || string(w.ClientConfig.CABundle) != "CA" ||
			*w.FailurePolicy != admissionregistrationv1.Ignore || *w.TimeoutSeconds != 3 || *w.SideEffects != admissionregistrationv1.SideEffectClassNone ||
			w.Rules[0].Operations[0] != admissionregistrationv1.Create || w.Rules[0].Resources[0] != "pods" {
			t.Fatalf("%+v", w)
		}
		return
	}
	t.Fatal("no ezgh-lb-readiness webhook")
}
