// Package autoscaler is Cluster Autoscaler's externalgrpc cloud provider, served by ezgh-cloud
// autoscaler-provider on 127.0.0.1:9447. Node groups map to their autoscaling groups: scale-out
// raises desiredCapacity, and scale-in terminates exactly the members CA picked, with decrement,
// draining through the group's kubernetesOwner. It keeps nothing: cloud.json and compute are read
// on each call.
//
// externalgrpc.proto is Cluster Autoscaler's, unedited, from tag cluster-autoscaler-1.36.1 (no
// 1.37.x tag was two weeks old; 1.37.0's only adds optional fields this provider doesn't set, so
// the wire is the same for both minors). Regenerate with protoc 36.2, protoc-gen-go v1.36.12 and
// protoc-gen-go-grpc v1.6.2, from this directory:
//
//	protoc -I . --go_out=. --go_opt=paths=source_relative \
//	  '--go_opt=Mexternalgrpc.proto=github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/autoscaler;autoscaler' \
//	  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
//	  '--go-grpc_opt=Mexternalgrpc.proto=github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/autoscaler;autoscaler' \
//	  externalgrpc.proto
package autoscaler

import (
	"context"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/ccm"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/computeapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/nodeauth"
)

// NodeGroupLabel is the label kubelet registers each node with (the node join facts).
const NodeGroupLabel = "kubernetes.ezghcloud.com/node-group"

// maxPods is every node's pod limit (the node join facts).
const maxPods = 110

// Provider is the CloudProvider service. Pricing and NodeGroupGetOptions stay Unimplemented, which
// Cluster Autoscaler treats as "use the defaults".
type Provider struct {
	UnimplementedCloudProviderServer
	Clients    *ezghclient.Clients
	ConfigPath string
}

func (p *Provider) NodeGroups(context.Context, *NodeGroupsRequest) (*NodeGroupsResponse, error) {
	cfg, err := config.Load(p.ConfigPath)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	out := &NodeGroupsResponse{}
	for _, g := range cfg.NodeGroups {
		out.NodeGroups = append(out.NodeGroups, nodeGroup(g))
	}
	return out, nil
}

// NodeGroupForNode is the node's group by its node-group label; an unknown one is id "", which
// Cluster Autoscaler leaves alone.
func (p *Provider) NodeGroupForNode(_ context.Context, req *NodeGroupForNodeRequest) (*NodeGroupForNodeResponse, error) {
	cfg, err := config.Load(p.ConfigPath)
	if err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if g, ok := cfg.NodeGroup(req.GetNode().GetLabels()[NodeGroupLabel]); ok {
		return &NodeGroupForNodeResponse{NodeGroup: nodeGroup(g)}, nil
	}
	return &NodeGroupForNodeResponse{NodeGroup: &NodeGroup{}}, nil
}

func (p *Provider) NodeGroupTargetSize(ctx context.Context, req *NodeGroupTargetSizeRequest) (*NodeGroupTargetSizeResponse, error) {
	cfg, ng, err := p.nodeGroup(req.Id)
	if err != nil {
		return nil, err
	}
	asg, _, err := p.Clients.Compute.GetAutoscalingGroup(ctx, cfg.Org, cfg.Project, ng.AutoscalingGroup)
	if err != nil {
		return nil, apiStatus(err)
	}
	return &NodeGroupTargetSizeResponse{TargetSize: int32(asg.DesiredCapacity)}, nil
}

func (p *Provider) NodeGroupIncreaseSize(ctx context.Context, req *NodeGroupIncreaseSizeRequest) (*NodeGroupIncreaseSizeResponse, error) {
	if req.Delta <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta %d isn't positive", req.Delta)
	}
	err := p.resize(ctx, req.Id, func(_ config.Config, ng config.NodeGroup, asg *computeapi.AutoscalingGroup) (int, error) {
		n := asg.DesiredCapacity + int(req.Delta)
		if n > ng.MaxSize {
			return 0, status.Errorf(codes.InvalidArgument, "node group %s would grow to %d, past its maxSize %d", ng.ID, n, ng.MaxSize)
		}
		return n, nil
	})
	return &NodeGroupIncreaseSizeResponse{}, err
}

