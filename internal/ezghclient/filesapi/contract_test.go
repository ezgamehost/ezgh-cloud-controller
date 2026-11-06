package filesapi

import (
	"context"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/openapitest"
)

// The client against files-api's own OpenAPI document (api/openapi/files-api.json), built from
// files-stack main (35c748a)'s manifest and overlay until the service is live for scripts/openapi.sh.
func TestClientMatchesFilesAPIsOpenAPI(t *testing.T) {
	spec := openapitest.Load(t, "../openapi/files-api.json")
	rec := openapitest.NewRecorder(t)
	c := &Client{API: &apiclient.Client{Service: "Files", BaseURL: rec.URL, HTTP: rec.Client()}}
	ctx := context.Background()
	o, pr := "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	fs, mt, ap, op := "fsy_k2m9x0a7qpe4", "fmt_k2m9x0a7qpe4", "fap_k2m9x0a7qpe4", "0199a3c2-7000-7000-8000-000000000001"
	text, yes := "x", true
	tags := map[string]string{"team": "a"}
	cidrs := []string{"10.0.0.0/16"}
	openapitest.Check(t, spec, rec, []openapitest.Call{
		{Name: "ListFileSystems", Run: func() error { _, err := c.ListFileSystems(ctx, o, pr, 20, "c"); return err }, Response: fileSystemPage{}},
		{Name: "GetFileSystem", Run: func() error { _, err := c.GetFileSystem(ctx, o, pr, fs); return err }, Response: FileSystem{}},
		{Name: "CreateFileSystem", Run: func() error {
			_, err := c.CreateFileSystem(ctx, o, pr, FileSystemCreate{Name: "web", Description: &text, Tags: tags, VPC: "vpc_k2m9x0a7qpe4", RootSquash: &yes, RequireTLS: &yes, DeletionProtection: &yes})
			return err
		}, Request: FileSystemCreate{}, Response: Operation{}},
		{Name: "UpdateFileSystem", Run: func() error {
			_, err := c.UpdateFileSystem(ctx, o, pr, fs, FileSystemUpdate{Name: &text, Description: &text, Tags: &tags, RootSquash: &yes, RequireTLS: &yes, DeletionProtection: &yes})
			return err
		}, Request: FileSystemUpdate{}, Response: FileSystem{}},
		{Name: "DeleteFileSystem", Run: func() error { _, err := c.DeleteFileSystem(ctx, o, pr, fs); return err }, Response: Operation{}},
		{Name: "ListMountTargets", Run: func() error { _, err := c.ListMountTargets(ctx, o, pr, fs, 20, "c"); return err }, Response: mountTargetPage{}},
		{Name: "GetMountTarget", Run: func() error { _, err := c.GetMountTarget(ctx, o, pr, mt); return err }, Response: MountTarget{}},
		{Name: "CreateMountTarget", Run: func() error {
			_, err := c.CreateMountTarget(ctx, o, pr, MountTargetCreate{FileSystem: fs, Subnet: "sbn_k2m9x0a7qpe4", Name: &text, AllowedSourceCidrs: cidrs, Tags: tags})
			return err
		}, Request: MountTargetCreate{}, Response: Operation{}},
		{Name: "UpdateMountTarget", Run: func() error {
			_, err := c.UpdateMountTarget(ctx, o, pr, mt, MountTargetUpdate{Name: &text, AllowedSourceCidrs: cidrs, Tags: &tags})
			return err
		}, Request: MountTargetUpdate{}, Response: MountTarget{}},
		{Name: "DeleteMountTarget", Run: func() error { _, err := c.DeleteMountTarget(ctx, o, pr, mt); return err }, Response: Operation{}},
		{Name: "ListAccessPoints", Run: func() error { _, err := c.ListAccessPoints(ctx, o, pr, fs, 20, "c"); return err }, Response: accessPointPage{}},
		{Name: "GetAccessPoint", Run: func() error { _, err := c.GetAccessPoint(ctx, o, pr, ap); return err }, Response: AccessPoint{}},
		{Name: "CreateAccessPoint", Run: func() error {
			_, err := c.CreateAccessPoint(ctx, o, pr, AccessPointCreate{FileSystem: fs, Name: "wp", Description: &text, Tags: tags,
				RootDirectory: &RootDirectoryInput{Path: "/wp", CreationInfo: &CreationInfo{OwnerUID: 33, OwnerGID: 33, Permissions: "755"}},
				PosixUser:     &PosixUserInput{UID: 33, GID: 33}})
			return err
		}, Request: AccessPointCreate{}, Response: Operation{}},
		{Name: "UpdateAccessPoint", Run: func() error {
			_, err := c.UpdateAccessPoint(ctx, o, pr, ap, AccessPointUpdate{Name: &text, Description: &text, Tags: &tags})
			return err
		}, Request: AccessPointUpdate{}, Response: AccessPoint{}},
		{Name: "DeleteAccessPoint", Run: func() error { _, err := c.DeleteAccessPoint(ctx, o, pr, ap); return err }, Response: Operation{}},
		{Name: "ListOperations", Run: func() error { _, err := c.ListOperations(ctx, o, "running", "rn", 20, "c"); return err }, Response: operationPage{}},
		{Name: "GetOperation", Run: func() error { _, err := c.GetOperation(ctx, o, op); return err }, Response: Operation{}},
		{Name: "CancelOperation", Run: func() error { _, err := c.CancelOperation(ctx, o, op); return err }, Response: Operation{}},
	}, nil)
}
