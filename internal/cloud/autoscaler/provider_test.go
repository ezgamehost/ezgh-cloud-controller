package autoscaler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

type staticKey struct{}

func (staticKey) APIKey(context.Context) (string, error) { return "kbt_test", nil }

// fake is compute as the cluster's Bot sees it: one autoscaling group asg_a.
type fake struct {
	mu      sync.Mutex
	desired int
	etag    int
	members []map[string]string // instanceId, state
	stale   int                 // answer this many PATCHes 412 (another writer got there first)
	denied  bool                // every write is 403
	rootGiB any
	writes  []string // "PATCH {body} If-Match" and "POST <path> {body}"
}

func (f *fake) serve(t *testing.T) *ezghclient.Clients {
	t.Helper()
	base := "/v1/organizations/org_a/projects/prj_b/autoscaling-groups/asg_a"
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
	denied := func(w http.ResponseWriter) bool {
		if f.denied {
			write(w, 403, map[string]any{"error": map[string]string{"code": "access_denied", "message": "This API key isn't allowed to compute.autoscalingGroups.update"}})
		}
		return f.denied
	}
	mux.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("ETag", etag(f.etag))
		write(w, 200, map[string]any{"id": "asg_a", "minSize": 1, "maxSize": 5, "desiredCapacity": f.desired,
			"launchSpec": map[string]any{"instanceType": "gp-1.large", "rootVolume": map[string]any{"sizeGiB": f.rootGiB}}})
	})
	mux.HandleFunc("PATCH "+base, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if denied(w) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.writes = append(f.writes, "PATCH "+string(body)+" "+r.Header.Get("If-Match"))
		if f.stale > 0 || r.Header.Get("If-Match") != etag(f.etag) {
			f.stale--
			f.etag++ // the other writer's change
			write(w, 412, map[string]any{"error": map[string]string{"code": "precondition_failed", "message": "stale"}})
			return
		}
		var b struct{ DesiredCapacity int }
		_ = json.Unmarshal(body, &b)
		f.desired, f.etag = b.DesiredCapacity, f.etag+1
		write(w, 200, map[string]any{"id": "asg_a", "desiredCapacity": f.desired})
	})
	mux.HandleFunc("GET "+base+"/members", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		write(w, 200, map[string]any{"members": f.members, "nextCursor": nil})
	})
	mux.HandleFunc("POST "+base+"/members/{ins}/terminate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if denied(w) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.writes = append(f.writes, "POST members/"+r.PathValue("ins")+"/terminate "+string(body))
		write(w, 200, map[string]any{"instanceId": r.PathValue("ins"), "state": "retiring"})
	})
	mux.HandleFunc("GET /v1/organizations/org_a/instance-types/gp-1.large", func(w http.ResponseWriter, r *http.Request) {
		write(w, 200, map[string]any{"name": "gp-1.large", "architecture": "amd64", "vcpus": 2, "memoryMiB": 8192})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return ezghclient.NewAt(staticKey{}, "test", func(string) string { return srv.URL })
}

func etag(n int) string { return `"e` + string(rune('0'+n)) + `"` }

const cloudJSON = `{"cluster":"kcl_a","clusterUuid":"0199c7a2-1111-7000-8000-000000000001","org":"org_a","project":"prj_b",
 "nodeGroups":[{"id":"kng_a","autoscalingGroup":"asg_a","instanceType":"gp-1.large","minSize":1,"maxSize":5,
  "labels":{"team":"web"},"taints":[{"key":"dedicated","value":"web","effect":"NoSchedule"}]}]}`

func newProvider(t *testing.T, f *fake) *Provider {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cloud.json")
	if err := os.WriteFile(path, []byte(cloudJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return &Provider{Clients: f.serve(t), ConfigPath: path}
}

func node(ins string) *ExternalGrpcNode {
	return &ExternalGrpcNode{ProviderID: "ezgh://us-west-1/" + ins, Name: strings.ReplaceAll(ins, "_", "-")}
}

var ctx = context.Background()

func TestNodeGroupsFromConfig(t *testing.T) {
	p := newProvider(t, &fake{})
	out, err := p.NodeGroups(ctx, &NodeGroupsRequest{})
	if err != nil || len(out.NodeGroups) != 1 || out.NodeGroups[0].Id != "kng_a" || out.NodeGroups[0].MinSize != 1 || out.NodeGroups[0].MaxSize != 5 {
		t.Fatal(out, err)
	}
}

func TestIncreaseSizeIfMatch(t *testing.T) {
	f := &fake{desired: 2}
	p := newProvider(t, f)
	if _, err := p.NodeGroupIncreaseSize(ctx, &NodeGroupIncreaseSizeRequest{Id: "kng_a", Delta: 2}); err != nil {
		t.Fatal(err)
	}
	if f.desired != 4 || len(f.writes) != 1 || f.writes[0] != `PATCH {"desiredCapacity":4} "e0"` {
		t.Fatal(f.desired, f.writes)
	}
	size, err := p.NodeGroupTargetSize(ctx, &NodeGroupTargetSizeRequest{Id: "kng_a"})
	if err != nil || size.TargetSize != 4 {
		t.Fatal(size, err)
	}
}

func TestIncreaseRereadsOnceOn412(t *testing.T) {
	f := &fake{desired: 2, stale: 1}
	p := newProvider(t, f)
	if _, err := p.NodeGroupIncreaseSize(ctx, &NodeGroupIncreaseSizeRequest{Id: "kng_a", Delta: 1}); err != nil {
		t.Fatal(err)
	}
	if f.desired != 3 || len(f.writes) != 2 || f.writes[1] != `PATCH {"desiredCapacity":3} "e1"` {
		t.Fatal(f.desired, f.writes)
	}
	// Two stale answers in a row: the second goes back to Cluster Autoscaler, which retries.
	f.stale = 2
	if _, err := p.NodeGroupIncreaseSize(ctx, &NodeGroupIncreaseSizeRequest{Id: "kng_a", Delta: 1}); err == nil {
		t.Fatal("want an error after the one reread")
	}
}

func TestIncreasePastMaxRefused(t *testing.T) {
	f := &fake{desired: 4}
	p := newProvider(t, f)
	_, err := p.NodeGroupIncreaseSize(ctx, &NodeGroupIncreaseSizeRequest{Id: "kng_a", Delta: 2})
	if status.Code(err) != codes.InvalidArgument || len(f.writes) != 0 {
		t.Fatal(err, f.writes)
	}
}

func TestDeleteNodesTerminatesChosen(t *testing.T) {
	f := &fake{desired: 3, members: []map[string]string{{"instanceId": "ins_a", "state": "inService"}, {"instanceId": "ins_b", "state": "inService"}, {"instanceId": "ins_c", "state": "launching"}}}
	p := newProvider(t, f)
	if _, err := p.NodeGroupDeleteNodes(ctx, &NodeGroupDeleteNodesRequest{Id: "kng_a", Nodes: []*ExternalGrpcNode{node("ins_a")}}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 || f.writes[0] != `POST members/ins_a/terminate {"decrementDesired":true}` {
		t.Fatal(f.writes)
	}
}

func TestDeleteNodeOfOtherGroupRefused(t *testing.T) {
	f := &fake{desired: 3, members: []map[string]string{{"instanceId": "ins_a", "state": "inService"}, {"instanceId": "ins_b", "state": "inService"}}}
	p := newProvider(t, f)
	_, err := p.NodeGroupDeleteNodes(ctx, &NodeGroupDeleteNodesRequest{Id: "kng_a", Nodes: []*ExternalGrpcNode{node("ins_a"), node("ins_z")}})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "ins_z") || len(f.writes) != 0 {
		t.Fatal(err, f.writes)
	}
}

func TestDeleteBelowMinRefused(t *testing.T) {
	f := &fake{desired: 1, members: []map[string]string{{"instanceId": "ins_a", "state": "inService"}}}
	p := newProvider(t, f)
	_, err := p.NodeGroupDeleteNodes(ctx, &NodeGroupDeleteNodesRequest{Id: "kng_a", Nodes: []*ExternalGrpcNode{node("ins_a")}})
	if status.Code(err) != codes.FailedPrecondition || len(f.writes) != 0 {
		t.Fatal(err, f.writes)
	}
}

func TestDecreaseNeverBelowMembers(t *testing.T) {
	// desired 4, three members (one already retiring, so it no longer counts): lowering by 2 leaves 2.
	f := &fake{desired: 4, members: []map[string]string{{"instanceId": "ins_a", "state": "inService"}, {"instanceId": "ins_b", "state": "launching"}, {"instanceId": "ins_c", "state": "retiring"}}}
	p := newProvider(t, f)
	if _, err := p.NodeGroupDecreaseTargetSize(ctx, &NodeGroupDecreaseTargetSizeRequest{Id: "kng_a", Delta: -2}); err != nil {
		t.Fatal(err)
	}
	if f.desired != 2 {
		t.Fatal(f.desired)
	}
	_, err := p.NodeGroupDecreaseTargetSize(ctx, &NodeGroupDecreaseTargetSizeRequest{Id: "kng_a", Delta: -1})
	if status.Code(err) != codes.FailedPrecondition || f.desired != 2 {
		t.Fatal(err, f.desired)
	}
	if _, err := p.NodeGroupDecreaseTargetSize(ctx, &NodeGroupDecreaseTargetSizeRequest{Id: "kng_a", Delta: 1}); status.Code(err) != codes.InvalidArgument {
		t.Fatal("a positive delta is refused:", err)
	}
}

func TestNodeGroupNodes(t *testing.T) {
	f := &fake{members: []map[string]string{{"instanceId": "ins_a", "state": "inService"}, {"instanceId": "ins_b", "state": "launching"}, {"instanceId": "ins_c", "state": "retiring"}}}
	p := newProvider(t, f)
	out, err := p.NodeGroupNodes(ctx, &NodeGroupNodesRequest{Id: "kng_a"})
	if err != nil || len(out.Instances) != 3 {
		t.Fatal(out, err)
	}
	want := map[string]InstanceStatus_InstanceState{
		"ezgh://us-west-1/ins_a": InstanceStatus_instanceRunning, "ezgh://us-west-1/ins_b": InstanceStatus_instanceCreating,
		"ezgh://us-west-1/ins_c": InstanceStatus_instanceDeleting,
	}
	for _, i := range out.Instances {
		if want[i.Id] != i.Status.InstanceState {
			t.Fatal(i)
		}
	}
}

func TestTemplateScaleFromZero(t *testing.T) {
	p := newProvider(t, &fake{rootGiB: 50})
	out, err := p.NodeGroupTemplateNodeInfo(ctx, &NodeGroupTemplateNodeInfoRequest{Id: "kng_a"})
	if err != nil {
		t.Fatal(err)
	}
	var n corev1.Node
	if err := n.Unmarshal(out.NodeBytes); err != nil {
		t.Fatal(err)
	}
	c, a := n.Status.Capacity, n.Status.Allocatable
	if c.Cpu().MilliValue() != 2000 || c.Memory().Value() != 8192<<20 || c.Pods().Value() != 110 || c.StorageEphemeral().Value() != 50<<30 {
		t.Fatalf("capacity %v", c)
	}
	// 2 vCPUs, 110 pods: 70m and 1465Mi reserved, and kubelet's 100Mi eviction threshold.
	if a.Cpu().MilliValue() != 1930 || a.Memory().Value() != (8192-1465-100)<<20 || a.Pods().Value() != 110 {
		t.Fatalf("allocatable %v", a)
	}
	l := n.Labels
	if l["team"] != "web" || l["kubernetes.ezghcloud.com/node-group"] != "kng_a" || l["node.kubernetes.io/instance-type"] != "gp-1.large" ||
		l["topology.kubernetes.io/region"] != "us-west-1" || l["kubernetes.io/os"] != "linux" || l["kubernetes.io/arch"] != "amd64" {
		t.Fatalf("labels %v", l)
	}
	if len(n.Spec.Taints) != 1 || n.Spec.Taints[0].Key != "dedicated" || n.Spec.Taints[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("taints %v", n.Spec.Taints)
	}
}

func TestNodeGroupForUnknownNode(t *testing.T) {
	p := newProvider(t, &fake{})
	n := node("ins_a")
	n.Labels = map[string]string{"kubernetes.ezghcloud.com/node-group": "kng_gone"}
	out, err := p.NodeGroupForNode(ctx, &NodeGroupForNodeRequest{Node: n})
	if err != nil || out.NodeGroup == nil || out.NodeGroup.Id != "" {
		t.Fatal(out, err)
	}
	n.Labels["kubernetes.ezghcloud.com/node-group"] = "kng_a"
	if out, err := p.NodeGroupForNode(ctx, &NodeGroupForNodeRequest{Node: n}); err != nil || out.NodeGroup.Id != "kng_a" {
		t.Fatal(out, err)
	}
	if _, err := p.NodeGroupTargetSize(ctx, &NodeGroupTargetSizeRequest{Id: "kng_gone"}); status.Code(err) != codes.NotFound {
		t.Fatal(err)
	}
}

// Review Focus 1: a narrowed cluster Bot is a PermissionDenied naming the action, which Cluster
// Autoscaler logs and retries; nothing is written.
func TestDeniedIsGRPCPermissionDenied(t *testing.T) {
	f := &fake{desired: 2, denied: true, members: []map[string]string{{"instanceId": "ins_a", "state": "inService"}, {"instanceId": "ins_b", "state": "inService"}}}
	p := newProvider(t, f)
	for _, call := range []func() error{
		func() error {
			_, err := p.NodeGroupIncreaseSize(ctx, &NodeGroupIncreaseSizeRequest{Id: "kng_a", Delta: 1})
			return err
		},
		func() error {
			_, err := p.NodeGroupDeleteNodes(ctx, &NodeGroupDeleteNodesRequest{Id: "kng_a", Nodes: []*ExternalGrpcNode{node("ins_a")}})
			return err
		},
	} {
		err := call()
		if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "compute.autoscalingGroups.update") {
			t.Fatal(err)
		}
	}
	if f.desired != 2 {
		t.Fatal(f.desired)
	}
}
