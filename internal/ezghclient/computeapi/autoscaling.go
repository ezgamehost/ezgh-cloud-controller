package computeapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// AutoscalingGroup keeps a number of instances launched from one launch spec running.
type AutoscalingGroup struct {
	ID                      string              `json:"id"`
	Name                    string              `json:"name"`
	Status                  string              `json:"status"`
	MinSize                 int                 `json:"minSize"`
	MaxSize                 int                 `json:"maxSize"`
	DesiredCapacity         int                 `json:"desiredCapacity"`
	LaunchSpec              AutoscalingSpec     `json:"launchSpec"`
	LaunchSpecVersion       int                 `json:"launchSpecVersion"`
	Policies                []AutoscalingPolicy `json:"policies"`
	InstanceWarmupSeconds   int                 `json:"instanceWarmupSeconds"`
	ScaleInCooldownSeconds  int                 `json:"scaleInCooldownSeconds"`
	HealthCheckGraceSeconds int                 `json:"healthCheckGraceSeconds"`
	TargetGroups            []string            `json:"targetGroups"`
	Release                 AutoscalingRelease  `json:"release"`
	LastError               *AutoscalingError   `json:"lastError"`
	CreatedAt               string              `json:"createdAt"`
	UpdatedAt               string              `json:"updatedAt"`
}

// AutoscalingError is the last thing that went wrong in a group.
type AutoscalingError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	At      string `json:"at"`
}

// AutoscalingSpec is a group's launch spec as read: nulls where a field is unset, the image
// every member launches (ResolvedImage) and whether user data is set (it's never returned).
// Its writable part is LaunchSpecInput.
type AutoscalingSpec struct {
	InstanceType      string              `json:"instanceType"`
	Image             string              `json:"image"`
	ResolvedImage     string              `json:"resolvedImage"`
	SSHKeyIDs         []string            `json:"sshKeyIds"`
	UserData          *string             `json:"userData"`
	UserDataPresent   bool                `json:"userDataPresent"`
	RootVolume        SpecDiskView        `json:"rootVolume"`
	NetworkInterfaces []SpecInterfaceView `json:"networkInterfaces"`
	Volumes           []SpecDiskView      `json:"volumes"`
	Tags              map[string]string   `json:"tags"`
	Bot               *string             `json:"bot"`
}

// SpecDiskView is a launch spec's disk as read: every field present, null when unset.
type SpecDiskView struct {
	SizeGiB        *int    `json:"sizeGiB"`
	VolumeType     *string `json:"volumeType"`
	IOPS           *int    `json:"iops"`
	ThroughputMBps *int    `json:"throughputMBps"`
}

// Input is the disk as a write, its nulls left out.
func (d SpecDiskView) Input() SpecDisk {
	w := SpecDisk{IOPS: d.IOPS, ThroughputMBps: d.ThroughputMBps}
	if d.SizeGiB != nil {
		w.SizeGiB = *d.SizeGiB
	}
	if d.VolumeType != nil {
		w.VolumeType = *d.VolumeType
	}
	return w
}

// SpecInterfaceView is a launch spec's interface as read: every field present, null when unset
// (firewallIds null is the default firewall, [] none).
type SpecInterfaceView struct {
	IPv4        *bool     `json:"ipv4"`
	IPv6        *bool     `json:"ipv6"`
	FirewallIDs *[]string `json:"firewallIds"`
	Subnet      *string   `json:"subnet"`
}

// Input is the interface as a write, its nulls left out.
func (n SpecInterfaceView) Input() SpecInterface {
	w := SpecInterface{IPv4: n.IPv4, IPv6: n.IPv6, FirewallIDs: n.FirewallIDs}
	if n.Subnet != nil {
		w.Subnet = *n.Subnet
	}
	return w
}

// AutoscalingPolicy scales the group to hold a metric at its target.
type AutoscalingPolicy struct {
	Name   string            `json:"name"`
	Metric AutoscalingMetric `json:"metric"`
	Target string            `json:"target"`
}

// AutoscalingMetric is cpuUtilization, memoryUtilization or promql (with its Query).
type AutoscalingMetric struct {
	Kind  string  `json:"kind"`
	Query *string `json:"query"`
}

// AutoscalingRelease is a launch spec change rolling out to the members.
type AutoscalingRelease struct {
	BatchPercent  int     `json:"batchPercent"`
	Status        *string `json:"status"`
	TargetVersion *int    `json:"targetVersion"`
	Replaced      int     `json:"replaced"`
	Remaining     int     `json:"remaining"`
	Reason        *string `json:"reason"`
}

