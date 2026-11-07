package l7

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

// tlsSecret is a valid kubernetes.io/tls Secret for host; for "", one with no DNS names and CN
// "default", as Gateway API conformance's.
func tlsSecret(t *testing.T, ns, name, host string) *corev1.Secret {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cn, names := host, []string{host}
	if host == "" {
		cn, names = "default", nil
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(key)
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{
		corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}),
	}}
}

func webModel() Model {
	web := BackendKey("shop", "web", 80)
	return Model{
		Key: "ing-shop", Name: "k8s-ing-shop", Tags: lb.Tags(testCfg, "shop"),
		Backends: []Backend{{Key: web, Service: "shop/web", Port: 80}},
		Listeners: []Listener{{Port: 80, Protocol: "http", Default: fixed(404), Rules: []Rule{
			{Source: "Ingress shop/web", Hosts: []string{"shop.example.com"}, Paths: []PathMatch{{Type: "prefix", Value: "/"}}, Action: forward(Weighted{Backend: web, Weight: 1})},
		}}},
	}
}

func converge(t *testing.T, c *ezghclient.Clients, st State, m Model) State {
	t.Helper()
	for range 3 {
		var err error
		if st, err = Apply(ctx, c, testCfg, st, m); err != nil {
			t.Fatal(err)
		}
		if st.Status == "active" && len(st.Listeners) == len(m.Listeners) {
			return st
		}
	}
	t.Fatalf("not converged: %+v", st)
	return st
}

func TestApplyMakesL7(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, webModel())
	w := f.log()
	if len(w) != 4 || w[0] != "POST /ipv4-addresses" || w[1] != "POST /target-groups" || w[2] != "POST /load-balancers" || !strings.HasSuffix(w[3], "/listeners") {
		t.Fatal(w)
	}
	g := f.bodies[1]
	if g["protocol"] != "http" || g["proxyProtocol"] != "none" || !strings.HasPrefix(f.keys[1], "ing-shop-shop.web.80-") {
		t.Fatal(g, f.keys[1])
	}
	body := f.bodies[3]
	rule := body["rules"].([]any)[0].(map[string]any)
	fwd := rule["action"].(map[string]any)["forward"].(map[string]any)
	if body["protocol"] != "http" || fwd["targetGroups"].([]any)[0].(map[string]any)["targetGroup"] != st.Groups[BackendKey("shop", "web", 80)] {
		t.Fatal(body)
	}
	// Routes without header modifiers never send the fields (lb-api refuses them while P5 is off).
	if _, ok := fwd["requestHeaderModifier"]; ok {
		t.Fatal(fwd)
	}
	if da := body["defaultAction"].(map[string]any); da["type"] != "fixedResponse" || da["fixedResponse"].(map[string]any)["statusCode"] != 404.0 {
		t.Fatal(da)
	}
	f.reset()
	if _, err := Apply(ctx, c, testCfg, st, webModel()); err != nil || len(f.log()) != 0 {
		t.Fatal(err, f.log())
	}
}

func TestApplyPatchesWithIfMatch(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, webModel())
	m := webModel()
	m.Listeners[0].Rules[0].Paths[0].Value = "/shop"
	f.reset()
	f.fail["PATCH /load-balancers/"] = [2]string{"412", "precondition_failed"} // changed since our read
	st, err := Apply(ctx, c, testCfg, st, m)
	if err != nil {
		t.Fatal(err)
	}
	w := f.log()
	if len(w) != 2 || !strings.HasPrefix(w[0], "PATCH ") || !strings.HasPrefix(w[1], "PATCH ") || f.ifMatch[0] != `"1"` || f.ifMatch[1] != `"1"` {
		t.Fatal(w, f.ifMatch)
	}
	if got := f.listeners[st.Listeners[80]].Rules[0].Match.Paths[0].Value; got != "/shop" {
		t.Fatal(got)
	}
	// A second 412 in a row goes back to the workqueue.
	m.Listeners[0].Rules[0].Paths[0].Value = "/x"
	f.fail["PATCH /load-balancers/"] = [2]string{"412", "precondition_failed"}
	f.fail["PATCH /load-balancers/lbr_"] = [2]string{"412", "precondition_failed"}
	if _, err := Apply(ctx, c, testCfg, st, m); ezghclient.Code(err) != "precondition_failed" {
		t.Fatal(err)
	}
}

