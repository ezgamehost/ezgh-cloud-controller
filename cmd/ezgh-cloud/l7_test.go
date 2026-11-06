package main

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
	"text/template"

	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	"sigs.k8s.io/yaml"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/l7"
)

// The bootstrap manifest carries the ezgh IngressClass and GatewayClass (our controller), Gateway
// API's standard CRDs at the pinned release before the GatewayClass, and the lb RBAC they need.
func TestL7Manifest(t *testing.T) {
	src, err := os.ReadFile("bootstrap/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := template.Must(template.New("m").Parse(string(src))).Execute(&out, bundle{}); err != nil {
		t.Fatal(err)
	}
	var crds []string
	var ingressClass, gatewayClass bool
	var lbRules []rbacv1.PolicyRule
	for _, doc := range strings.Split(out.String(), "\n---\n") {
		switch {
		case strings.Contains(doc, "\nkind: CustomResourceDefinition"):
			var crd struct {
				metav1.ObjectMeta `json:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(doc), &crd); err != nil {
				t.Fatal(err)
			}
			if crd.Annotations["gateway.networking.k8s.io/bundle-version"] != "v1.6.3" || crd.Annotations["gateway.networking.k8s.io/channel"] != "standard" {
				t.Fatal(crd.Name, crd.Annotations)
			}
			crds = append(crds, crd.Name)
		case strings.Contains(doc, "\nkind: IngressClass"):
			var ic networkingv1.IngressClass
			if err := yaml.UnmarshalStrict([]byte(doc), &ic); err != nil {
				t.Fatal(err)
			}
			ingressClass = ic.Name == l7.ClassName && ic.Spec.Controller == l7.ControllerName
		case strings.Contains(doc, "\nkind: GatewayClass"):
			var gc gatewayv1.GatewayClass
			if err := yaml.UnmarshalStrict([]byte(doc), &gc); err != nil {
				t.Fatal(err)
			}
			gatewayClass = gc.Name == l7.ClassName && gc.Spec.ControllerName == l7.ControllerName && slices.Contains(crds, "gatewayclasses.gateway.networking.k8s.io")
		case strings.Contains(doc, "\nkind: ClusterRole\nmetadata:") && strings.Contains(doc, "name: ezgh-cloud-lb\n"):
			var cr rbacv1.ClusterRole
			if err := yaml.UnmarshalStrict([]byte(doc), &cr); err != nil {
				t.Fatal(err)
			}
			lbRules = cr.Rules
		}
	}
	for _, want := range []string{"gateways", "httproutes", "referencegrants", "gatewayclasses"} {
		if !slices.Contains(crds, want+".gateway.networking.k8s.io") {
			t.Fatal("no CRD", want, crds)
		}
	}
	if !ingressClass || !gatewayClass {
		t.Fatal("IngressClass or GatewayClass ezgh missing, or before the CRDs", ingressClass, gatewayClass)
	}
	can := func(group, resource, verb string) bool {
		return slices.ContainsFunc(lbRules, func(r rbacv1.PolicyRule) bool {
			return slices.Contains(r.APIGroups, group) && slices.Contains(r.Resources, resource) && slices.Contains(r.Verbs, verb)
		})
	}
	for _, c := range [][3]string{
		{"networking.k8s.io", "ingresses", "patch"}, {"networking.k8s.io", "ingresses/status", "patch"}, {"", "secrets", "watch"}, {"", "namespaces", "list"},
		{"gateway.networking.k8s.io", "gateways", "patch"}, {"gateway.networking.k8s.io", "httproutes/status", "patch"},
		{"gateway.networking.k8s.io", "gatewayclasses/status", "patch"}, {"gateway.networking.k8s.io", "referencegrants", "list"},
	} {
		if !can(c[0], c[1], c[2]) {
			t.Fatal("ezgh-cloud-lb can't", c)
		}
	}
	if can("", "secrets", "patch") || can("gateway.networking.k8s.io", "httproutes", "patch") {
		t.Fatal("ezgh-cloud-lb writes more than it needs")
	}
}
