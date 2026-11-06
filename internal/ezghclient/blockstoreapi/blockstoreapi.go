// Package blockstoreapi is a typed client for blockstore-api (EZGH Block Storage): a project's
// volumes and snapshots, the volume types an organization can use, and the operations that
// creating, modifying and deleting them run as.
//
// Every write declares an Idempotency-Key, so each carries one, the same on every attempt: a
// retry after a lost answer is replayed rather than done twice.
package blockstoreapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
)

// Client calls blockstore-api.
type Client struct {
	API *apiclient.Client
}

// The shapes of blockstore-api (api/openapi/blockstore-api.json), in the API's order. Byte
// counts are decimal strings; throughput is MB/s (10^6 bytes per second).

// Volume is an encrypted block storage volume. Slug is its ID (vol_…), which bodies and filters
// take; ID is its UUID.
type Volume struct {
	ID                 string            `json:"id"`
	Slug               string            `json:"slug"`
	Name               string            `json:"name"`
	ResourceName       string            `json:"resourceName"`
	OrganizationID     string            `json:"organizationId"`
	ProjectID          string            `json:"projectId"`
	Region             string            `json:"region"`
	Tags               map[string]string `json:"tags"`
	Status             string            `json:"status"`
	VolumeType         string            `json:"volumeType"`
	SizeBytes          string            `json:"sizeBytes"`
	IOPS               int               `json:"iops"`
	ThroughputMBps     int               `json:"throughputMBps"`
	Source             VolumeSource      `json:"source"`
	Attachment         *VolumeAttachment `json:"attachment"`
	DeletionProtection bool              `json:"deletionProtection"`
	Encryption         struct {
		KeyManagement string `json:"keyManagement"`
	} `json:"encryption"`
	Modification  *Modification `json:"modification"`
	Progress      *Progress     `json:"progress"`
	FailureReason *string       `json:"failureReason"`
	CreatedAt     string        `json:"createdAt"`
	UpdatedAt     string        `json:"updatedAt"`
}

// VolumeSource is what a volume was made from: empty, an image (img_…) or a snapshot (snp_…).
type VolumeSource struct {
	Kind        string `json:"kind"`
	Image       string `json:"image,omitempty"`
	ImageSha256 string `json:"imageSha256,omitempty"`
	Snapshot    string `json:"snapshot,omitempty"`
}

// VolumeAttachment is the instance a volume is attached to.
type VolumeAttachment struct {
	Instance             string `json:"instance"`
	InstanceResourceName string `json:"instanceResourceName"`
	DeleteOnTermination  bool   `json:"deleteOnTermination"`
	AttachedAt           string `json:"attachedAt"`
}

// Modification is a change being applied: a growth or new IOPS and throughput (applying), or a
// type change (moving).
type Modification struct {
	State          string `json:"state"`
	VolumeType     string `json:"volumeType"`
	SizeBytes      string `json:"sizeBytes"`
	IOPS           int    `json:"iops"`
	ThroughputMBps int    `json:"throughputMBps"`
	StartedAt      string `json:"startedAt"`
}

// Progress is how far a copy (from an image or snapshot) has got.
type Progress struct {
	BytesDone  string `json:"bytesDone"`
	BytesTotal string `json:"bytesTotal"`
}

// VolumeCreate is a new volume. SizeBytes is required for an empty volume; VolumeType except for
// a restore, which keeps the snapshot's.
type VolumeCreate struct {
	Name               string            `json:"name"`
	VolumeType         string            `json:"volumeType,omitempty"`
	SizeBytes          string            `json:"sizeBytes,omitempty"`
	IOPS               *int              `json:"iops,omitempty"`
	ThroughputMBps     *int              `json:"throughputMBps,omitempty"`
	Source             CreateSource      `json:"source"`
	DeletionProtection *bool             `json:"deletionProtection,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
}

// CreateSource is empty, an image alias (linux/ubuntu:24.04) or a snapshot's snp_ ID.
type CreateSource struct {
	Kind         string `json:"kind"`
	Image        string `json:"image,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	SnapshotID   string `json:"snapshotId,omitempty"`
}

// VolumeUpdate renames or retags a volume (Tags replace its tags), or sets its deletion
// protection.
type VolumeUpdate struct {
	Name               *string            `json:"name,omitempty"`
	DeletionProtection *bool              `json:"deletionProtection,omitempty"`
	Tags               *map[string]string `json:"tags,omitempty"`
}

// VolumeModify grows a volume or changes its IOPS and throughput (or, later, its type).
type VolumeModify struct {
	VolumeType     string `json:"volumeType,omitempty"`
	SizeBytes      string `json:"sizeBytes,omitempty"`
	IOPS           *int   `json:"iops,omitempty"`
	ThroughputMBps *int   `json:"throughputMBps,omitempty"`
}

