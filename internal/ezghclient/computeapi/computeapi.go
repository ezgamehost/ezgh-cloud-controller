// Package computeapi is a typed client for compute-api (EZGH Compute): instances, their consoles,
// SSH keys, the organization's instance types and compute's operations.
package computeapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// Client calls compute-api.
type Client struct {
	API *apiclient.Client
}

// Instance is a virtual machine. Slug is its prefixed ID (ins_…), which request bodies take.
type Instance struct {
	ID             string            `json:"id"`
	Slug           string            `json:"slug"`
	Name           string            `json:"name"`
	ResourceName   string            `json:"resourceName"`
	OrganizationID string            `json:"organizationId"`
	ProjectID      string            `json:"projectId"`
	Region         string            `json:"region"`
	Tags           map[string]string `json:"tags"`
	Bot            *InstanceBot      `json:"bot,omitempty"`
	Status         string            `json:"status"`
	StatusReason   *StatusReason     `json:"statusReason"`
	StatusChecks   *StatusChecks     `json:"statusChecks,omitempty"`
	InstanceType   string            `json:"instanceType"`
	CPUFamily      string            `json:"cpuFamily"`
	VCPUs          int               `json:"vcpus"`
	MemoryMiB      int               `json:"memoryMiB"`
	Architecture   string            `json:"architecture"`
	Image          InstanceImage     `json:"image"`
	SSHKeys        []InstanceSSHKey  `json:"sshKeys"`
	PrimaryENI     *string           `json:"primaryNetworkInterfaceId"`
	ENIs           []string          `json:"networkInterfaceIds"`
	RootVolumeID   *string           `json:"rootVolumeId"`
	VolumeIDs      []string          `json:"volumeIds"`
	LaunchedAt     *string           `json:"launchedAt"`
	CreatedAt      string            `json:"createdAt"`
	UpdatedAt      string            `json:"updatedAt"`
	// VolumeHotplug says attach-volume and detach-volume work while it runs; absent from a
	// compute-api without volume hot-plug.
	VolumeHotplug *bool `json:"volumeHotplug,omitempty"`
	// InstanceStore is the type's local disks, copied at launch; absent (nil) from a compute-api
	// before instance store or for a type without it.
	InstanceStore *InstanceStore `json:"instanceStore,omitempty"`
	// RootDevice is "volume" or "instanceStore"; empty from a compute-api before instance store.
	RootDevice string `json:"rootDevice,omitempty"`
}

// InstanceBot is the Bot attached to an instance; its credentials are available to code on it.
type InstanceBot struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
}

// StatusReason says why an instance is in its status (impaired, failed).
type StatusReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StatusChecks are an instance's two status checks: system (the host, the VM process, volumes
// and the cell: EZGH fixes them) and instance (inside the guest: probes, kernel panics).
type StatusChecks struct {
	System    StatusCheckGroup `json:"system"`
	Instance  StatusCheckGroup `json:"instance"`
	UpdatedAt string           `json:"updatedAt"`
}

// StatusCheckGroup is one of the two checks: passed, failed, initializing, not_applicable or
// unknown, and the checks it's made of.
type StatusCheckGroup struct {
	Status string        `json:"status"`
	Since  string        `json:"since"`
	Checks []StatusCheck `json:"checks"`
}

// StatusCheck is one check of a group (host, process, volumes; reachability, kernel).
type StatusCheck struct {
	Name   string             `json:"name"`
	Status string             `json:"status"`
	Reason *StatusCheckReason `json:"reason"`
	// Since is when the check entered its status; nil before the cell first judged it.
	Since *string `json:"since"`
}

// StatusCheckReason says why a check failed; Detail has volumeId for the volume reasons.
type StatusCheckReason struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Detail  map[string]string `json:"detail,omitempty"`
}