// NodeGroupDecreaseTargetSize drops capacity that hasn't launched yet: it never goes below the
// members that are launching or in service (deleting those is NodeGroupDeleteNodes).
func (p *Provider) NodeGroupDecreaseTargetSize(ctx context.Context, req *NodeGroupDecreaseTargetSizeRequest) (*NodeGroupDecreaseTargetSizeResponse, error) {
	if req.Delta >= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "delta %d isn't negative", req.Delta)
	}
	err := p.resize(ctx, req.Id, func(cfg config.Config, ng config.NodeGroup, asg *computeapi.AutoscalingGroup) (int, error) {
		members, err := p.members(ctx, cfg, ng)
		if err != nil {
			return 0, err
		}
		live := 0
		for _, m := range members {
			if m.State != "retiring" {
				live++
			}
		}
		n := asg.DesiredCapacity + int(req.Delta)
		if n < live {
			return 0, status.Errorf(codes.FailedPrecondition, "node group %s has %d members; desired %d can't drop to %d", ng.ID, live, asg.DesiredCapacity, n)
		}
		return n, nil
	})
	return &NodeGroupDecreaseTargetSizeResponse{}, err
}

// NodeGroupDeleteNodes terminates exactly the nodes asked for, each with decrementDesired, once
// every one is checked to be a member of the group and the group stays at or above minSize.
func (p *Provider) NodeGroupDeleteNodes(ctx context.Context, req *NodeGroupDeleteNodesRequest) (*NodeGroupDeleteNodesResponse, error) {
	cfg, ng, err := p.nodeGroup(req.Id)
	if err != nil {
		return nil, err
	}
	asg, _, err := p.Clients.Compute.GetAutoscalingGroup(ctx, cfg.Org, cfg.Project, ng.AutoscalingGroup)
	if err != nil {
		return nil, apiStatus(err)
	}
	members, err := p.members(ctx, cfg, ng)
	if err != nil {
		return nil, err
	}
	state := map[string]string{}
	for _, m := range members {
		state[m.InstanceID] = m.State
	}
	var ids []string
	for _, n := range req.Nodes {
		id, err := ccm.InstanceID(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: n.Name}, Spec: corev1.NodeSpec{ProviderID: n.ProviderID}})
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		switch state[id] {
		case "":
			return nil, status.Errorf(codes.InvalidArgument, "node %s (%s) isn't a member of node group %s", n.Name, id, ng.ID)
		case "retiring": // already on its way out, its desired already decremented
		default:
			ids = append(ids, id)
		}
	}
	if left := asg.DesiredCapacity - len(ids); left < ng.MinSize {
		return nil, status.Errorf(codes.FailedPrecondition, "deleting %d nodes would leave node group %s at %d, under its minSize %d", len(ids), ng.ID, left, ng.MinSize)
	}
	for _, id := range ids {
		if _, err := p.Clients.Compute.TerminateMember(ctx, cfg.Org, cfg.Project, ng.AutoscalingGroup, id, true); err != nil {
			return nil, apiStatus(err)
		}
	}
	return &NodeGroupDeleteNodesResponse{}, nil
}

func (p *Provider) NodeGroupNodes(ctx context.Context, req *NodeGroupNodesRequest) (*NodeGroupNodesResponse, error) {
	cfg, ng, err := p.nodeGroup(req.Id)
	if err != nil {
		return nil, err
	}
	members, err := p.members(ctx, cfg, ng)
	if err != nil {
		return nil, err
	}
	out := &NodeGroupNodesResponse{}
	for _, m := range members {
		s := InstanceStatus_instanceRunning
		switch m.State {
		case "launching":
			s = InstanceStatus_instanceCreating
		case "retiring":
			s = InstanceStatus_instanceDeleting
		}
		out.Instances = append(out.Instances, &Instance{Id: "ezgh://" + cfg.Region + "/" + m.InstanceID, Status: &InstanceStatus{InstanceState: s}})
	}
	return out, nil
}