// AutoscalingMember is an instance of a group.
type AutoscalingMember struct {
	InstanceID        string                    `json:"instanceId"`
	LaunchSpecVersion int                       `json:"launchSpecVersion"`
	State             string                    `json:"state"`
	Health            string                    `json:"health"`
	LaunchedAt        string                    `json:"launchedAt"`
	TargetHealth      []AutoscalingTargetHealth `json:"targetHealth"`
}

// AutoscalingTargetHealth is a member's state in one attached target group; State is null
// before registration or when unknown.
type AutoscalingTargetHealth struct {
	TargetGroup string  `json:"targetGroup"`
	State       *string `json:"state"`
	Reason      *string `json:"reason"`
}

// AutoscalingActivity is something the group did.
type AutoscalingActivity struct {
	ID      string         `json:"id"`
	At      string         `json:"at"`
	Kind    string         `json:"kind"`
	Cause   string         `json:"cause"`
	Outcome string         `json:"outcome"`
	Details map[string]any `json:"details"`
}

// SpecDisk is the root volume or a new volume of a launch spec; unset fields are null when read
// and left out when written.
type SpecDisk struct {
	SizeGiB        int    `json:"sizeGiB,omitempty"`
	VolumeType     string `json:"volumeType,omitempty"`
	IOPS           *int   `json:"iops,omitempty"`
	ThroughputMBps *int   `json:"throughputMBps,omitempty"`
}

// SpecInterface is a new network interface of a launch spec. A nil FirewallIDs is the default
// firewall; an empty one is none.
type SpecInterface struct {
	IPv4        *bool     `json:"ipv4,omitempty"`
	IPv6        *bool     `json:"ipv6,omitempty"`
	FirewallIDs *[]string `json:"firewallIds,omitempty"`
	Subnet      string    `json:"subnet,omitempty"`
}

// LaunchSpecInput is what a group launches: a launch minus what can't repeat across members.
// UserData is absent to keep the stored user data (on a PUT), a JSON string to set it, or null
// to clear it.
type LaunchSpecInput struct {
	InstanceType      string            `json:"instanceType"`
	Image             string            `json:"image"`
	SSHKeyIDs         []string          `json:"sshKeyIds,omitempty"`
	UserData          json.RawMessage   `json:"userData,omitempty"`
	RootVolume        *SpecDisk         `json:"rootVolume,omitempty"`
	Volumes           []SpecDisk        `json:"volumes,omitempty"`
	NetworkInterfaces []SpecInterface   `json:"networkInterfaces"`
	Tags              map[string]string `json:"tags,omitempty"`
	Bot               string            `json:"bot,omitempty"`
}

// ReleaseInput is the release setting a group takes.
type ReleaseInput struct {
	BatchPercent int `json:"batchPercent"`
}

// AutoscalingGroupCreate creates a group; nil numbers take the API's defaults.
type AutoscalingGroupCreate struct {
	Name                    string              `json:"name"`
	MinSize                 int                 `json:"minSize"`
	MaxSize                 int                 `json:"maxSize"`
	DesiredCapacity         *int                `json:"desiredCapacity,omitempty"`
	LaunchSpec              LaunchSpecInput     `json:"launchSpec"`
	Policies                []AutoscalingPolicy `json:"policies,omitempty"`
	InstanceWarmupSeconds   *int                `json:"instanceWarmupSeconds,omitempty"`
	ScaleInCooldownSeconds  *int                `json:"scaleInCooldownSeconds,omitempty"`
	HealthCheckGraceSeconds *int                `json:"healthCheckGraceSeconds,omitempty"`
	Release                 *ReleaseInput       `json:"release,omitempty"`
	TargetGroups            []string            `json:"targetGroups,omitempty"`
}

// AutoscalingGroupUpdate changes what's set. Policies and target groups replace the group's.
type AutoscalingGroupUpdate struct {
	MinSize                 *int                 `json:"minSize,omitempty"`
	MaxSize                 *int                 `json:"maxSize,omitempty"`
	DesiredCapacity         *int                 `json:"desiredCapacity,omitempty"`
	Policies                *[]AutoscalingPolicy `json:"policies,omitempty"`
	InstanceWarmupSeconds   *int                 `json:"instanceWarmupSeconds,omitempty"`
	ScaleInCooldownSeconds  *int                 `json:"scaleInCooldownSeconds,omitempty"`
	HealthCheckGraceSeconds *int                 `json:"healthCheckGraceSeconds,omitempty"`
	Release                 *ReleaseInput        `json:"release,omitempty"`
	TargetGroups            *[]string            `json:"targetGroups,omitempty"`
}