// HealthEvent is a status check transition, a guest panic, a watchdog reset, a volume I/O error
// or a crash.
type HealthEvent struct {
	ID     string             `json:"id"`
	At     string             `json:"at"`
	Check  string             `json:"check"`
	Kind   string             `json:"kind"`
	Reason *StatusCheckReason `json:"reason"`
}

// InstanceImage is the image alias an instance launched from; ID and SHA256 are the exact image
// (set on instances launched since instance store; empty before).
type InstanceImage struct {
	Alias  string `json:"alias"`
	ID     string `json:"id,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// InstanceStore is a type's local disks on the host: disks of sizeGiB each, with optional
// per-disk caps (nil: uncapped). Contents live only while the instance runs on its host.
type InstanceStore struct {
	Disks          int  `json:"disks"`
	SizeGiB        int  `json:"sizeGiB"`
	IOPS           *int `json:"iops"`
	ThroughputMBps *int `json:"throughputMBps"`
}

// InstanceSSHKey is a key the instance launched with.
type InstanceSSHKey struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
}

// InstanceCreate launches an instance. References are prefixed IDs (ssh_, eni_, fwl_, vol_).
type InstanceCreate struct {
	Name         string      `json:"name"`
	InstanceType string      `json:"instanceType"`
	Image        string      `json:"image,omitempty"`
	SSHKeyIDs    []string    `json:"sshKeyIds,omitempty"`
	UserData     string      `json:"userData,omitempty"`
	RootVolume   *RootVolume `json:"rootVolume,omitempty"`
	// RootDevice is "volume" or "instanceStore" (boot from the type's store disk 0, no RootVolume);
	// sent only when set.
	RootDevice        string            `json:"rootDevice,omitempty"`
	Volumes           []LaunchVolume    `json:"volumes,omitempty"`
	NetworkInterfaces []LaunchInterface `json:"networkInterfaces"`
	Tags              map[string]string `json:"tags,omitempty"`
	Bot               string            `json:"bot,omitempty"`
}

// RootVolume sizes the root volume, or restores it from a snapshot.
type RootVolume struct {
	SizeGiB        int    `json:"sizeGiB,omitempty"`
	VolumeType     string `json:"volumeType,omitempty"`
	IOPS           *int   `json:"iops,omitempty"`
	ThroughputMBps *int   `json:"throughputMBps,omitempty"`
	SnapshotID     string `json:"snapshotId,omitempty"`
}

// LaunchVolume is an existing volume (VolumeID) or a new one.
type LaunchVolume struct {
	VolumeID   string `json:"volumeId,omitempty"`
	SizeGiB    int    `json:"sizeGiB,omitempty"`
	VolumeType string `json:"volumeType,omitempty"`
}

// LaunchInterface is an existing interface (NetworkInterfaceID) or a new one. A nil FirewallIDs
// is the project's default firewall (a private interface's VPC's); an empty one is none. Subnet
// (sbn_…) makes a new interface private, at PrivateIPv4Address when that's set; the primary
// (index 0) may be one (Network Gateways). IPv4 and IPv6 nil leave a private interface's public
// addresses to its subnet's auto-assign; IPv6 is taken only with Subnet.
type LaunchInterface struct {
	NetworkInterfaceID string    `json:"networkInterfaceId,omitempty"`
	IPv4               *bool     `json:"ipv4,omitempty"`
	IPv6               *bool     `json:"ipv6,omitempty"`
	FirewallIDs        *[]string `json:"firewallIds,omitempty"`
	Subnet             string    `json:"subnet,omitempty"`
	PrivateIPv4Address string    `json:"privateIpv4Address,omitempty"`
}

// InstanceUpdate changes what's set. Tags replace the instance's. Bot is nil to leave it, a
// pointer to nil to detach the Bot (sent as null), or a pointer to the Bot to attach.
type InstanceUpdate struct {
	Name *string            `json:"name,omitempty"`
	Tags *map[string]string `json:"tags,omitempty"`
	Bot  **string           `json:"bot,omitempty"`
}

// Operation is an asynchronous change (a launch, start, stop…), followed until FinishedAt is set.
type Operation struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organizationId"`
	ProjectID      *string         `json:"projectId"`
	Type           string          `json:"type"`
	Action         string          `json:"action"`
	Status         string          `json:"status"`
	Target         OperationTarget `json:"target"`
	Cancellable    bool            `json:"cancellable"`
	CreatedBy      Principal       `json:"createdBy"`
	CreatedAt      string          `json:"createdAt"`
	UpdatedAt      string          `json:"updatedAt"`
	StartedAt      *string         `json:"startedAt"`
	FinishedAt     *string         `json:"finishedAt"`
	// Error is a failed operation's { code, message }; the document leaves its fields open.
	Error map[string]any `json:"error"`
}

