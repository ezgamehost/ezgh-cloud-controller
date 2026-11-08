package ccm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

const clusterUUID = "0199c7a2-1111-7000-8000-000000000001"

type staticKey struct{}

func (staticKey) APIKey(context.Context) (string, error) { return "kbt_test", nil }

// fake is compute and network as the cluster's Bot sees them.
type fake struct {
	instanceStatus int // the instance read's HTTP status; 0 is 200
	instance       map[string]any
	projectGone    bool // the project's instance list answers 404 too
}

func newInstance(status string) map[string]any {
	return map[string]any{
		"id": "0199c7a2-2222-7000-8000-000000000002", "slug": "ins_3k9d0a1m2x7q", "region": "us-west-1",
		"status": status, "instanceType": "gp-1.large",
		"tags": map[string]string{"ezgh:kubernetes-cluster": clusterUUID, "ezgh:kubernetes-node-group": "kng_a"},
		// network has eni_moving0001 attached elsewhere already: it lends the node no address.
		"networkInterfaceIds":       []string{"eni_public0001", "eni_private001", "eni_moving0001"},
		"primaryNetworkInterfaceId": "eni_public0001",
	}
}

func (f *fake) serve(t *testing.T) *ezghclient.Clients {
	t.Helper()
	write := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "60")
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	mux := http.NewServeMux()
	base := "/v1/organizations/org_a/projects/prj_b"
	mux.HandleFunc("GET "+base+"/instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch f.instanceStatus {
		case 0:
			if r.PathValue("id") != "ins_3k9d0a1m2x7q" {
				write(w, 404, map[string]any{"error": map[string]string{"code": "not_found", "message": "Not found"}})
				return
			}
			write(w, 200, f.instance)
		case 403:
			write(w, 403, map[string]any{"error": map[string]string{"code": "access_denied", "message": "This API key isn't allowed to compute.instances.get"}})
		default:
			write(w, f.instanceStatus, map[string]any{"error": map[string]string{"code": "error", "message": "nope"}})
		}
	})
	mux.HandleFunc("GET "+base+"/instances", func(w http.ResponseWriter, r *http.Request) {
		if f.projectGone {
			write(w, 404, map[string]any{"error": map[string]string{"code": "not_found", "message": "Not found"}})
			return
		}
		write(w, 200, map[string]any{"instances": []any{}, "nextCursor": nil})
	})
	mux.HandleFunc("GET "+base+"/network-interfaces/eni_moving0001", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{
			"id": "u4", "slug": "eni_moving0001", "vpc": "vpc_c", "privateIpv4Address": "10.0.1.9", "ipv4Address": nil,
			"attachment": map[string]any{"instance": "ins_0ther0000000", "deviceIndex": 2, "primary": false},
		})
	})
	mux.HandleFunc("GET "+base+"/network-interfaces/eni_public0001", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{
			"id": "u1", "slug": "eni_public0001", "vpc": nil, "privateIpv4Address": nil,
			"ipv4Address": map[string]string{"id": "u2", "slug": "pip_x", "address": "203.0.113.7"},
			"ipv6Address": "2001:db8:7::1",
			"attachment":  map[string]any{"instance": "ins_3k9d0a1m2x7q", "deviceIndex": 0, "primary": true},
		})
	})
	mux.HandleFunc("GET "+base+"/network-interfaces/eni_private001", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{
			"id": "u3", "slug": "eni_private001", "vpc": "vpc_c", "subnet": "sbn_d", "privateIpv4Address": "10.0.1.5",
			"ipv4Address": nil, "ipv6Address": "fd65:7a67:6800:1::5",
			"attachment": map[string]any{"instance": "ins_3k9d0a1m2x7q", "deviceIndex": 1, "primary": false},
		})
	})
	mux.HandleFunc("GET /v1/organizations/org_a/instance-types/gp-1.large", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"name": "gp-1.large", "limits": map[string]int{"networkInterfaces": 4, "volumes": 26}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return ezghclient.NewAt(staticKey{}, "test", func(string) string { return srv.URL })
}

func cloud(t *testing.T, f *fake) (*Cloud, *events.FakeRecorder) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloud.json")
	cfg := `{"cluster":"kcl_x","clusterUuid":"` + clusterUUID + `","org":"org_a","project":"prj_b","region":"us-west-1","vpc":"vpc_c","subnet":"sbn_d"}`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFakeRecorder(10)
	return &Cloud{Clients: f.serve(t), ConfigPath: path, Recorder: rec}, rec
}

func node(name, providerID string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{ProviderID: providerID}}
}

var theNode = node("ins-3k9d0a1m2x7q", "")