// Snapshot is a crash-consistent copy of a volume. Slug is its ID (snp_…).
type Snapshot struct {
	ID              string            `json:"id"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	ResourceName    string            `json:"resourceName"`
	OrganizationID  string            `json:"organizationId"`
	ProjectID       string            `json:"projectId"`
	Region          string            `json:"region"`
	Tags            map[string]string `json:"tags"`
	Status          string            `json:"status"`
	Volume          string            `json:"volume"`
	VolumeType      string            `json:"volumeType"`
	VolumeSizeBytes string            `json:"volumeSizeBytes"`
	StorageBytes    string            `json:"storageBytes"`
	CreatedAt       string            `json:"createdAt"`
	UpdatedAt       string            `json:"updatedAt"`
}

// SnapshotCreate snapshots a volume, by its vol_ ID.
type SnapshotCreate struct {
	VolumeID string            `json:"volumeId"`
	Name     string            `json:"name"`
	Tags     map[string]string `json:"tags,omitempty"`
}

// SnapshotUpdate renames or retags a snapshot.
type SnapshotUpdate struct {
	Name *string            `json:"name,omitempty"`
	Tags *map[string]string `json:"tags,omitempty"`
}

// Operation is an asynchronous change: poll it until its status is succeeded, failed or
// cancelled. Its ID is a UUID.
type Operation struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organizationId"`
	ProjectID      *string         `json:"projectId"`
	Type           string          `json:"type"`
	Action         string          `json:"action"`
	Status         string          `json:"status"`
	Target         OperationTarget `json:"target"`
	Cancellable    bool            `json:"cancellable"`
	CreatedBy      struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"createdBy"`
	CreatedAt  string  `json:"createdAt"`
	UpdatedAt  string  `json:"updatedAt"`
	StartedAt  *string `json:"startedAt"`
	FinishedAt *string `json:"finishedAt"`
	// Error is a failed operation's { code, message }, as the API gives it.
	Error json.RawMessage `json:"error"`
}

// OperationTarget is the resource an operation changes.
type OperationTarget struct {
	ID           string `json:"id"`
	ResourceName string `json:"resourceName"`
	Type         string `json:"type"`
}

// VolumeOperation is a volume write's answer: the operation, with the volume as it is now.
type VolumeOperation struct {
	Operation
	Volume Volume `json:"volume"`
}

// SnapshotOperation is a snapshot write's answer: the operation, with the snapshot as it is now.
type SnapshotOperation struct {
	Operation
	Snapshot Snapshot `json:"snapshot"`
}

// VolumeType is a kind of volume an organization can use, with its performance and size range.
type VolumeType struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Default     bool   `json:"default"`
	Status      string `json:"status"`
	Visibility  string `json:"visibility"`
	SizeGiB     struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"sizeGiB"`
	Baseline Performance `json:"baseline"`
	Maximum  Performance `json:"maximum"`
	Burst    struct {
		Factor  float64 `json:"factor"`
		Seconds int     `json:"seconds"`
	} `json:"burst"`
	Ratios struct {
		MaxIOPSPerGiB  int     `json:"maxIopsPerGiB"`
		MaxMBpsPerIOPS float64 `json:"maxMBpsPerIops"`
	} `json:"ratios"`
}

// Performance is IOPS and throughput (MB/s).
type Performance struct {
	IOPS           int `json:"iops"`
	ThroughputMBps int `json:"throughputMBps"`
}

type volumePage struct {
	Volumes    []Volume `json:"volumes"`
	NextCursor *string  `json:"nextCursor"`
}

type snapshotPage struct {
	Snapshots  []Snapshot `json:"snapshots"`
	NextCursor *string    `json:"nextCursor"`
}

type operationPage struct {
	Operations []Operation `json:"operations"`
	NextCursor *string     `json:"nextCursor"`
}

type volumeTypeList struct {
	VolumeTypes []VolumeType `json:"volumeTypes"`
}

var p = apiclient.Path

func pageQuery(limit int, cursor string, filters ...string) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	for i := 0; i+1 < len(filters); i += 2 {
		if filters[i+1] != "" {
			q.Set(filters[i], filters[i+1])
		}
	}
	return q
}

func projectPath(orgID, projectID, rest string, values ...string) string {
	return p("/v1/organizations/{}/projects/{}", orgID, projectID) + p(rest, values...)
}

func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodGet, Path: path, Query: query}, out)
	return err
}

// write sends a write with an Idempotency-Key, the same on every attempt.
func (c *Client) write(ctx context.Context, method, path string, body, out any) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: method, Path: path, Body: body, Create: true}, out)
	return err
}

// ListVolumes reads a page of the project's volumes, newest first; status, volumeType and
// instance (ins_…), when set, filter it.
func (c *Client) ListVolumes(ctx context.Context, orgID, projectID, status, volumeType, instance string, limit int, cursor string) (apiclient.Page[Volume], error) {
	var out volumePage
	err := c.get(ctx, projectPath(orgID, projectID, "/volumes"), pageQuery(limit, cursor, "status", status, "volumeType", volumeType, "instance", instance), &out)
	if out.Volumes == nil {
		out.Volumes = []Volume{}
	}
	return apiclient.Page[Volume]{Items: out.Volumes, NextCursor: out.NextCursor}, err
}

// GetVolume reads a volume by ID (vol_…) or UUID.
func (c *Client) GetVolume(ctx context.Context, orgID, projectID, volume string) (*Volume, error) {
	var out Volume
	return &out, c.get(ctx, projectPath(orgID, projectID, "/volumes/{}", volume), nil, &out)
}

