package computeapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/openapitest"
)

// The client against compute-api's own OpenAPI document (api/openapi/compute-api.json).
func TestClientMatchesComputeAPIsOpenAPI(t *testing.T) {
	spec := openapitest.Load(t, "../openapi/compute-api.json")
	rec := openapitest.NewRecorder(t)
	c := &Client{API: &apiclient.Client{Service: "Compute", BaseURL: rec.URL, HTTP: rec.Client()}}
	ctx := context.Background()
	o, p, ins, key, op := "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81", "ins_k2m9x0a7qpe4", "ssh_k2m9x0a7qpe4", "0199a3c2-7000-7000-8000-000000000001"
	asg := "asg_k2m9x0a7qpe4"
	name, tags, n, none, yes := "web-1", map[string]string{"env": "prod"}, 100, []string{}, true
	calls := []openapitest.Call{
		{Name: "ListInstances", Run: func() error {
			_, err := c.ListInstances(ctx, o, p, InstanceFilter{Status: "running", InstanceType: "t0.small", Tags: tags}, 20, "c")
			return err
		}, Response: instancePage{}},
		{Name: "GetInstance", Run: func() error { _, _, err := c.GetInstance(ctx, o, p, ins); return err }, Response: Instance{}},
		{Name: "CreateInstance", Run: func() error {
			_, err := c.CreateInstance(ctx, o, p, InstanceCreate{Name: name, InstanceType: "t0.small", Image: "linux/ubuntu:26.04", SSHKeyIDs: []string{key}, UserData: "eA==",
				RootVolume: &RootVolume{SizeGiB: 20, VolumeType: "d0", IOPS: &n, ThroughputMBps: &n, SnapshotID: "snp_k2m9x0a7qpe4"}, RootDevice: "volume",
				Volumes:           []LaunchVolume{{VolumeID: "vol_k2m9x0a7qpe4"}, {SizeGiB: 10, VolumeType: "d0"}},
				NetworkInterfaces: []LaunchInterface{{IPv4: &yes, FirewallIDs: &none}, {NetworkInterfaceID: "eni_k2m9x0a7qpe4"}, {Subnet: "sbn_k2m9x0a7qpe4", PrivateIPv4Address: "10.0.1.20", IPv4: &yes, IPv6: &yes}}, Tags: tags})
			return err
		}, Request: InstanceCreate{}, Response: Operation{}},
		{Name: "UpdateInstance", Run: func() error {
			_, err := c.UpdateInstance(ctx, o, p, ins, InstanceUpdate{Name: &name, Tags: &tags}, `"e1"`)
			return err
		}, Request: InstanceUpdate{}, Response: Instance{}},
		{Name: "TerminateInstance", Run: func() error { _, err := c.TerminateInstance(ctx, o, p, ins); return err }, Response: Operation{}},
		{Name: "StartInstance", Run: func() error { _, err := c.Action(ctx, o, p, ins, "start", nil); return err }, Response: Operation{}},
		{Name: "StopInstance", Run: func() error { _, err := c.Action(ctx, o, p, ins, "stop", ForceOption{Force: true}); return err }, Request: ForceOption{}, Response: Operation{}},
		{Name: "RebootInstance", Run: func() error { _, err := c.Action(ctx, o, p, ins, "reboot", ForceOption{}); return err }, Request: ForceOption{}, Response: Operation{}},
		{Name: "ResizeInstance", Run: func() error {
			_, err := c.Action(ctx, o, p, ins, "resize", InstanceResize{InstanceType: "t0.medium"})
			return err
		}, Request: InstanceResize{}, Response: Operation{}},
		{Name: "AttachVolume", Run: func() error {
			_, _, err := c.Attachment(ctx, o, p, ins, "attach-volume", VolumeAttach{VolumeID: "vol_k2m9x0a7qpe4", DeleteOnTermination: true})
			return err
		}, Request: VolumeAttach{}, Response: Instance{}},
		{Name: "DetachVolume", Run: func() error {
			_, _, err := c.Attachment(ctx, o, p, ins, "detach-volume", VolumeDetach{VolumeID: "vol_k2m9x0a7qpe4", Force: true})
			return err
		}, Request: VolumeDetach{}, Response: Instance{}},
		// A running instance's volume changes answer 202 with the operation.
		{Name: "AttachVolume (202)", Run: func() error {
			_, _, err := c.Attachment(ctx, o, p, ins, "attach-volume", VolumeAttach{VolumeID: "vol_k2m9x0a7qpe4"})
			return err
		}, Request: VolumeAttach{}, Response: Operation{}},
		{Name: "DetachVolume (202)", Run: func() error {
			_, _, err := c.Attachment(ctx, o, p, ins, "detach-volume", VolumeDetach{VolumeID: "vol_k2m9x0a7qpe4", Force: true})
			return err
		}, Request: VolumeDetach{}, Response: Operation{}},
		{Name: "AttachNetworkInterface", Run: func() error {
			_, _, err := c.Attachment(ctx, o, p, ins, "attach-network-interface", NetworkInterfaceAttach{NetworkInterfaceID: "eni_k2m9x0a7qpe4", DeviceIndex: 1})
			return err
		}, Request: NetworkInterfaceAttach{}, Response: Instance{}},
		{Name: "DetachNetworkInterface", Run: func() error {
			_, _, err := c.Attachment(ctx, o, p, ins, "detach-network-interface", NetworkInterfaceDetach{NetworkInterfaceID: "eni_k2m9x0a7qpe4"})
			return err
		}, Request: NetworkInterfaceDetach{}, Response: Instance{}},
		{Name: "ListInstanceHealthEvents", Run: func() error { _, err := c.ListHealthEvents(ctx, o, p, ins, 20, "c"); return err }, Response: healthEventPage{}},
		{Name: "GetConsoleOutput", Run: func() error { _, err := c.GetConsoleOutput(ctx, o, p, ins); return err }, Response: ConsoleOutput{}},
		{Name: "ListSshKeys", Run: func() error { _, err := c.ListSSHKeys(ctx, o, p, 20, "c"); return err }, Response: sshKeyPage{}},
		{Name: "GetSshKey", Run: func() error { _, err := c.GetSSHKey(ctx, o, p, key); return err }, Response: SSHKey{}},
		{Name: "ImportSshKey", Run: func() error {
			_, err := c.ImportSSHKey(ctx, o, p, SSHKeyCreate{Name: "laptop", PublicKey: "ssh-ed25519 AAAA", Tags: tags})
			return err
		}, Request: SSHKeyCreate{}, Response: SSHKey{}},
		{Name: "UpdateSshKey", Run: func() error {
			_, err := c.UpdateSSHKey(ctx, o, p, key, SSHKeyUpdate{Name: &name, Tags: &tags})
			return err
		}, Request: SSHKeyUpdate{}, Response: SSHKey{}},
		{Name: "DeleteSshKey", Run: func() error { return c.DeleteSSHKey(ctx, o, p, key) }},
		{Name: "ListInstanceTypes", Run: func() error { _, err := c.ListInstanceTypes(ctx, o); return err }, Response: instanceTypeList{}},
		{Name: "GetInstanceType", Run: func() error { _, err := c.GetInstanceType(ctx, o, "t0.small"); return err }, Response: InstanceType{}},
		{Name: "ListOperations", Run: func() error { _, err := c.ListOperations(ctx, o, "running", "ezgh:x", 20, "c"); return err }, Response: operationPage{}},
		{Name: "GetOperation", Run: func() error { _, err := c.GetOperation(ctx, o, op); return err }, Response: Operation{}},
		{Name: "CancelOperation", Run: func() error { _, err := c.CancelOperation(ctx, o, op); return err }, Response: Operation{}},
		{Name: "ListAutoscalingGroups", Run: func() error { _, err := c.ListAutoscalingGroups(ctx, o, p, 20, "c"); return err }, Response: autoscalingGroupPage{}},
		{Name: "GetAutoscalingGroup", Run: func() error { _, _, err := c.GetAutoscalingGroup(ctx, o, p, asg); return err }, Response: AutoscalingGroup{}},
		{Name: "CreateAutoscalingGroup", Run: func() error {
			q := "avg(up)"
			_, err := c.CreateAutoscalingGroup(ctx, o, p, AutoscalingGroupCreate{Name: "web", MinSize: 1, MaxSize: 4, DesiredCapacity: &n, InstanceWarmupSeconds: &n, ScaleInCooldownSeconds: &n, HealthCheckGraceSeconds: &n,
				Release: &ReleaseInput{BatchPercent: 25}, Policies: []AutoscalingPolicy{{Name: "cpu", Metric: AutoscalingMetric{Kind: "cpuUtilization"}, Target: "60"}, {Name: "q", Metric: AutoscalingMetric{Kind: "promql", Query: &q}, Target: "1"}},
				LaunchSpec: LaunchSpecInput{InstanceType: "t0.small", Image: "linux/ubuntu:26.04", SSHKeyIDs: []string{key}, UserData: json.RawMessage(`"eA=="`), RootVolume: &SpecDisk{SizeGiB: 20, VolumeType: "d0", IOPS: &n, ThroughputMBps: &n},
					Volumes: []SpecDisk{{SizeGiB: 10, VolumeType: "d0", IOPS: &n, ThroughputMBps: &n}}, NetworkInterfaces: []SpecInterface{{Subnet: "sbn_k2m9x0a7qpe4", IPv4: &yes, IPv6: &yes, FirewallIDs: &none}}, Tags: tags, Bot: "bot_k2m9x0a7qpe4"}})
			return err
		}, Request: AutoscalingGroupCreate{}, Response: Operation{}},
		{Name: "UpdateAutoscalingGroup", Run: func() error {
			_, err := c.UpdateAutoscalingGroup(ctx, o, p, asg, AutoscalingGroupUpdate{MinSize: &n, MaxSize: &n, DesiredCapacity: &n, InstanceWarmupSeconds: &n, ScaleInCooldownSeconds: &n, HealthCheckGraceSeconds: &n,
				Policies: &[]AutoscalingPolicy{}, Release: &ReleaseInput{BatchPercent: 50}}, `"e1"`)
			return err
		}, Request: AutoscalingGroupUpdate{}, Response: AutoscalingGroup{}},
		{Name: "SetAutoscalingLaunchSpec", Run: func() error {
			_, err := c.SetAutoscalingLaunchSpec(ctx, o, p, asg, LaunchSpecInput{InstanceType: "t0.small", Image: "linux/ubuntu:26.04", UserData: json.RawMessage("null"), NetworkInterfaces: []SpecInterface{{}}}, `"e1"`)
			return err
		}, Request: LaunchSpecInput{}, Response: Operation{}},
		{Name: "CancelAutoscalingRelease", Run: func() error { _, err := c.CancelAutoscalingRelease(ctx, o, p, asg); return err }, Response: AutoscalingGroup{}},
		{Name: "DeleteAutoscalingGroup", Run: func() error { _, err := c.DeleteAutoscalingGroup(ctx, o, p, asg); return err }, Response: Operation{}},
		{Name: "ListAutoscalingMembers", Run: func() error { _, err := c.ListAutoscalingMembers(ctx, o, p, asg, 20, "c"); return err }, Response: autoscalingMemberPage{}},
		{Name: "ListAutoscalingActivities", Run: func() error { _, err := c.ListAutoscalingActivities(ctx, o, p, asg, 20, "c"); return err }, Response: autoscalingActivityPage{}},
	}
	b := "/v1/organizations/{orgId}/projects/{projectId}"
	openapitest.Check(t, spec, rec, calls, map[string]string{
		"POST " + b + "/instances/{instance}/console-sessions": "sent without an Idempotency-Key on purpose (a replay answers ticket null); covered by internal/cli/compute_test.go",
		"GET " + b + "/settings/observability":                 "deprecated: instance metrics are free and automatic",
		"PUT " + b + "/settings/observability":                 "deprecated: instance metrics are free and automatic",
	})
}
