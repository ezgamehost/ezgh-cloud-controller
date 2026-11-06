package csr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	certv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

const (
	clusterUUID = "0199c7a2-1111-7000-8000-000000000001"
	nodeName    = "ins-3k9d0a1m2x7q"
	nodeUser    = "system:node:" + nodeName
)

type staticKey struct{}

func (staticKey) APIKey(context.Context) (string, error) { return "kbt_test", nil }

// world is compute and network: the instance's status, or a refused read (403).
type world struct {
	status   string // "" is no such instance (404)
	refused  bool
	foreignC bool
}

func (w *world) clients(t *testing.T) *ezghclient.Clients {
	t.Helper()
	write := func(rw http.ResponseWriter, status int, body any) {
		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(status)
		_ = json.NewEncoder(rw).Encode(body)
	}
	base := "/v1/organizations/org_a/projects/prj_b"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+base+"/instances/ins_3k9d0a1m2x7q", func(rw http.ResponseWriter, r *http.Request) {
		if w.status == "" && !w.refused {
			write(rw, 404, map[string]any{"error": map[string]string{"code": "not_found", "message": "Not found"}})
			return
		}
		if w.refused {
			write(rw, 403, map[string]any{"error": map[string]string{"code": "access_denied", "message": "This API key isn't allowed to compute.instances.get"}})
			return
		}
		cluster := clusterUUID
		if w.foreignC {
			cluster = "0199c7a2-9999-7000-8000-000000000009"
		}
		write(rw, 200, map[string]any{
			"slug": "ins_3k9d0a1m2x7q", "region": "us-west-1", "status": w.status, "instanceType": "gp-1.large",
			"tags":                map[string]string{"ezgh:kubernetes-cluster": cluster},
			"networkInterfaceIds": []string{"eni_public0001", "eni_private001", "eni_moving0001"},
		})
	})
	mux.HandleFunc("GET "+base+"/network-interfaces/eni_public0001", func(rw http.ResponseWriter, r *http.Request) {
		write(rw, 200, map[string]any{
			"slug": "eni_public0001", "vpc": nil, "ipv4Address": map[string]string{"id": "u", "slug": "pip_x", "address": "203.0.113.7"},
			"ipv6Address": "2001:db8:7::1", "attachment": map[string]any{"instance": "ins_3k9d0a1m2x7q", "primary": true},
		})
	})
	mux.HandleFunc("GET "+base+"/network-interfaces/eni_moving0001", func(rw http.ResponseWriter, r *http.Request) {
		write(rw, 200, map[string]any{
			"slug": "eni_moving0001", "vpc": "vpc_c", "privateIpv4Address": "10.0.1.9", "ipv4Address": nil,
			"attachment": map[string]any{"instance": "ins_0ther0000000", "primary": false},
		})
	})
	mux.HandleFunc("GET "+base+"/network-interfaces/eni_private001", func(rw http.ResponseWriter, r *http.Request) {
		write(rw, 200, map[string]any{
			"slug": "eni_private001", "vpc": "vpc_c", "privateIpv4Address": "10.0.1.5", "ipv4Address": nil,
			"ipv6Address": "fd65:7a67:6800:1::5", "attachment": map[string]any{"instance": "ins_3k9d0a1m2x7q", "primary": false},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return ezghclient.NewAt(staticKey{}, "test", func(string) string { return srv.URL })
}

type request struct {
	signer, username string
	groups           []string
	subject          pkix.Name
	dns              []string
	ips              []string
	uris             []string
	usages           []certv1.KeyUsage
	notPEM           bool
}

func clientRequest() request {
	return request{
		signer: certv1.KubeAPIServerClientKubeletSignerName, username: nodeUser, groups: []string{"system:nodes", "system:authenticated"},
		subject: pkix.Name{CommonName: nodeUser, Organization: []string{"system:nodes"}},
		usages:  []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageClientAuth},
	}
}

func servingRequest(ips ...string) request {
	r := clientRequest()
	r.signer = certv1.KubeletServingSignerName
	r.dns, r.ips = []string{nodeName}, ips
	r.usages = []certv1.KeyUsage{certv1.UsageDigitalSignature, certv1.UsageKeyEncipherment, certv1.UsageServerAuth}
	return r
}

func (r request) object(t *testing.T) *certv1.CertificateSigningRequest {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.CertificateRequest{Subject: r.subject, DNSNames: r.dns}
	for _, ip := range r.ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, net.ParseIP(ip))
	}
	for _, u := range r.uris {
		parsed, _ := url.Parse(u)
		tmpl.URIs = append(tmpl.URIs, parsed)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		t.Fatal(err)
	}
	body := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	if r.notPEM {
		body = der
	}
	return &certv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "csr-1"},
		Spec: certv1.CertificateSigningRequestSpec{
			Request:    body,
			SignerName: r.signer, Username: r.username, Groups: r.groups, Usages: r.usages,
		},
	}
}

