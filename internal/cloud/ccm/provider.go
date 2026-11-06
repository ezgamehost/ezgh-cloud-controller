// Package ccm is the ezgh provider for k8s.io/cloud-provider's cloud controller manager. It
// implements InstancesV2 only: the cloud-node controller initializes each Node from its instance
// (providerID, type, region, addresses) and the cloud-node-lifecycle controller deletes a Node
// once its instance is gone. Everything is read live as the cluster's Bot; nothing is cached.
package ccm

import (
	"io"

	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	cloudprovider "k8s.io/cloud-provider"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

// ProviderName is --cloud-provider's value.
const ProviderName = "ezgh"

// Cloud is the provider.
type Cloud struct {
	Clients *ezghclient.Clients
	// ConfigPath is cloud.json, loaded on each call.
	ConfigPath string
	// Recorder reports refused reads on the Node; Initialize sets it.
	Recorder events.EventRecorder
}

// Register makes c the provider named ezgh.
func Register(c *Cloud) {
	cloudprovider.RegisterCloudProvider(ProviderName, func(io.Reader) (cloudprovider.Interface, error) { return c, nil })
}

// Initialize runs once the CCM holds its Lease, before the controllers start.
func (c *Cloud) Initialize(b cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	broadcaster := events.NewBroadcaster(&events.EventSinkImpl{Interface: b.ClientOrDie("ezgh-cloud-ccm").EventsV1()})
	broadcaster.StartRecordingToSink(stop)
	c.Recorder = broadcaster.NewRecorder(scheme.Scheme, "ezgh-cloud-ccm")
}

func (c *Cloud) InstancesV2() (cloudprovider.InstancesV2, bool)   { return c, true }
func (c *Cloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) { return nil, false }
func (c *Cloud) Instances() (cloudprovider.Instances, bool)       { return nil, false }
func (c *Cloud) Zones() (cloudprovider.Zones, bool)               { return nil, false }
func (c *Cloud) Clusters() (cloudprovider.Clusters, bool)         { return nil, false }
func (c *Cloud) Routes() (cloudprovider.Routes, bool)             { return nil, false }
func (c *Cloud) ProviderName() string                             { return ProviderName }

// HasClusterID is true: cloud.json always names the cluster.
func (c *Cloud) HasClusterID() bool { return true }
