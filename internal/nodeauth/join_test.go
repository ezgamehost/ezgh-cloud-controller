package nodeauth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// caPath is absolute: TestNoSecretWritten runs from a temporary directory.
var caPath, _ = filepath.Abs("testdata/ca.crt")

func testCA(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// validFacts is a JoinFacts as kubernetes-controller's launch spec writes it, as a JSON object to edit.
func validFacts(t *testing.T) map[string]any {
	return map[string]any{
		"cluster": "kcl_3k9d0a1m2x7q", "clusterUuid": "0f1d2c3b-4a59-4687-8796-a5b4c3d2e1f0", "org": "org_7k2m9x0a4qpe",
		"project": "prj_r2v8k1m3p0qa", "nodeGroup": "kng_5q2m9x0a7k1d",
		"privateEndpoint": "abcdefghijklmnopqrstuvwxyz.private.k8s.us-west-1.ezghcompute.com", "caBundle": testCA(t),
		"clusterDNS": []any{"10.100.0.10", "fd00:10:96::a"}, "maxPods": 110, "labels": map[string]any{"team": "web"},
		"taints": []any{map[string]any{"key": "dedicated", "value": "gpu", "effect": "NoSchedule"}},
	}
}

func marshal(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestJoinFactsParse(t *testing.T) {
	m := validFacts(t)
	f, err := ParseJoinFacts(marshal(t, m))
	want := JoinFacts{Cluster: "kcl_3k9d0a1m2x7q", ClusterUUID: "0f1d2c3b-4a59-4687-8796-a5b4c3d2e1f0", Org: "org_7k2m9x0a4qpe",
		Project: "prj_r2v8k1m3p0qa", NodeGroup: "kng_5q2m9x0a7k1d", PrivateEndpoint: m["privateEndpoint"].(string), CABundle: testCA(t),
		ClusterDNS: []string{"10.100.0.10", "fd00:10:96::a"}, MaxPods: 110, Labels: map[string]string{"team": "web"},
		Taints: []Taint{{"dedicated", "gpu", "NoSchedule"}}}
	if err != nil || !reflect.DeepEqual(f, want) {
		t.Fatalf("%v\n%+v", err, f)
	}
	if _, err := ParseJoinFacts([]byte("#cloud-config")); err == nil {
		t.Fatal("non-JSON accepted")
	}
}

func TestUnknownFieldsIgnored(t *testing.T) {
	m := validFacts(t)
	m["later"] = map[string]any{"x": true}
	if _, err := ParseJoinFacts(marshal(t, m)); err != nil {
		t.Fatal(err)
	}
}

func TestJoinFactsRefused(t *testing.T) {
	ca := testCA(t)
	for name, edit := range map[string]func(m map[string]any){
		"endpoint elsewhere":       func(m map[string]any) { m["privateEndpoint"] = "abc.k8s.us-west-1.ezghcompute.com" },
		"endpoint suffix only":     func(m map[string]any) { m["privateEndpoint"] = ".private.k8s.us-west-1.ezghcompute.com" },
		"endpoint with a port":     func(m map[string]any) { m["privateEndpoint"] = "abc.private.k8s.us-west-1.ezghcompute.com:6443" },
		"endpoint without a zone":  func(m map[string]any) { m["privateEndpoint"] = "abc.private.k8s.us-west-1" },
		"endpoint upper case":      func(m map[string]any) { m["privateEndpoint"] = "ABC.private.k8s.us-west-1.ezghcompute.com" },
		"endpoint with a path":     func(m map[string]any) { m["privateEndpoint"] = "abc.private.k8s.us-west-1.ezghcompute.com/x" },
		"endpoint label dotted":    func(m map[string]any) { m["privateEndpoint"] = "a.b.private.k8s.us-west-1.ezghcompute.com" },
		"cluster not kcl_":         func(m map[string]any) { m["cluster"] = "kng_3k9d0a1m2x7q" },
		"cluster UUID not a UUID":  func(m map[string]any) { m["clusterUuid"] = "kcl_3k9d0a1m2x7q" },
		"node group not kng_":      func(m map[string]any) { m["nodeGroup"] = "asg_3k9d0a1m2x7q" },
		"maxPods 0":                func(m map[string]any) { m["maxPods"] = 0 },
		"maxPods 111":              func(m map[string]any) { m["maxPods"] = 111 },
		"CA not PEM":               func(m map[string]any) { m["caBundle"] = "MIIC" },
		"CA empty":                 func(m map[string]any) { m["caBundle"] = "" },
		"CA trailing junk":         func(m map[string]any) { m["caBundle"] = ca + "junk" },
		"CA a certificate and key": func(m map[string]any) { m["caBundle"] = ca + strings.ReplaceAll(ca, "CERTIFICATE", "PRIVATE KEY") },
		"CA a key":                 func(m map[string]any) { m["caBundle"] = strings.ReplaceAll(ca, "CERTIFICATE", "PRIVATE KEY") },
		"unknown taint effect":     func(m map[string]any) { m["taints"] = []any{map[string]any{"key": "a", "effect": "NoRun"}} },
		"taint key bad":            func(m map[string]any) { m["taints"] = []any{map[string]any{"key": "a b", "effect": "NoSchedule"}} },
		"label value bad":          func(m map[string]any) { m["labels"] = map[string]any{"a": "x --b"} },
		"clusterDNS IPv6 first":    func(m map[string]any) { m["clusterDNS"] = []any{"fd00:10:96::a", "10.100.0.10"} },
		"clusterDNS one address":   func(m map[string]any) { m["clusterDNS"] = []any{"10.100.0.10"} },
		"clusterDNS not addresses": func(m map[string]any) { m["clusterDNS"] = []any{"a", "b"} },
	} {
		t.Run(name, func(t *testing.T) {
			m := validFacts(t)
			edit(m)
			if _, err := ParseJoinFacts(marshal(t, m)); !errors.Is(err, ErrInvalidJoinFacts) {
				t.Fatal("not refused as invalid:", err)
			}
		})
	}
}

// TestJoinFactsAccepted: the endpoint's region and zone are kubernetes-api's (the local stack is
// us-local-1), and a CA bundle may hold several certificates while the cluster CA rotates.
func TestJoinFactsAccepted(t *testing.T) {
	ca := testCA(t)
	for name, edit := range map[string]func(m map[string]any){
		"another region and zone": func(m map[string]any) { m["privateEndpoint"] = "abc.private.k8s.us-local-1.ezgh.test" },
		"two CA certificates":     func(m map[string]any) { m["caBundle"] = ca + "\n" + ca },
	} {
		t.Run(name, func(t *testing.T) {
			m := validFacts(t)
			edit(m)
			if _, err := ParseJoinFacts(marshal(t, m)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