// reconcileOnce runs the approver on csr and reports whether it ended Approved.
func reconcileOnce(t *testing.T, w *world, csr *certv1.CertificateSigningRequest) (approved bool, rec *events.FakeRecorder, err error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloud.json")
	cfg := `{"cluster":"kcl_x","clusterUuid":"` + clusterUUID + `","org":"org_a","project":"prj_b","vpc":"vpc_c"}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(csr).WithStatusSubresource(csr).Build()
	rec = events.NewFakeRecorder(10)
	a := &Approver{Client: c, Clients: w.clients(t), ConfigPath: path, Recorder: rec, Log: slog.New(slog.DiscardHandler)}
	_, err = a.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
	var got certv1.CertificateSigningRequest
	if gerr := c.Get(context.Background(), client.ObjectKeyFromObject(csr), &got); gerr != nil {
		t.Fatal(gerr)
	}
	for _, cond := range got.Status.Conditions {
		if cond.Type == certv1.CertificateDenied {
			t.Fatal("the approver denied a CSR")
		}
		if cond.Type == certv1.CertificateApproved && cond.Reason == "EZGHAutoApproved" {
			approved = true
		}
	}
	return approved, rec, err
}

func pendingWith(t *testing.T, rec *events.FakeRecorder, want string) {
	t.Helper()
	select {
	case ev := <-rec.Events:
		if !strings.HasPrefix(ev, "Warning ") || !strings.Contains(ev, want) {
			t.Fatalf("event %q doesn't name %q", ev, want)
		}
	default:
		t.Fatal("no event")
	}
}

func TestClientCSRApprovedWhenNameMatches(t *testing.T) {
	approved, _, err := reconcileOnce(t, &world{status: "running"}, clientRequest().object(t))
	if err != nil || !approved {
		t.Fatal(approved, err)
	}
}

func TestClientCSRRefused(t *testing.T) {
	cases := map[string]func(*request){
		"another node's CN": func(r *request) { r.subject.CommonName = "system:node:ins-other0000000" },
		"an extra O":        func(r *request) { r.subject.Organization = append(r.subject.Organization, "system:masters") },
		"server auth usage": func(r *request) { r.usages = append(r.usages, certv1.UsageServerAuth) },
		"a requester that isn't a node": func(r *request) {
			r.username, r.groups = "alice", []string{"system:authenticated"}
		},
		"a node name that isn't an instance": func(r *request) {
			r.username, r.subject.CommonName = "system:node:worker-1", "system:node:worker-1"
		},
		"SANs":                        func(r *request) { r.dns, r.ips = []string{nodeName}, []string{"10.0.1.5"} },
		"a node without system:nodes": func(r *request) { r.groups = []string{"system:authenticated"} },
		"a request that isn't PEM":    func(r *request) { r.notPEM = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := clientRequest()
			mutate(&r)
			approved, rec, err := reconcileOnce(t, &world{status: "running"}, r.object(t))
			if err != nil || approved {
				t.Fatal(approved, err)
			}
			pendingWith(t, rec, "NotApproved")
		})
	}
}

// A client CSR reads the instance too: one that doesn't exist, is terminated or is another
// cluster's stays pending.
func TestClientCSRNeedsThisClustersLiveInstance(t *testing.T) {
	for name, w := range map[string]*world{
		"no such instance":       {},
		"terminated":             {status: "terminated"},
		"another cluster's node": {status: "running", foreignC: true},
	} {
		t.Run(name, func(t *testing.T) {
			approved, rec, err := reconcileOnce(t, w, clientRequest().object(t))
			if err != nil || approved {
				t.Fatal(approved, err)
			}
			pendingWith(t, rec, "NotApproved")
		})
	}
	approved, rec, err := reconcileOnce(t, &world{refused: true}, clientRequest().object(t))
	if err == nil || approved {
		t.Fatal("a refused read must requeue and stay pending:", approved, err)
	}
	pendingWith(t, rec, "MissingPermission")
}

func TestServingCSRAllIPsMustBeTheInstances(t *testing.T) {
	all := []string{"10.0.1.5", "fd65:7a67:6800:1::5", "203.0.113.7", "2001:db8:7::1"}
	if approved, _, err := reconcileOnce(t, &world{status: "running"}, servingRequest(all...).object(t)); err != nil || !approved {
		t.Fatal("all its addresses:", approved, err)
	}
	approved, rec, err := reconcileOnce(t, &world{status: "running"}, servingRequest("10.0.1.5", "10.0.1.6").object(t))
	if err != nil || approved {
		t.Fatal("a foreign IP:", approved, err)
	}
	pendingWith(t, rec, "10.0.1.6")

	// eni_moving0001 is in compute's list, but network has it attached to another instance.
	approved, rec, err = reconcileOnce(t, &world{status: "running"}, servingRequest("10.0.1.5", "10.0.1.9").object(t))
	if err != nil || approved {
		t.Fatal("an interface attached elsewhere:", approved, err)
	}
	pendingWith(t, rec, "10.0.1.9")

	approved, rec, err = reconcileOnce(t, &world{status: "terminated"}, servingRequest("10.0.1.5").object(t))
	if err != nil || approved {
		t.Fatal("terminated:", approved, err)
	}
	pendingWith(t, rec, "terminated")

	approved, rec, err = reconcileOnce(t, &world{status: "running", foreignC: true}, servingRequest("10.0.1.5").object(t))
	if err != nil || approved {
		t.Fatal("another cluster's instance:", approved, err)
	}
	pendingWith(t, rec, "cluster")

	approved, rec, err = reconcileOnce(t, &world{refused: true}, servingRequest("10.0.1.5").object(t))
	if err == nil || approved {
		t.Fatal("a refused read must requeue and stay pending:", approved, err)
	}
	pendingWith(t, rec, "MissingPermission")
}

func TestServingCSRRejectsExtraDNS(t *testing.T) {
	for name, mutate := range map[string]func(*request){
		"extra DNS": func(r *request) { r.dns = append(r.dns, "kubernetes.default") },
		"a URI":     func(r *request) { r.uris = []string{"spiffe://x/y"} },
		"no IP SAN": func(r *request) { r.ips = nil },
		"a CN that isn't the requester": func(r *request) {
			r.subject.CommonName = "system:node:ins-other0000000"
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := servingRequest("10.0.1.5")
			mutate(&r)
			approved, rec, err := reconcileOnce(t, &world{status: "running"}, r.object(t))
			if err != nil || approved {
				t.Fatal(approved, err)
			}
			pendingWith(t, rec, "NotApproved")
		})
	}
}

func TestAlreadyApprovedIsNoop(t *testing.T) {
	csr := clientRequest().object(t)
	csr.Status.Conditions = []certv1.CertificateSigningRequestCondition{{Type: certv1.CertificateApproved, Status: "True", Reason: "AutoApproved"}}
	// A refused world would make any read fail: a decided CSR reads nothing and records nothing.
	approved, rec, err := reconcileOnce(t, &world{refused: true}, csr)
	if err != nil || approved || len(rec.Events) != 0 {
		t.Fatal(approved, err, len(rec.Events))
	}
}

func TestOtherSignersIgnored(t *testing.T) {
	r := clientRequest()
	r.signer = certv1.KubeAPIServerClientSignerName
	approved, rec, err := reconcileOnce(t, &world{status: "running"}, r.object(t))
	if err != nil || approved || len(rec.Events) != 0 {
		t.Fatal(approved, err, len(rec.Events))
	}
}