func TestInstanceMetadata(t *testing.T) {
	c, _ := cloud(t, &fake{instance: newInstance("running")})
	md, err := c.InstanceMetadata(context.Background(), theNode)
	if err != nil {
		t.Fatal(err)
	}
	want := []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.1.5"},
		{Type: corev1.NodeInternalIP, Address: "fd65:7a67:6800:1::5"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.7"},
		{Type: corev1.NodeExternalIP, Address: "2001:db8:7::1"},
		{Type: corev1.NodeHostName, Address: "ins-3k9d0a1m2x7q"},
	}
	if md.ProviderID != "ezgh://us-west-1/ins_3k9d0a1m2x7q" || md.InstanceType != "gp-1.large" || md.Region != "us-west-1" || md.Zone != "" {
		t.Fatalf("%+v", md)
	}
	if !reflect.DeepEqual(md.NodeAddresses, want) {
		t.Fatalf("addresses %+v", md.NodeAddresses)
	}
	if !reflect.DeepEqual(md.AdditionalLabels, map[string]string{"blockstore.csi.ezghcloud.com/max-volumes": "25"}) {
		t.Fatalf("labels %v", md.AdditionalLabels)
	}
}

func TestInstanceExists(t *testing.T) {
	for status, want := range map[string]bool{"terminated": false, "terminating": true, "running": true, "stopped": true, "failed": true} {
		c, _ := cloud(t, &fake{instance: newInstance(status)})
		got, err := c.InstanceExists(context.Background(), theNode)
		if err != nil || got != want {
			t.Errorf("%s: %v, %v", status, got, err)
		}
	}
	c, _ := cloud(t, &fake{instance: newInstance("running")})
	got, err := c.InstanceExists(context.Background(), node("ins-0000gone0000", "ezgh://us-west-1/ins_0000gone0000"))
	if err != nil || got {
		t.Fatalf("404: %v, %v", got, err)
	}
}

// A 404 while the project doesn't answer either is cloud.json naming a wrong or deleted project,
// not a gone instance: an error, so no Node is deleted.
func TestInstanceNotFoundInAMissingProjectIsAnError(t *testing.T) {
	c, _ := cloud(t, &fake{instance: newInstance("running"), projectGone: true})
	if got, err := c.InstanceExists(context.Background(), node("ins-0000gone0000", "")); err == nil {
		t.Fatalf("answered %v without an error", got)
	}
}

func TestInstanceShutdown(t *testing.T) {
	for status, want := range map[string]bool{"stopping": true, "stopped": true, "failed": true, "running": false, "terminating": false} {
		c, _ := cloud(t, &fake{instance: newInstance(status)})
		got, err := c.InstanceShutdown(context.Background(), theNode)
		if err != nil || got != want {
			t.Errorf("%s: %v, %v", status, got, err)
		}
	}
}

func TestInstanceInAnotherCluster(t *testing.T) {
	inst := newInstance("running")
	inst["tags"] = map[string]string{"ezgh:kubernetes-cluster": "0199c7a2-9999-7000-8000-000000000009"}
	c, _ := cloud(t, &fake{instance: inst})
	if _, err := c.InstanceMetadata(context.Background(), theNode); err == nil || !strings.Contains(err.Error(), "cluster") {
		t.Fatalf("metadata: %v", err)
	}
	if ok, err := c.InstanceExists(context.Background(), theNode); err == nil {
		t.Fatalf("exists: %v, no error", ok)
	}
}

// Review Focus 1: a refused read is never "the instance is gone", so the Node is never deleted.
func TestPermissionDeniedNeverDeletesNode(t *testing.T) {
	for _, status := range []int{403, 503} {
		c, rec := cloud(t, &fake{instanceStatus: status, instance: newInstance("running")})
		if ok, err := c.InstanceExists(context.Background(), theNode); err == nil {
			t.Fatalf("%d: exists answered %v without an error", status, ok)
		}
		if ok, err := c.InstanceShutdown(context.Background(), theNode); err == nil {
			t.Fatalf("%d: shutdown answered %v without an error", status, ok)
		}
		if status == 403 {
			if ev := <-rec.Events; !strings.HasPrefix(ev, "Warning MissingPermission ") || !strings.Contains(ev, "compute.instances.get") {
				t.Fatal(ev)
			}
		}
	}
}

func TestNodeNameMapping(t *testing.T) {
	for _, n := range []*corev1.Node{node("ins-3k9d0a1m2x7q", ""), node("anything", "ezgh://us-west-1/ins_3k9d0a1m2x7q")} {
		if id, err := InstanceID(n); err != nil || id != "ins_3k9d0a1m2x7q" {
			t.Fatalf("%s: %q, %v", n.Name, id, err)
		}
	}
	for _, n := range []*corev1.Node{node("worker-1", ""), node("ins-", ""), node("ins-ABC", ""), node("ins-3k9d0a1m2x7q", "aws:///us-west-1a/i-1"), node("x", "ezgh://us-west-1/vol_1")} {
		if id, err := InstanceID(n); err == nil {
			t.Fatalf("%s %s: %q, no error", n.Name, n.Spec.ProviderID, id)
		}
	}
}
