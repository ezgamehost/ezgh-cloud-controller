//go:build conformance

// Gateway API core conformance for GatewayClass ezgh: a proof against a real cluster, never
// CI. Run it (kubeconfig of a 1.36, then a 1.37 cluster in a dev-profile project, `ezgh --profile
// dev`, whose lb.loadBalancersPerProject, lb.targetGroupsPerProject and
// network.ipv4AddressesPerOrganization were raised: the suite makes a load balancer per Gateway):
//
//	KUBECONFIG=… go test -tags conformance -run TestConformance -timeout 6h ./internal/cloud/l7/ \
//	  -args --report-output=$HOME/code/.kubernetes-build/conformance/gateway-<minor>.yaml \
//	  --organization=ezghcloud --project=ezgh-cloud-controller --url=https://docs.ezghcloud.com \
//	  --version=<ezgh-cloud-controller sha> --contact=kubernetes@ezghcloud.com
//
// Expected: every core GATEWAY-HTTP test passes.
package l7

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/gateway-api/conformance"
	"sigs.k8s.io/gateway-api/conformance/utils/suite"
	"sigs.k8s.io/gateway-api/pkg/features"
)

func TestConformance(t *testing.T) {
	opts := conformance.DefaultOptions(t)
	opts.GatewayClassName = ClassName
	opts.ConformanceProfiles = sets.New(suite.GatewayHTTPConformanceProfileName).UnsortedList()
	opts.SupportedFeatures = nil
	for _, f := range features.GatewayCoreFeatures.Union(features.HTTPRouteCoreFeatures).Union(features.ReferenceGrantCoreFeatures).UnsortedList() {
		opts.SupportedFeatures = append(opts.SupportedFeatures, f.Name)
	}
	// A load balancer takes minutes to become active (plan call: 15 minutes for a Gateway).
	tc := &opts.TimeoutConfig
	for _, d := range []*time.Duration{&tc.GatewayMustHaveAddress, &tc.GatewayMustHaveCondition, &tc.GatewayStatusMustHaveListeners,
		&tc.GatewayListenersMustHaveConditions, &tc.RouteMustHaveParents, &tc.HTTPRouteMustHaveCondition, &tc.MaxTimeToConsistency,
		&tc.DeleteTimeout, &tc.DefaultTestTimeout} {
		*d = max(*d, 15*time.Minute)
	}
	conformance.RunConformanceWithOptions(t, opts)
}