// OperationTarget is what an operation changes.
type OperationTarget struct {
	ID           string `json:"id"`
	ResourceName string `json:"resourceName"`
	Type         string `json:"type"`
}

// Principal is who started an operation.
type Principal struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ErrorText is a failed operation's error as one line: "message (code)".
func (o *Operation) ErrorText() string {
	msg, _ := o.Error["message"].(string)
	code, _ := o.Error["code"].(string)
	switch {
	case msg != "" && code != "":
		return msg + " (" + code + ")"
	case msg != "":
		return msg
	}
	return code
}

// InstanceType is a type the organization may launch.
type InstanceType struct {
	Name          string `json:"name"`
	Family        string `json:"family"`
	CPUGroup      string `json:"cpuGroup"`
	Architecture  string `json:"architecture"`
	VCPUs         int    `json:"vcpus"`
	MemoryMiB     int    `json:"memoryMiB"`
	NUMANodes     int    `json:"numaNodes"`
	CPUMaxPercent *int   `json:"cpuMaxPercent"`
	Migratable    bool   `json:"migratable"`
	Network       struct {
		BaselineMbps int `json:"baselineMbps"`
		BurstMbps    int `json:"burstMbps"`
		PPS          int `json:"pps"`
	} `json:"network"`
	Storage struct {
		IOPS           int `json:"iops"`
		ThroughputMBps int `json:"throughputMBps"`
	} `json:"storage"`
	Limits struct {
		NetworkInterfaces int `json:"networkInterfaces"`
		Volumes           int `json:"volumes"`
	} `json:"limits"`
	Status string `json:"status"`
	// InstanceStore is the type's local disks, or nil for a type without.
	InstanceStore *InstanceStore `json:"instanceStore"`
}

// SSHKey is a public key instances launch with. Slug is its prefixed ID (ssh_…).
type SSHKey struct {
	ID             string            `json:"id"`
	Slug           string            `json:"slug"`
	Name           string            `json:"name"`
	ResourceName   string            `json:"resourceName"`
	OrganizationID string            `json:"organizationId"`
	ProjectID      string            `json:"projectId"`
	Region         string            `json:"region"`
	Tags           map[string]string `json:"tags"`
	KeyType        string            `json:"keyType"`
	PublicKey      string            `json:"publicKey"`
	Fingerprint    string            `json:"fingerprint"`
	CreatedAt      string            `json:"createdAt"`
	UpdatedAt      string            `json:"updatedAt"`
}

// SSHKeyCreate imports a public key (one OpenSSH line).
type SSHKeyCreate struct {
	Name      string            `json:"name"`
	PublicKey string            `json:"publicKey"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// SSHKeyUpdate changes what's set. Tags replace the key's.
type SSHKeyUpdate struct {
	Name *string            `json:"name,omitempty"`
	Tags *map[string]string `json:"tags,omitempty"`
}

// ConsoleOutput is the tail of an instance's serial output.
type ConsoleOutput struct {
	// Output is base64: the last 64 KiB.
	Output     string `json:"output"`
	CapturedAt string `json:"capturedAt"`
	Truncated  bool   `json:"truncated"`
}

// Console session types.
const (
	ConsoleSerial = "serial"
	ConsoleVNC    = "vnc"
)

// ConsoleSession is a new console session: connect to URL with the ticket (single use, 30 s) as
// a WebSocket subprotocol.
type ConsoleSession struct {
	SessionID string `json:"sessionId"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	Ticket    string `json:"ticket"`
	ExpiresAt string `json:"expiresAt"`
}