// CreateVolume starts creating a volume: the CreateVolume operation, with the volume (creating).
func (c *Client) CreateVolume(ctx context.Context, orgID, projectID string, body VolumeCreate) (*VolumeOperation, error) {
	var out VolumeOperation
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/volumes"), body, &out)
}

// UpdateVolume renames or retags a volume, or sets its deletion protection.
func (c *Client) UpdateVolume(ctx context.Context, orgID, projectID, volume string, body VolumeUpdate) (*Volume, error) {
	var out Volume
	return &out, c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/volumes/{}", volume), body, &out)
}

// ModifyVolume starts growing a volume or changing its performance: one change at a time.
func (c *Client) ModifyVolume(ctx context.Context, orgID, projectID, volume string, body VolumeModify) (*VolumeOperation, error) {
	var out VolumeOperation
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/volumes/{}/modify", volume), body, &out)
}

// DeleteVolume starts deleting a volume (409 volume_in_use while attached,
// deletion_protection_enabled while protected). Its snapshots stay.
func (c *Client) DeleteVolume(ctx context.Context, orgID, projectID, volume string) (*VolumeOperation, error) {
	var out VolumeOperation
	return &out, c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/volumes/{}", volume), nil, &out)
}

// ListSnapshots reads a page of the project's snapshots, newest first; status and volume
// (vol_…), when set, filter it.
func (c *Client) ListSnapshots(ctx context.Context, orgID, projectID, status, volume string, limit int, cursor string) (apiclient.Page[Snapshot], error) {
	var out snapshotPage
	err := c.get(ctx, projectPath(orgID, projectID, "/snapshots"), pageQuery(limit, cursor, "status", status, "volume", volume), &out)
	if out.Snapshots == nil {
		out.Snapshots = []Snapshot{}
	}
	return apiclient.Page[Snapshot]{Items: out.Snapshots, NextCursor: out.NextCursor}, err
}

// GetSnapshot reads a snapshot by ID (snp_…) or UUID.
func (c *Client) GetSnapshot(ctx context.Context, orgID, projectID, snapshot string) (*Snapshot, error) {
	var out Snapshot
	return &out, c.get(ctx, projectPath(orgID, projectID, "/snapshots/{}", snapshot), nil, &out)
}

// CreateSnapshot starts snapshotting a volume.
func (c *Client) CreateSnapshot(ctx context.Context, orgID, projectID string, body SnapshotCreate) (*SnapshotOperation, error) {
	var out SnapshotOperation
	return &out, c.write(ctx, http.MethodPost, projectPath(orgID, projectID, "/snapshots"), body, &out)
}

// UpdateSnapshot renames or retags a snapshot.
func (c *Client) UpdateSnapshot(ctx context.Context, orgID, projectID, snapshot string, body SnapshotUpdate) (*Snapshot, error) {
	var out Snapshot
	return &out, c.write(ctx, http.MethodPatch, projectPath(orgID, projectID, "/snapshots/{}", snapshot), body, &out)
}

// DeleteSnapshot starts deleting a snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, orgID, projectID, snapshot string) (*SnapshotOperation, error) {
	var out SnapshotOperation
	return &out, c.write(ctx, http.MethodDelete, projectPath(orgID, projectID, "/snapshots/{}", snapshot), nil, &out)
}

// ListVolumeTypes reads the volume types the organization can use.
func (c *Client) ListVolumeTypes(ctx context.Context, orgID string) ([]VolumeType, error) {
	var out volumeTypeList
	err := c.get(ctx, p("/v1/organizations/{}/volume-types", orgID), nil, &out)
	if out.VolumeTypes == nil {
		out.VolumeTypes = []VolumeType{}
	}
	return out.VolumeTypes, err
}

// ListOperations reads a page of the organization's Block Storage operations, newest first;
// status and targetResourceName, when set, filter it.
func (c *Client) ListOperations(ctx context.Context, orgID, status, target string, limit int, cursor string) (apiclient.Page[Operation], error) {
	var out operationPage
	err := c.get(ctx, p("/v1/organizations/{}/operations", orgID), pageQuery(limit, cursor, "status", status, "targetResourceName", target), &out)
	if out.Operations == nil {
		out.Operations = []Operation{}
	}
	return apiclient.Page[Operation]{Items: out.Operations, NextCursor: out.NextCursor}, err
}

// GetOperation reads an operation by its UUID.
func (c *Client) GetOperation(ctx context.Context, orgID, operation string) (*Operation, error) {
	var out Operation
	return &out, c.get(ctx, p("/v1/organizations/{}/operations/{}", orgID, operation), nil, &out)
}

// CancelOperation asks for an operation to stop (409 operation_finished or
// operation_not_cancellable when it can't). It declares no Idempotency-Key.
func (c *Client) CancelOperation(ctx context.Context, orgID, operation string) (*Operation, error) {
	var out Operation
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodDelete, Path: p("/v1/organizations/{}/operations/{}", orgID, operation)}, &out)
	return &out, err
}
