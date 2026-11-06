// Package filesapi is a typed client for files-api (EZGH Files): a project's shared file systems,
// the mount targets that put them in a VPC's subnets, and their access points.
package filesapi

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/computeapi"
)

// Client calls files-api.
type Client struct {
	API *apiclient.Client
}

// Operation is an asynchronous write's 202: files-api's operations are the contract's, like
// compute-api's. Its target's resource name ends in the fsy_, fmt_ or fap_ ID.
type Operation = computeapi.Operation

// Failure is why a resource failed.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Limits are a file system's throughput and metadata operations, set from the organization's quotas.
type Limits struct {
	ThroughputMBps       int `json:"throughputMBps"`
	MetadataOpsPerSecond int `json:"metadataOpsPerSecond"`
}

// FileSystem is a file system. Slug (fsy_…) is what customers name it by; a path also takes its
// ID, a UUID. SizeBytes is a decimal string. Status is an open string.
type FileSystem struct {
	ID                 string            `json:"id"`
	Slug               string            `json:"slug"`
	ResourceName       string            `json:"resourceName"`
	OrganizationID     string            `json:"organizationId"`
	ProjectID          string            `json:"projectId"`
	Region             string            `json:"region"`
	Name               string            `json:"name"`
	Description        *string           `json:"description"`
	Tags               map[string]string `json:"tags"`
	VPC                string            `json:"vpc"`
	SizeBytes          string            `json:"sizeBytes"`
	Limits             Limits            `json:"limits"`
	RootSquash         bool              `json:"rootSquash"`
	RequireTLS         bool              `json:"requireTls"`
	DeletionProtection bool              `json:"deletionProtection"`
	Status             string            `json:"status"`
	Failure            *Failure          `json:"failure"`
	CreatedAt          string            `json:"createdAt"`
	UpdatedAt          string            `json:"updatedAt"`
}