// Pages as the API sends them.
type (
	instancePage struct {
		Instances  []Instance `json:"instances"`
		NextCursor *string    `json:"nextCursor"`
	}
	healthEventPage struct {
		HealthEvents []HealthEvent `json:"healthEvents"`
		NextCursor   *string       `json:"nextCursor"`
	}
	sshKeyPage struct {
		SSHKeys    []SSHKey `json:"sshKeys"`
		NextCursor *string  `json:"nextCursor"`
	}
	operationPage struct {
		Operations []Operation `json:"operations"`
		NextCursor *string     `json:"nextCursor"`
	}
	instanceTypeList struct {
		InstanceTypes []InstanceType `json:"instanceTypes"`
	}
)

func orgPath(orgID, rest string, values ...string) string {
	return apiclient.Path("/v1/organizations/{}", orgID) + apiclient.Path(rest, values...)
}

func projectPath(orgID, projectID, rest string, values ...string) string {
	return apiclient.Path("/v1/organizations/{}/projects/{}", orgID, projectID) + apiclient.Path(rest, values...)
}

func pageQuery(limit int, cursor string) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return q
}

// InstanceFilter narrows the instance list; every one given must match.
type InstanceFilter struct {
	Status, InstanceType string
	Tags                 map[string]string
}

// ListInstances reads a page of the project's instances.
func (c *Client) ListInstances(ctx context.Context, orgID, projectID string, filter InstanceFilter, limit int, cursor string) (apiclient.Page[Instance], error) {
	q := pageQuery(limit, cursor)
	if filter.Status != "" {
		q.Set("status", filter.Status)
	}
	if filter.InstanceType != "" {
		q.Set("instanceType", filter.InstanceType)
	}
	for k, v := range filter.Tags {
		q.Set("tag."+k, v)
	}
	var out instancePage
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/instances"), Query: q}, &out)
	if out.Instances == nil {
		out.Instances = []Instance{}
	}
	return apiclient.Page[Instance]{Items: out.Instances, NextCursor: out.NextCursor}, err
}

// GetInstance reads an instance (by ins_ ID or UUID) and its ETag.
func (c *Client) GetInstance(ctx context.Context, orgID, projectID, instance string) (*Instance, string, error) {
	var out Instance
	resp, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/instances/{}", instance)}, &out)
	if err != nil {
		return nil, "", err
	}
	return &out, resp.Header.Get("ETag"), nil
}

// CreateInstance launches an instance: 202 with the CreateInstance operation.
func (c *Client) CreateInstance(ctx context.Context, orgID, projectID string, body InstanceCreate) (*Operation, error) {
	return c.operation(ctx, http.MethodPost, projectPath(orgID, projectID, "/instances"), body)
}

// UpdateInstance renames an instance or replaces its tags, conditional on ifMatch when it's set.
func (c *Client) UpdateInstance(ctx context.Context, orgID, projectID, instance string, body InstanceUpdate, ifMatch string) (*Instance, error) {
	var out Instance
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPatch, Path: projectPath(orgID, projectID, "/instances/{}", instance), Body: body, IfMatch: ifMatch, Create: true}, &out)
	return &out, err
}

// TerminateInstance terminates an instance: 202 with the TerminateInstance operation.
func (c *Client) TerminateInstance(ctx context.Context, orgID, projectID, instance string) (*Operation, error) {
	return c.operation(ctx, http.MethodDelete, projectPath(orgID, projectID, "/instances/{}", instance), nil)
}