func TestHeaderModifiersSerialized(t *testing.T) {
	f, c := newFake(t)
	m := webModel()
	edits := &HeaderModifier{Set: []NameValue{{Name: "X-Env", Value: "prod"}}, Remove: []string{"X-Debug"}}
	m.Listeners[0].Rules[0].Action.RequestHeaders = edits
	m.Listeners[0].Rules[0].Action.ResponseHeaders = &HeaderModifier{Add: []NameValue{{Name: "Cache-Control", Value: "no-store"}}}
	converge(t, c, State{}, m)
	body := f.body("POST /load-balancers/")
	fwd := body["rules"].([]any)[0].(map[string]any)["action"].(map[string]any)["forward"].(map[string]any)
	got, _ := json.Marshal([]any{fwd["requestHeaderModifier"], fwd["responseHeaderModifier"]})
	want := `[{"add":[],"remove":["X-Debug"],"set":[{"name":"X-Env","value":"prod"}]},{"add":[{"name":"Cache-Control","value":"no-store"}],"remove":[],"set":[]}]`
	if string(got) != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
}

func TestSecretImportedOnceReimportedOnChangeDeletedLast(t *testing.T) {
	f, c := newFake(t)
	m := webModel()
	s := tlsSecret(t, "shop", "tls", "shop.example.com")
	m.Secrets = map[string]*corev1.Secret{"shop/tls": s}
	m.Listeners = append(m.Listeners, Listener{Port: 443, Protocol: "https", Secrets: []string{"shop/tls"}, Default: m.Listeners[0].Default, Rules: m.Listeners[0].Rules})
	st := converge(t, c, State{}, m)
	crt := st.Certificates["shop/tls"].ID
	if f.log()[0] != "POST /certificates/import" || !strings.HasPrefix(f.keys[0], "ing-shop-crt-") || f.listeners[st.Listeners[443]].Certificate == nil ||
		*f.listeners[st.Listeners[443]].Certificate != crt {
		t.Fatal(f.log(), st)
	}
	if b := f.bodies[0]; b["privateKey"] == "" || b["certificateChain"] != nil || b["tags"].(map[string]any)["ezgh:kubernetes-service"] != "shop" {
		t.Fatal(b)
	}
	stateJSON, _ := json.Marshal(st)
	if strings.Contains(string(stateJSON), "PRIVATE KEY") {
		t.Fatal("key in state")
	}
	f.reset()
	if st, _ = Apply(ctx, c, testCfg, st, m); len(f.log()) != 0 {
		t.Fatal(f.log())
	}
	m.Secrets["shop/tls"] = tlsSecret(t, "shop", "tls", "shop.example.com")
	st, err := Apply(ctx, c, testCfg, st, m)
	if w := f.log(); err != nil || len(w) != 1 || w[0] != "POST /certificates/"+crt+"/import" || st.Certificates["shop/tls"].ID != crt {
		t.Fatal(err, w)
	}

	// Teardown: the load balancer goes (its listeners with it), then the group, the pip_, and the
	// imported certificate last.
	f.reset()
	gone := Model{Tags: m.Tags, Named: m.Name}
	for range 3 {
		if st, err = Apply(ctx, c, testCfg, st, gone); err != nil {
			t.Fatal(err)
		}
		if st.LoadBalancer != "" {
			f.lbs[st.LoadBalancer]["status"] = "deleted"
		}
	}
	w := f.log()
	if !st.Empty() || len(w) != 4 || !strings.HasPrefix(w[0], "DELETE /load-balancers/") || w[3] != "DELETE /certificates/"+crt {
		t.Fatal(st, w)
	}
}

// Review Focus 1: a refused read deletes nothing, and the error says which action.
func TestApplyDeniedKeepsState(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, webModel())
	f.reset()
	f.fail["GET /load-balancers/"] = [2]string{"403", "access_denied"}
	got, err := Apply(ctx, c, testCfg, st, Model{Tags: webModel().Tags, Named: webModel().Name})
	if p, ok := ezghclient.AsPermission(err); !ok || p.Action != "lb.loadBalancers.create" || len(f.log()) != 0 || got.LoadBalancer != st.LoadBalancer {
		t.Fatal(err, f.log(), got)
	}
}