// FileSystemCreate is a create's body.
type FileSystemCreate struct {
	Name               string            `json:"name"`
	Description        *string           `json:"description,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
	VPC                string            `json:"vpc"`
	RootSquash         *bool             `json:"rootSquash,omitempty"`
	RequireTLS         *bool             `json:"requireTls,omitempty"`
	DeletionProtection *bool             `json:"deletionProtection,omitempty"`
}

// FileSystemUpdate is an update's body: only what's set changes; Tags replaces the whole set.
type FileSystemUpdate struct {
	Name               *string            `json:"name,omitempty"`
	Description        *string            `json:"description,omitempty"`
	Tags               *map[string]string `json:"tags,omitempty"`
	RootSquash         *bool              `json:"rootSquash,omitempty"`
	RequireTLS         *bool              `json:"requireTls,omitempty"`
	DeletionProtection *bool              `json:"deletionProtection,omitempty"`
}

// MountTarget is a file system's address in a subnet. Hostname and IPv4Address are nil until
// it's available.
type MountTarget struct {
	ID                 string            `json:"id"`
	Slug               string            `json:"slug"`
	ResourceName       string            `json:"resourceName"`
	OrganizationID     string            `json:"organizationId"`
	ProjectID          string            `json:"projectId"`
	Region             string            `json:"region"`
	Name               string            `json:"name"`
	Tags               map[string]string `json:"tags"`
	FileSystem         string            `json:"fileSystem"`
	Subnet             string            `json:"subnet"`
	VPC                string            `json:"vpc"`
	IPv4Address        *string           `json:"ipv4Address"`
	Hostname           *string           `json:"hostname"`
	AllowedSourceCidrs []string          `json:"allowedSourceCidrs"`
	Status             string            `json:"status"`
	Failure            *Failure          `json:"failure"`
	CreatedAt          string            `json:"createdAt"`
	UpdatedAt          string            `json:"updatedAt"`
}

// MountTargetCreate is a create's body.
type MountTargetCreate struct {
	FileSystem         string            `json:"fileSystem"`
	Subnet             string            `json:"subnet"`
	Name               *string           `json:"name,omitempty"`
	AllowedSourceCidrs []string          `json:"allowedSourceCidrs,omitempty"`
	Tags               map[string]string `json:"tags,omitempty"`
}

// MountTargetUpdate is an update's body: only what's set changes.
type MountTargetUpdate struct {
	Name               *string            `json:"name,omitempty"`
	AllowedSourceCidrs []string           `json:"allowedSourceCidrs,omitempty"`
	Tags               *map[string]string `json:"tags,omitempty"`
}

// CreationInfo is the owner and mode an access point's missing root directory is created with.
type CreationInfo struct {
	OwnerUID    uint32 `json:"ownerUid"`
	OwnerGID    uint32 `json:"ownerGid"`
	Permissions string `json:"permissions"`
}

// RootDirectory is the folder an access point exports.
type RootDirectory struct {
	Path         string        `json:"path"`
	CreationInfo *CreationInfo `json:"creationInfo"`
}

// RootDirectoryInput is a create's root directory: Path defaults to /.
type RootDirectoryInput struct {
	Path         string        `json:"path,omitempty"`
	CreationInfo *CreationInfo `json:"creationInfo,omitempty"`
}

// PosixUser is the user and groups an access point replaces every client's with.
type PosixUser struct {
	UID           uint32   `json:"uid"`
	GID           uint32   `json:"gid"`
	SecondaryGIDs []uint32 `json:"secondaryGids"`
}

// PosixUserInput is a create's POSIX user.
type PosixUserInput struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// AccessPoint is a folder of a file system exported on its own. PosixUser nil means the file
// system's rootSquash applies.
type AccessPoint struct {
	ID             string            `json:"id"`
	Slug           string            `json:"slug"`
	ResourceName   string            `json:"resourceName"`
	OrganizationID string            `json:"organizationId"`
	ProjectID      string            `json:"projectId"`
	Region         string            `json:"region"`
	Name           string            `json:"name"`
	Description    *string           `json:"description"`
	Tags           map[string]string `json:"tags"`
	FileSystem     string            `json:"fileSystem"`
	RootDirectory  RootDirectory     `json:"rootDirectory"`
	PosixUser      *PosixUser        `json:"posixUser"`
	Status         string            `json:"status"`
	Failure        *Failure          `json:"failure"`
	CreatedAt      string            `json:"createdAt"`
	UpdatedAt      string            `json:"updatedAt"`
}

// AccessPointCreate is a create's body. An access point never changes after it, but for its
// name, description and tags.
type AccessPointCreate struct {
	FileSystem    string              `json:"fileSystem"`
	Name          string              `json:"name"`
	Description   *string             `json:"description,omitempty"`
	Tags          map[string]string   `json:"tags,omitempty"`
	RootDirectory *RootDirectoryInput `json:"rootDirectory,omitempty"`
	PosixUser     *PosixUserInput     `json:"posixUser,omitempty"`
}

// AccessPointUpdate is an update's body: only what's set changes.
type AccessPointUpdate struct {
	Name        *string            `json:"name,omitempty"`
	Description *string            `json:"description,omitempty"`
	Tags        *map[string]string `json:"tags,omitempty"`
}

type fileSystemPage struct {
	FileSystems []FileSystem `json:"fileSystems"`
	NextCursor  *string      `json:"nextCursor"`
}

type mountTargetPage struct {
	MountTargets []MountTarget `json:"mountTargets"`
	NextCursor   *string       `json:"nextCursor"`
}

type accessPointPage struct {
	AccessPoints []AccessPoint `json:"accessPoints"`
	NextCursor   *string       `json:"nextCursor"`
}

type operationPage struct {
	Operations []Operation `json:"operations"`
	NextCursor *string     `json:"nextCursor"`
}

var p = apiclient.Path

func projectPath(orgID, projectID string) string {
	return p("/v1/organizations/{}/projects/{}", orgID, projectID)
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

// Every write to a project's resources declares Idempotency-Key, PATCH and DELETE too; a cancel
// doesn't.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	_, err := c.API.Do(ctx, apiclient.Request{Method: method, Path: path, Query: q, Body: body, Create: method != http.MethodGet}, out)
	return err
}

// ListFileSystems reads a page of the project's file systems, newest first.
func (c *Client) ListFileSystems(ctx context.Context, orgID, projectID string, limit int, cursor string) (apiclient.Page[FileSystem], error) {
	var out fileSystemPage
	err := c.do(ctx, http.MethodGet, projectPath(orgID, projectID)+"/file-systems", pageQuery(limit, cursor), nil, &out)
	return apiclient.Page[FileSystem]{Items: out.FileSystems, NextCursor: out.NextCursor}, err
}

// GetFileSystem reads a file system.
func (c *Client) GetFileSystem(ctx context.Context, orgID, projectID, fileSystem string) (*FileSystem, error) {
	var out FileSystem
	return &out, c.do(ctx, http.MethodGet, projectPath(orgID, projectID)+p("/file-systems/{}", fileSystem), nil, nil, &out)
}

// CreateFileSystem starts creating a file system, and answers the operation.
func (c *Client) CreateFileSystem(ctx context.Context, orgID, projectID string, body FileSystemCreate) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodPost, projectPath(orgID, projectID)+"/file-systems", nil, body, &out)
}

// UpdateFileSystem changes a file system, and answers it changed.
func (c *Client) UpdateFileSystem(ctx context.Context, orgID, projectID, fileSystem string, body FileSystemUpdate) (*FileSystem, error) {
	var out FileSystem
	return &out, c.do(ctx, http.MethodPatch, projectPath(orgID, projectID)+p("/file-systems/{}", fileSystem), nil, body, &out)
}

// DeleteFileSystem starts deleting a file system and its files (409 file_system_in_use while it
// has mount targets or access points).
func (c *Client) DeleteFileSystem(ctx context.Context, orgID, projectID, fileSystem string) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodDelete, projectPath(orgID, projectID)+p("/file-systems/{}", fileSystem), nil, nil, &out)
}

// ListMountTargets reads a page of the project's mount targets; fileSystem (an fsy_ ID) keeps one
// file system's.
func (c *Client) ListMountTargets(ctx context.Context, orgID, projectID, fileSystem string, limit int, cursor string) (apiclient.Page[MountTarget], error) {
	q := pageQuery(limit, cursor)
	if fileSystem != "" {
		q.Set("fileSystem", fileSystem)
	}
	var out mountTargetPage
	err := c.do(ctx, http.MethodGet, projectPath(orgID, projectID)+"/mount-targets", q, nil, &out)
	return apiclient.Page[MountTarget]{Items: out.MountTargets, NextCursor: out.NextCursor}, err
}

// GetMountTarget reads a mount target.
func (c *Client) GetMountTarget(ctx context.Context, orgID, projectID, mountTarget string) (*MountTarget, error) {
	var out MountTarget
	return &out, c.do(ctx, http.MethodGet, projectPath(orgID, projectID)+p("/mount-targets/{}", mountTarget), nil, nil, &out)
}

// CreateMountTarget starts creating a mount target.
func (c *Client) CreateMountTarget(ctx context.Context, orgID, projectID string, body MountTargetCreate) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodPost, projectPath(orgID, projectID)+"/mount-targets", nil, body, &out)
}

// UpdateMountTarget changes a mount target, and answers it changed.
func (c *Client) UpdateMountTarget(ctx context.Context, orgID, projectID, mountTarget string, body MountTargetUpdate) (*MountTarget, error) {
	var out MountTarget
	return &out, c.do(ctx, http.MethodPatch, projectPath(orgID, projectID)+p("/mount-targets/{}", mountTarget), nil, body, &out)
}

// DeleteMountTarget starts deleting a mount target; the file system's files stay.
func (c *Client) DeleteMountTarget(ctx context.Context, orgID, projectID, mountTarget string) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodDelete, projectPath(orgID, projectID)+p("/mount-targets/{}", mountTarget), nil, nil, &out)
}

// ListAccessPoints reads a page of the project's access points; fileSystem (an fsy_ ID) keeps one
// file system's.
func (c *Client) ListAccessPoints(ctx context.Context, orgID, projectID, fileSystem string, limit int, cursor string) (apiclient.Page[AccessPoint], error) {
	q := pageQuery(limit, cursor)
	if fileSystem != "" {
		q.Set("fileSystem", fileSystem)
	}
	var out accessPointPage
	err := c.do(ctx, http.MethodGet, projectPath(orgID, projectID)+"/access-points", q, nil, &out)
	return apiclient.Page[AccessPoint]{Items: out.AccessPoints, NextCursor: out.NextCursor}, err
}

// GetAccessPoint reads an access point.
func (c *Client) GetAccessPoint(ctx context.Context, orgID, projectID, accessPoint string) (*AccessPoint, error) {
	var out AccessPoint
	return &out, c.do(ctx, http.MethodGet, projectPath(orgID, projectID)+p("/access-points/{}", accessPoint), nil, nil, &out)
}

// CreateAccessPoint starts creating an access point (and its root directory, with creationInfo).
func (c *Client) CreateAccessPoint(ctx context.Context, orgID, projectID string, body AccessPointCreate) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodPost, projectPath(orgID, projectID)+"/access-points", nil, body, &out)
}

// UpdateAccessPoint changes an access point's name, description or tags.
func (c *Client) UpdateAccessPoint(ctx context.Context, orgID, projectID, accessPoint string, body AccessPointUpdate) (*AccessPoint, error) {
	var out AccessPoint
	return &out, c.do(ctx, http.MethodPatch, projectPath(orgID, projectID)+p("/access-points/{}", accessPoint), nil, body, &out)
}

// DeleteAccessPoint starts deleting an access point; its files stay.
func (c *Client) DeleteAccessPoint(ctx context.Context, orgID, projectID, accessPoint string) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodDelete, projectPath(orgID, projectID)+p("/access-points/{}", accessPoint), nil, nil, &out)
}

// ListOperations reads a page of the organization's Files operations, newest first; status and
// targetResourceName narrow it when set.
func (c *Client) ListOperations(ctx context.Context, orgID, status, targetResourceName string, limit int, cursor string) (apiclient.Page[Operation], error) {
	q := pageQuery(limit, cursor)
	if status != "" {
		q.Set("status", status)
	}
	if targetResourceName != "" {
		q.Set("targetResourceName", targetResourceName)
	}
	var out operationPage
	err := c.do(ctx, http.MethodGet, p("/v1/organizations/{}/operations", orgID), q, nil, &out)
	return apiclient.Page[Operation]{Items: out.Operations, NextCursor: out.NextCursor}, err
}

// GetOperation reads an operation.
func (c *Client) GetOperation(ctx context.Context, orgID, id string) (*Operation, error) {
	var out Operation
	return &out, c.do(ctx, http.MethodGet, p("/v1/organizations/{}/operations/{}", orgID, id), nil, nil, &out)
}

// CancelOperation asks an operation to stop (409 operation_finished or operation_not_cancellable).
func (c *Client) CancelOperation(ctx context.Context, orgID, id string) (*Operation, error) {
	var out Operation
	_, err := c.API.Do(ctx, apiclient.Request{Method: http.MethodDelete, Path: p("/v1/organizations/{}/operations/{}", orgID, id)}, &out)
	return &out, err
}