// Action is an instance action that answers with an operation: start, stop, reboot, resize.
// body is nil for start, {force} for stop and reboot, {instanceType} for resize.
func (c *Client) Action(ctx context.Context, orgID, projectID, instance, action string, body any) (*Operation, error) {
	return c.operation(ctx, http.MethodPost, projectPath(orgID, projectID, "/instances/{}/"+action, instance), body)
}

// ForceOption is stop's and reboot's body.
type ForceOption struct {
	Force bool `json:"force,omitempty"`
}

// InstanceResize is resize's body.
type InstanceResize struct {
	InstanceType string `json:"instanceType"`
}

func (c *Client) operation(ctx context.Context, method, path string, body any) (*Operation, error) {
	var out Operation
	_, err := c.API.Do(ctx, apiclient.Request{Method: method, Path: path, Body: body, Create: true}, &out)
	return &out, err
}

// VolumeAttach attaches a volume (vol_…).
type VolumeAttach struct {
	VolumeID            string `json:"volumeId"`
	DeleteOnTermination bool   `json:"deleteOnTermination,omitempty"`
}

// VolumeDetach detaches a volume (vol_…). Force frees it from a running instance whether or not
// the guest releases it; a stopped instance ignores it.
type VolumeDetach struct {
	VolumeID string `json:"volumeId"`
	Force    bool   `json:"force,omitempty"`
}

// NetworkInterfaceAttach attaches an interface (eni_…) at DeviceIndex (0: the lowest free).
type NetworkInterfaceAttach struct {
	NetworkInterfaceID string `json:"networkInterfaceId"`
	DeviceIndex        int    `json:"deviceIndex,omitempty"`
}

// NetworkInterfaceDetach detaches an interface (eni_…).
type NetworkInterfaceDetach struct {
	NetworkInterfaceID string `json:"networkInterfaceId"`
}

// Attachment changes an instance's volumes or interfaces (attach-volume, detach-volume,
// attach-network-interface, detach-network-interface). A stopped instance's change answers 200
// with the instance; a running instance's volume change 202 with its AttachVolume or
// DetachVolume operation. Exactly one of the two is returned.
func (c *Client) Attachment(ctx context.Context, orgID, projectID, instance, action string, body any) (*Instance, *Operation, error) {
	var raw json.RawMessage
	resp, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/instances/{}/"+action, instance), Body: body, Create: true}, &raw)
	if err != nil {
		return nil, nil, err
	}
	if resp.Status == http.StatusAccepted {
		var op Operation
		return nil, &op, json.Unmarshal(raw, &op)
	}
	var out Instance
	return &out, nil, json.Unmarshal(raw, &out)
}

// GetConsoleOutput reads an instance's console output (404 console_output_unavailable when it
// never produced any).
func (c *Client) GetConsoleOutput(ctx context.Context, orgID, projectID, instance string) (*ConsoleOutput, error) {
	var out ConsoleOutput
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/instances/{}/console-output", instance)}, &out)
	return &out, err
}

// ListHealthEvents reads a page of an instance's health events of the last 30 days, newest first.
func (c *Client) ListHealthEvents(ctx context.Context, orgID, projectID, instance string, limit int, cursor string) (apiclient.Page[HealthEvent], error) {
	var out healthEventPage
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/instances/{}/health-events", instance), Query: pageQuery(limit, cursor)}, &out)
	if out.HealthEvents == nil {
		out.HealthEvents = []HealthEvent{}
	}
	return apiclient.Page[HealthEvent]{Items: out.HealthEvents, NextCursor: out.NextCursor}, err
}

// CreateConsoleSession opens a console session of type serial or vnc on a running instance (409
// instance_not_running otherwise). It's sent without an Idempotency-Key (not Create): a replay
// would answer ticket null, and a session that isn't used just ends.
func (c *Client) CreateConsoleSession(ctx context.Context, orgID, projectID, instance, sessionType string) (*ConsoleSession, error) {
	var out ConsoleSession
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/instances/{}/console-sessions", instance), Body: map[string]string{"type": sessionType}}, &out)
	return &out, err
}