// NodeGroupTemplateNodeInfo is the Node a new member would register as, so Cluster Autoscaler can
// scale a group up from zero.
func (p *Provider) NodeGroupTemplateNodeInfo(ctx context.Context, req *NodeGroupTemplateNodeInfoRequest) (*NodeGroupTemplateNodeInfoResponse, error) {
	cfg, ng, err := p.nodeGroup(req.Id)
	if err != nil {
		return nil, err
	}
	it, err := p.Clients.Compute.GetInstanceType(ctx, cfg.Org, ng.InstanceType)
	if err != nil {
		return nil, apiStatus(err)
	}
	asg, _, err := p.Clients.Compute.GetAutoscalingGroup(ctx, cfg.Org, cfg.Project, ng.AutoscalingGroup)
	if err != nil {
		return nil, apiStatus(err)
	}
	cpuMilli, memMi := nodeauth.Reserved(it.VCPUs, maxPods)
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(it.VCPUs), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(it.MemoryMiB)<<20, resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(maxPods, resource.DecimalSI),
	}
	allocatable := corev1.ResourceList{
		corev1.ResourceCPU: *resource.NewMilliQuantity(int64(max(it.VCPUs*1000-cpuMilli, 0)), resource.DecimalSI),
		// kubelet's default eviction threshold, memory.available<100Mi, comes off too.
		corev1.ResourceMemory: *resource.NewQuantity(int64(max(it.MemoryMiB-memMi-100, 0))<<20, resource.BinarySI),
		corev1.ResourcePods:   *resource.NewQuantity(maxPods, resource.DecimalSI),
	}
	if size := asg.LaunchSpec.RootVolume.SizeGiB; size != nil {
		// kubelet's default nodefs.available<10% eviction threshold comes off allocatable.
		capacity[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(int64(*size)<<30, resource.BinarySI)
		allocatable[corev1.ResourceEphemeralStorage] = *resource.NewQuantity(int64(*size)<<30/10*9, resource.BinarySI)
	}
	name := "template-" + strings.ReplaceAll(ng.ID, "_", "-")
	labels := map[string]string{}
	for k, v := range ng.Labels {
		labels[k] = v
	}
	for k, v := range map[string]string{
		NodeGroupLabel: ng.ID, corev1.LabelInstanceTypeStable: ng.InstanceType, corev1.LabelTopologyRegion: cfg.Region,
		corev1.LabelOSStable: "linux", corev1.LabelArchStable: it.Architecture, corev1.LabelHostname: name,
	} {
		labels[k] = v
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{Taints: ng.Taints},
		Status: corev1.NodeStatus{
			Capacity: capacity, Allocatable: allocatable, Phase: corev1.NodeRunning,
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	b, err := node.Marshal()
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &NodeGroupTemplateNodeInfoResponse{NodeBytes: b}, nil
}

func (p *Provider) GPULabel(context.Context, *GPULabelRequest) (*GPULabelResponse, error) {
	return &GPULabelResponse{}, nil
}

func (p *Provider) GetAvailableGPUTypes(context.Context, *GetAvailableGPUTypesRequest) (*GetAvailableGPUTypesResponse, error) {
	return &GetAvailableGPUTypesResponse{GpuTypes: map[string]*anypb.Any{}}, nil
}

func (p *Provider) Refresh(context.Context, *RefreshRequest) (*RefreshResponse, error) {
	return &RefreshResponse{}, nil
}

func (p *Provider) Cleanup(context.Context, *CleanupRequest) (*CleanupResponse, error) {
	return &CleanupResponse{}, nil
}

func nodeGroup(g config.NodeGroup) *NodeGroup {
	return &NodeGroup{Id: g.ID, MinSize: int32(g.MinSize), MaxSize: int32(g.MaxSize)}
}

func (p *Provider) nodeGroup(id string) (config.Config, config.NodeGroup, error) {
	cfg, err := config.Load(p.ConfigPath)
	if err != nil {
		return cfg, config.NodeGroup{}, status.Error(codes.Unavailable, err.Error())
	}
	ng, ok := cfg.NodeGroup(id)
	if !ok {
		return cfg, ng, status.Errorf(codes.NotFound, "no node group %q in this cluster", id)
	}
	return cfg, ng, nil
}

func (p *Provider) members(ctx context.Context, cfg config.Config, ng config.NodeGroup) ([]computeapi.AutoscalingMember, error) {
	var all []computeapi.AutoscalingMember
	_, err := apiclient.Paginate(ctx, "", true, func(ctx context.Context, cursor string) (apiclient.Page[computeapi.AutoscalingMember], error) {
		return p.Clients.Compute.ListAutoscalingMembers(ctx, cfg.Org, cfg.Project, ng.AutoscalingGroup, 100, cursor)
	}, func(page []computeapi.AutoscalingMember) error { all = append(all, page...); return nil })
	return all, apiStatus(err)
}

// resize writes the desiredCapacity next picks, conditional on the ETag read with the group it was
// picked from. A 412 (another writer between the read and the write) rereads and picks once more.
//
// Compute declares Idempotency-Key on this PATCH, and the copied client sends one key for every
// attempt of a call, so its own retries and the transport's resends are replayed, not applied
// twice; If-Match refuses them too. What no key can cover: an answer lost after compute applied
// the write fails the call, Cluster Autoscaler asks again, and the fresh read already shows the
// new size, so the delta is added a second time. That's bounded by maxSize, and Cluster Autoscaler
// scales the surplus back in. (ezghclient.WithIdempotencyKey only rewrites POSTs, and a key from
// the observed size would differ on that second read anyway.)
func (p *Provider) resize(ctx context.Context, id string, next func(config.Config, config.NodeGroup, *computeapi.AutoscalingGroup) (int, error)) error {
	cfg, ng, err := p.nodeGroup(id)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		asg, etag, err := p.Clients.Compute.GetAutoscalingGroup(ctx, cfg.Org, cfg.Project, ng.AutoscalingGroup)
		if err != nil {
			return apiStatus(err)
		}
		n, err := next(cfg, ng, asg)
		if err != nil {
			return err
		}
		_, err = p.Clients.Compute.UpdateAutoscalingGroup(ctx, cfg.Org, cfg.Project, ng.AutoscalingGroup, computeapi.AutoscalingGroupUpdate{DesiredCapacity: &n}, etag)
		if e, ok := apiclient.AsError(err); ok && e.Status == http.StatusPreconditionFailed && attempt == 0 {
			continue
		}
		return apiStatus(err)
	}
}

// apiStatus is an EZGH API error as a gRPC status, which Cluster Autoscaler logs and retries. A
// permission refusal names the IAM action the cluster's Bot is missing (Review Focus 1).
func apiStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	_, msg := events.Describe(err)
	switch {
	case isPermission(err):
		return status.Error(codes.PermissionDenied, msg)
	case isQuota(err):
		return status.Error(codes.ResourceExhausted, msg)
	case ezghclient.IsNotFound(err):
		return status.Error(codes.NotFound, msg)
	}
	if e, ok := apiclient.AsError(err); ok && (e.Status == http.StatusConflict || e.Status == http.StatusBadRequest) {
		return status.Error(codes.FailedPrecondition, msg) // below_min_size, a size compute refuses
	}
	return status.Error(codes.Unavailable, msg)
}

func isPermission(err error) bool { _, ok := ezghclient.AsPermission(err); return ok }
func isQuota(err error) bool      { _, _, ok := ezghclient.AsQuota(err); return ok }
