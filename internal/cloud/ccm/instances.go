package ccm

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/computeapi"
)

// MaxVolumesLabel is how many Block Storage volumes the node's CSI plugin may attach: the
// instance type's limit less the root volume.
const MaxVolumesLabel = "blockstore.csi.ezghcloud.com/max-volumes"

const clusterTag = "ezgh:kubernetes-cluster"

// ErrNotThisCluster is an instance whose ezgh:kubernetes-cluster tag isn't this cluster's UUID.
var ErrNotThisCluster = errors.New("not a node of this cluster")

// InstanceID is node's instance (ins_…): from its providerID ezgh://<region>/ins_…, else from its
// name ins-…. Anything else is an error, never a guess.
func InstanceID(node *corev1.Node) (string, error) {
	if p := node.Spec.ProviderID; p != "" {
		rest, ok := strings.CutPrefix(p, "ezgh://")
		_, id, ok2 := strings.Cut(rest, "/")
		suffix, ok3 := strings.CutPrefix(id, "ins_")
		if !ok || !ok2 || !ok3 || !validSuffix(suffix) {
			return "", fmt.Errorf("node %s: providerID %q isn't ezgh://<region>/ins_…", node.Name, p)
		}
		return id, nil
	}
	suffix, ok := strings.CutPrefix(node.Name, "ins-")
	if !ok || !validSuffix(suffix) {
		return "", fmt.Errorf("node %s isn't named after an EZGH instance (ins-…)", node.Name)
	}
	return "ins_" + suffix, nil
}

func validSuffix(s string) bool {
	return s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789") == ""
}

// GetInstance reads instance id live, and refuses one that isn't this cluster's node.
func GetInstance(ctx context.Context, c *ezghclient.Clients, cfg config.Config, id string) (*computeapi.Instance, error) {
	inst, _, err := c.Compute.GetInstance(ctx, cfg.Org, cfg.Project, id)
	if err != nil {
		return nil, err
	}
	if inst.Tags[clusterTag] != cfg.ClusterUUID {
		return nil, fmt.Errorf("instance %s is %w: its %s tag is %q", id, ErrNotThisCluster, clusterTag, inst.Tags[clusterTag])
	}
	return inst, nil
}

// Addresses are inst's IP addresses, each interface read from network and used only while network
// also says it's attached to inst (a detach or move in flight lends no address): the private
// interface in the cluster's VPC gives InternalIP (its IPv4, then its IPv6), the primary gives
// ExternalIP (its public IPv4, then its IPv6).
func Addresses(ctx context.Context, c *ezghclient.Clients, cfg config.Config, inst *computeapi.Instance) ([]corev1.NodeAddress, error) {
	var internal, external []corev1.NodeAddress
	add := func(list []corev1.NodeAddress, t corev1.NodeAddressType, addrs ...*string) []corev1.NodeAddress {
		for _, a := range addrs {
			if a != nil && *a != "" {
				list = append(list, corev1.NodeAddress{Type: t, Address: *a})
			}
		}
		return list
	}
	for _, eni := range inst.ENIs {
		ni, err := c.Network.GetNetworkInterface(ctx, cfg.Org, cfg.Project, eni)
		if err != nil {
			return nil, err
		}
		if ni.Attachment == nil || (ni.Attachment.Instance != inst.Slug && ni.Attachment.Instance != inst.ID) {
			continue
		}
		switch {
		case ni.VPC != nil && *ni.VPC == cfg.VPC:
			internal = add(internal, corev1.NodeInternalIP, ni.PrivateIPv4Address, ni.IPv6Address)
		case ni.Attachment != nil && ni.Attachment.Primary:
			var v4 *string
			if ni.IPv4Address != nil {
				v4 = &ni.IPv4Address.Address
			}
			external = add(external, corev1.NodeExternalIP, v4, ni.IPv6Address)
		}
	}
	return append(internal, external...), nil
}

// InstanceExists is false only when compute says the instance is gone (404, or terminated). A
// refused or failed read is an error, so the Node is never deleted because of it; so is a 404 while
// the project itself doesn't answer (a wrong or deleted project in cloud.json would otherwise
// delete every Node).
func (c *Cloud) InstanceExists(ctx context.Context, node *corev1.Node) (bool, error) {
	inst, cfg, err := c.instance(ctx, node)
	if ezghclient.IsNotFound(err) {
		if _, perr := c.Clients.Compute.ListInstances(ctx, cfg.Org, cfg.Project, computeapi.InstanceFilter{}, 1, ""); perr != nil {
			return false, c.report(node, fmt.Errorf("instance not found, and project %s doesn't answer either: %w", cfg.Project, perr))
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return inst.Status != "terminated", nil
}

// InstanceShutdown is true while the instance is stopping, stopped or failed (a failed one is
// down, but only terminated or a 404 removes the Node).
func (c *Cloud) InstanceShutdown(ctx context.Context, node *corev1.Node) (bool, error) {
	inst, _, err := c.instance(ctx, node)
	if ezghclient.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return inst.Status == "stopping" || inst.Status == "stopped" || inst.Status == "failed", nil
}

// InstanceMetadata is what the cloud-node controller initializes node with.
func (c *Cloud) InstanceMetadata(ctx context.Context, node *corev1.Node) (*cloudprovider.InstanceMetadata, error) {
	inst, cfg, err := c.instance(ctx, node)
	if err != nil {
		return nil, err
	}
	addrs, err := Addresses(ctx, c.Clients, cfg, inst)
	if err != nil {
		return nil, c.report(node, err)
	}
	it, err := c.Clients.Compute.GetInstanceType(ctx, cfg.Org, inst.InstanceType)
	if err != nil {
		return nil, c.report(node, err)
	}
	return &cloudprovider.InstanceMetadata{
		ProviderID:       "ezgh://" + inst.Region + "/" + inst.Slug,
		InstanceType:     inst.InstanceType,
		NodeAddresses:    append(addrs, corev1.NodeAddress{Type: corev1.NodeHostName, Address: node.Name}),
		Region:           inst.Region,
		AdditionalLabels: map[string]string{MaxVolumesLabel: strconv.Itoa(max(it.Limits.Volumes-1, 0))},
	}, nil
}

func (c *Cloud) instance(ctx context.Context, node *corev1.Node) (*computeapi.Instance, config.Config, error) {
	cfg, err := config.Load(c.ConfigPath)
	if err != nil {
		return nil, cfg, err
	}
	id, err := InstanceID(node)
	if err != nil {
		return nil, cfg, err
	}
	inst, err := GetInstance(ctx, c.Clients, cfg, id)
	return inst, cfg, c.report(node, err)
}

// report puts a permission refusal on the Node as an event, naming the missing action.
func (c *Cloud) report(node *corev1.Node, err error) error {
	if _, denied := ezghclient.AsPermission(err); denied && c.Recorder != nil {
		events.Report(c.Recorder, node, err)
	}
	return err
}