// ListSSHKeys reads a page of the project's SSH keys.
func (c *Client) ListSSHKeys(ctx context.Context, orgID, projectID string, limit int, cursor string) (apiclient.Page[SSHKey], error) {
	var out sshKeyPage
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/ssh-keys"), Query: pageQuery(limit, cursor)}, &out)
	if out.SSHKeys == nil {
		out.SSHKeys = []SSHKey{}
	}
	return apiclient.Page[SSHKey]{Items: out.SSHKeys, NextCursor: out.NextCursor}, err
}

// GetSSHKey reads a key by ssh_ ID or UUID.
func (c *Client) GetSSHKey(ctx context.Context, orgID, projectID, key string) (*SSHKey, error) {
	var out SSHKey
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: projectPath(orgID, projectID, "/ssh-keys/{}", key)}, &out)
	return &out, err
}

// ImportSSHKey imports a public key (409 ssh_key_exists for a fingerprint the project has).
func (c *Client) ImportSSHKey(ctx context.Context, orgID, projectID string, body SSHKeyCreate) (*SSHKey, error) {
	var out SSHKey
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPost, Path: projectPath(orgID, projectID, "/ssh-keys"), Body: body, Create: true}, &out)
	return &out, err
}

// UpdateSSHKey renames a key or replaces its tags.
func (c *Client) UpdateSSHKey(ctx context.Context, orgID, projectID, key string, body SSHKeyUpdate) (*SSHKey, error) {
	var out SSHKey
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodPatch, Path: projectPath(orgID, projectID, "/ssh-keys/{}", key), Body: body, Create: true}, &out)
	return &out, err
}

// DeleteSSHKey deletes a key; instances keep the copy they launched with.
func (c *Client) DeleteSSHKey(ctx context.Context, orgID, projectID, key string) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodDelete, Path: projectPath(orgID, projectID, "/ssh-keys/{}", key), Create: true}, nil)
	return err
}

// ListInstanceTypes reads every type the organization can launch (not paged).
func (c *Client) ListInstanceTypes(ctx context.Context, orgID string) ([]InstanceType, error) {
	var out instanceTypeList
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: orgPath(orgID, "/instance-types")}, &out)
	if out.InstanceTypes == nil {
		out.InstanceTypes = []InstanceType{}
	}
	return out.InstanceTypes, err
}

// GetInstanceType reads a type by name (t0.small).
func (c *Client) GetInstanceType(ctx context.Context, orgID, name string) (*InstanceType, error) {
	var out InstanceType
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: orgPath(orgID, "/instance-types/{}", name)}, &out)
	return &out, err
}

// ListOperations reads a page of the organization's compute operations, newest first.
func (c *Client) ListOperations(ctx context.Context, orgID, status, targetResourceName string, limit int, cursor string) (apiclient.Page[Operation], error) {
	q := pageQuery(limit, cursor)
	if status != "" {
		q.Set("status", status)
	}
	if targetResourceName != "" {
		q.Set("targetResourceName", targetResourceName)
	}
	var out operationPage
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: orgPath(orgID, "/operations"), Query: q}, &out)
	if out.Operations == nil {
		out.Operations = []Operation{}
	}
	return apiclient.Page[Operation]{Items: out.Operations, NextCursor: out.NextCursor}, err
}

// GetOperation reads an operation.
func (c *Client) GetOperation(ctx context.Context, orgID, operationID string) (*Operation, error) {
	var out Operation
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: orgPath(orgID, "/operations/{}", operationID)}, &out)
	return &out, err
}

// CancelOperation cancels a queued or running operation (409 operation_finished or
// operation_not_cancellable otherwise): 202 with the operation. It takes no Idempotency-Key.
func (c *Client) CancelOperation(ctx context.Context, orgID, operationID string) (*Operation, error) {
	var out Operation
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodDelete, Path: orgPath(orgID, "/operations/{}", operationID)}, &out)
	return &out, err
}
