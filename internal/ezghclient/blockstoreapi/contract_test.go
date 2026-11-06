package blockstoreapi

import (
	"context"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/openapitest"
)

// The client against blockstore-api's own OpenAPI document (api/openapi/blockstore-api.json).
func TestClientMatchesBlockstoreAPIsOpenAPI(t *testing.T) {
	spec := openapitest.Load(t, "../openapi/blockstore-api.json")
	rec := openapitest.NewRecorder(t)
	c := &Client{API: &apiclient.Client{Service: "Block Storage", BaseURL: rec.URL, HTTP: rec.Client()}}
	ctx := context.Background()
	o, p := "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	vol, snp, op := "vol_k2m9x0a7qpe4", "snp_k2m9x0a7qpe4", "0199a3c2-7000-7000-8000-000000000001"
	n, yes, name, tags := 3000, true, "data", map[string]string{"team": "a"}
	openapitest.Check(t, spec, rec, []openapitest.Call{
		{Name: "ListVolumes", Run: func() error {
			_, err := c.ListVolumes(ctx, o, p, "available", "d0", "ins_k2m9x0a7qpe4", 20, "c")
			return err
		}, Response: volumePage{}},
		{Name: "GetVolume", Run: func() error { _, err := c.GetVolume(ctx, o, p, vol); return err }, Response: Volume{}},
		{Name: "CreateVolume", Run: func() error {
			_, err := c.CreateVolume(ctx, o, p, VolumeCreate{Name: "data", VolumeType: "d0", SizeBytes: "10737418240", IOPS: &n, ThroughputMBps: &n,
				Source: CreateSource{Kind: "image", Image: "linux/ubuntu:24.04", Architecture: "amd64", SnapshotID: snp}, DeletionProtection: &yes, Tags: tags})
			return err
		}, Request: VolumeCreate{}, Response: VolumeOperation{}},
		{Name: "UpdateVolume", Run: func() error {
			_, err := c.UpdateVolume(ctx, o, p, vol, VolumeUpdate{Name: &name, DeletionProtection: &yes, Tags: &tags})
			return err
		}, Request: VolumeUpdate{}, Response: Volume{}},
		{Name: "ModifyVolume", Run: func() error {
			_, err := c.ModifyVolume(ctx, o, p, vol, VolumeModify{VolumeType: "d0", SizeBytes: "21474836480", IOPS: &n, ThroughputMBps: &n})
			return err
		}, Request: VolumeModify{}, Response: VolumeOperation{}},
		{Name: "DeleteVolume", Run: func() error { _, err := c.DeleteVolume(ctx, o, p, vol); return err }, Response: VolumeOperation{}},
		{Name: "ListSnapshots", Run: func() error { _, err := c.ListSnapshots(ctx, o, p, "available", vol, 20, "c"); return err }, Response: snapshotPage{}},
		{Name: "GetSnapshot", Run: func() error { _, err := c.GetSnapshot(ctx, o, p, snp); return err }, Response: Snapshot{}},
		{Name: "CreateSnapshot", Run: func() error {
			_, err := c.CreateSnapshot(ctx, o, p, SnapshotCreate{VolumeID: vol, Name: "nightly", Tags: tags})
			return err
		}, Request: SnapshotCreate{}, Response: SnapshotOperation{}},
		{Name: "UpdateSnapshot", Run: func() error {
			_, err := c.UpdateSnapshot(ctx, o, p, snp, SnapshotUpdate{Name: &name, Tags: &tags})
			return err
		}, Request: SnapshotUpdate{}, Response: Snapshot{}},
		{Name: "DeleteSnapshot", Run: func() error { _, err := c.DeleteSnapshot(ctx, o, p, snp); return err }, Response: SnapshotOperation{}},
		{Name: "ListOrganizationVolumeTypes", Run: func() error { _, err := c.ListVolumeTypes(ctx, o); return err }, Response: volumeTypeList{}},
		{Name: "ListOperations", Run: func() error {
			_, err := c.ListOperations(ctx, o, "running", "ezgh:us-west-1:org_a:prj_b:vol_k2m9x0a7qpe4", 20, "c")
			return err
		}, Response: operationPage{}},
		{Name: "GetOperation", Run: func() error { _, err := c.GetOperation(ctx, o, op); return err }, Response: Operation{}},
		{Name: "CancelOperation", Run: func() error { _, err := c.CancelOperation(ctx, o, op); return err }, Response: Operation{}},
	}, nil)
}
