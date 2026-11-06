package lbapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/openapitest"
)

// The client against lb-api's own OpenAPI document (api/openapi/lb-api.json), built from lb-stack's
// manifest and overlay until the service is live for scripts/openapi.sh.
func TestClientMatchesLBAPIsOpenAPI(t *testing.T) {
	spec := openapitest.Load(t, "../openapi/lb-api.json")
	rec := openapitest.NewRecorder(t)
	c := &Client{API: &apiclient.Client{Service: "Load Balancers", BaseURL: rec.URL, HTTP: rec.Client()}}
	ctx := context.Background()
	o, pr := "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	lb, ls, g, tg := "lbr_k2m9x0a7qpe4", "lsn_k2m9x0a7qpe4", "tgp_k2m9x0a7qpe4", "0199a3c2-7000-7000-8000-000000000002"
	op := "0199a3c2-7000-7000-8000-000000000001"
	name, port, text, yes := "web", 443, "tcp", true
	tags := map[string]string{"team": "a"}
	check := HealthCheckInput{Protocol: &text, Port: json.RawMessage("443"), Path: &text, IntervalSeconds: &port, TimeoutSeconds: &port, HealthyThreshold: &port, UnhealthyThreshold: &port, Matcher: &text}
	openapitest.Check(t, spec, rec, []openapitest.Call{
		{Name: "ListLoadBalancers", Run: func() error { _, err := c.ListLoadBalancers(ctx, o, pr, 20, "c"); return err }, Response: loadBalancerPage{}},
		{Name: "GetLoadBalancer", Run: func() error { _, err := c.GetLoadBalancer(ctx, o, pr, lb); return err }, Response: LoadBalancer{}},
		{Name: "CreateLoadBalancer", Run: func() error {
			_, err := c.CreateLoadBalancer(ctx, o, pr, LoadBalancerCreate{Name: name, Description: &name, Tags: tags, Subnet: "sbn_k2m9x0a7qpe4", IPv4Address: "pip_k2m9x0a7qpe4", IPv6: &yes})
			return err
		}, Request: LoadBalancerCreate{}, Response: Operation{}},
		{Name: "UpdateLoadBalancer", Run: func() error {
			_, err := c.UpdateLoadBalancer(ctx, o, pr, lb, LoadBalancerUpdate{Name: &name, Description: &name, Tags: &tags, IPv6: &yes})
			return err
		}, Request: LoadBalancerUpdate{}, Response: LoadBalancer{}},
		{Name: "DeleteLoadBalancer", Run: func() error { _, err := c.DeleteLoadBalancer(ctx, o, pr, lb); return err }, Response: Operation{}},
		{Name: "ListListeners", Run: func() error { _, err := c.ListListeners(ctx, o, pr, lb, 20, "c"); return err }, Response: listenerPage{}},
		{Name: "GetListener", Run: func() error { _, err := c.GetListener(ctx, o, pr, lb, ls); return err }, Response: Listener{}},
		{Name: "CreateListener", Run: func() error {
			_, err := c.CreateListener(ctx, o, pr, lb, ListenerCreate{Protocol: "tls", Port: port, TargetGroup: g, IdleTimeoutSeconds: &port, Certificate: &text, AllowedSourceCidrs: []string{"0.0.0.0/0"}})
			return err
		}, Request: ListenerCreate{}, Response: Listener{}},
		{Name: "UpdateListener", Run: func() error {
			_, err := c.UpdateListener(ctx, o, pr, lb, ls, ListenerUpdate{Port: &port, TargetGroup: &g, IdleTimeoutSeconds: &port, Certificate: &text, AllowedSourceCidrs: []string{"0.0.0.0/0"}})
			return err
		}, Request: ListenerUpdate{}, Response: Listener{}},
		{Name: "DeleteListener", Run: func() error { return c.DeleteListener(ctx, o, pr, lb, ls) }},
		{Name: "ListTargetGroups", Run: func() error { _, err := c.ListTargetGroups(ctx, o, pr, 20, "c"); return err }, Response: targetGroupPage{}},
		{Name: "GetTargetGroup", Run: func() error { _, err := c.GetTargetGroup(ctx, o, pr, g); return err }, Response: TargetGroup{}},
		{Name: "CreateTargetGroup", Run: func() error {
			_, err := c.CreateTargetGroup(ctx, o, pr, TargetGroupCreate{Name: name, Description: &name, Tags: tags, VPC: "vpc_k2m9x0a7qpe4", Protocol: &text, Port: port, DeregistrationDelaySeconds: &port, ProxyProtocol: &text, HealthCheck: &check})
			return err
		}, Request: TargetGroupCreate{}, Response: TargetGroup{}},
		{Name: "UpdateTargetGroup", Run: func() error {
			_, err := c.UpdateTargetGroup(ctx, o, pr, g, TargetGroupUpdate{Name: &name, Description: &name, Tags: &tags, Port: &port, DeregistrationDelaySeconds: &port, ProxyProtocol: &text, HealthCheck: &check})
			return err
		}, Request: TargetGroupUpdate{}, Response: TargetGroup{}},
		{Name: "DeleteTargetGroup", Run: func() error { return c.DeleteTargetGroup(ctx, o, pr, g) }},
		{Name: "ListTargets", Run: func() error { _, err := c.ListTargets(ctx, o, pr, g, 20, "c"); return err }, Response: targetPage{}},
		{Name: "RegisterTarget", Run: func() error {
			_, err := c.RegisterTarget(ctx, o, pr, g, TargetCreate{NetworkInterface: &text, PrivateIPv4: &text, Port: &port})
			return err
		}, Request: TargetCreate{}, Response: Target{}},
		{Name: "DeregisterTarget", Run: func() error { _, err := c.DeregisterTarget(ctx, o, pr, g, tg); return err }, Response: Target{}},
		{Name: "ListOperations", Run: func() error { _, err := c.ListOperations(ctx, o, 20, "c"); return err }, Response: operationPage{}},
		{Name: "CancelOperation", Run: func() error { _, err := c.CancelOperation(ctx, o, op); return err }, Response: Operation{}},
		{Name: "GetOperation", Run: func() error { _, err := c.GetOperation(ctx, o, op); return err }, Response: Operation{}},
	}, nil)
}