type autoscalingGroupPage struct {
	Groups     []AutoscalingGroup `json:"autoscalingGroups"`
	NextCursor *string            `json:"nextCursor"`
}

type autoscalingMemberPage struct {
	Members    []AutoscalingMember `json:"members"`
	NextCursor *string             `json:"nextCursor"`
}

type autoscalingActivityPage struct {
	Activities []AutoscalingActivity `json:"activities"`
	NextCursor *string               `json:"nextCursor"`
}

// ListAutoscalingGroups reads a page of the project's groups.
func (c *Client) ListAutoscalingGroups(ctx context.Context, orgID, projectID string, limit int, cursor string) (apiclient.Page[AutoscalingGroup], error) {
	var out autoscalingGroupPage
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/autoscaling-groups"), Query: pageQuery(limit, cursor)}, &out)
	if out.Groups == nil {
		out.Groups = []AutoscalingGroup{}
	}
	return apiclient.Page[AutoscalingGroup]{Items: out.Groups, NextCursor: out.NextCursor}, err
}

// GetAutoscalingGroup reads a group (by asg_ ID) and its ETag.
func (c *Client) GetAutoscalingGroup(ctx context.Context, orgID, projectID, group string) (*AutoscalingGroup, string, error) {
	var out AutoscalingGroup
	resp, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/autoscaling-groups/{}", group)}, &out)
	if err != nil {
		return nil, "", err
	}
	return &out, resp.Header.Get("ETag"), nil
}

// CreateAutoscalingGroup creates a group: 202 with the operation.
func (c *Client) CreateAutoscalingGroup(ctx context.Context, orgID, projectID string, body AutoscalingGroupCreate) (*Operation, error) {
	return c.operation(ctx, http.MethodPost, projectPath(orgID, projectID, "/autoscaling-groups"), body)
}

// UpdateAutoscalingGroup changes a group's sizes, policies and timings, conditional on ifMatch.
func (c *Client) UpdateAutoscalingGroup(ctx context.Context, orgID, projectID, group string, body AutoscalingGroupUpdate, ifMatch string) (*AutoscalingGroup, error) {
	var out AutoscalingGroup
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPatch, Path: projectPath(orgID, projectID, "/autoscaling-groups/{}", group), Body: body, IfMatch: ifMatch, Create: true}, &out)
	return &out, err
}

// SetAutoscalingLaunchSpec replaces a group's launch spec, conditional on ifMatch: 202 with the
// operation, and a release rolls the members onto it.
func (c *Client) SetAutoscalingLaunchSpec(ctx context.Context, orgID, projectID, group string, body LaunchSpecInput, ifMatch string) (*Operation, error) {
	var out Operation
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPut, Path: projectPath(orgID, projectID, "/autoscaling-groups/{}/launch-spec", group), Body: body, IfMatch: ifMatch, Create: true}, &out)
	return &out, err
}

// CancelAutoscalingRelease stops the group's running release.
func (c *Client) CancelAutoscalingRelease(ctx context.Context, orgID, projectID, group string) (*AutoscalingGroup, error) {
	var out AutoscalingGroup
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/autoscaling-groups/{}/release/cancel", group), Create: true}, &out)
	return &out, err
}

// DeleteAutoscalingGroup deletes a group and its members: 202 with the operation.
func (c *Client) DeleteAutoscalingGroup(ctx context.Context, orgID, projectID, group string) (*Operation, error) {
	return c.operation(ctx, http.MethodDelete, projectPath(orgID, projectID, "/autoscaling-groups/{}", group), nil)
}

// ListAutoscalingMembers reads a page of a group's members.
func (c *Client) ListAutoscalingMembers(ctx context.Context, orgID, projectID, group string, limit int, cursor string) (apiclient.Page[AutoscalingMember], error) {
	var out autoscalingMemberPage
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/autoscaling-groups/{}/members", group), Query: pageQuery(limit, cursor)}, &out)
	if out.Members == nil {
		out.Members = []AutoscalingMember{}
	}
	return apiclient.Page[AutoscalingMember]{Items: out.Members, NextCursor: out.NextCursor}, err
}

// ListAutoscalingActivities reads a page of a group's activities, newest first.
func (c *Client) ListAutoscalingActivities(ctx context.Context, orgID, projectID, group string, limit int, cursor string) (apiclient.Page[AutoscalingActivity], error) {
	var out autoscalingActivityPage
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/autoscaling-groups/{}/activities", group), Query: pageQuery(limit, cursor)}, &out)
	if out.Activities == nil {
		out.Activities = []AutoscalingActivity{}
	}
	return apiclient.Page[AutoscalingActivity]{Items: out.Activities, NextCursor: out.NextCursor}, err
}
