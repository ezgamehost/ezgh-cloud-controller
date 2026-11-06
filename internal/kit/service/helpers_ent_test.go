package service_test

import (
	"context"
	"errors"
	"testing"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// The kit's helpers for products' purge and replay checks: quota.DeleteOrganization,
// service.DeleteIdempotencyKeys and service.IdempotencyKeyExists. Each touches one organization's
// rows only, and the deletes are part of the caller's transaction.

func storeIdempotencyKey(t *testing.T, d *testdb.DB, org, key string) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), `
		INSERT INTO idempotency_keys (organization_id, key, fingerprint, status, body)
		VALUES ($1, $2, 'fp', 201, '{}')`, org, key); err != nil {
		t.Fatal(err)
	}
}

func idempotencyKeysOf(t *testing.T, d *testdb.DB, org string) int {
	t.Helper()
	var n int
	if err := d.Pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys WHERE organization_id = $1`, org).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIdempotencyKeyExists(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	a, b := ids.NewV7(), ids.NewV7()
	storeIdempotencyKey(t, d, a, "k1")

	for _, tc := range []struct {
		org, key string
		want     bool
	}{
		{a, "k1", true},
		{a, "k2", false},
		{b, "k1", false}, // another organization's key is not this one's
		{a, "K1", false}, // keys are compared exactly
	} {
		got, err := service.IdempotencyKeyExists(ctx, d.DB, tc.org, tc.key)
		if err != nil || got != tc.want {
			t.Errorf("IdempotencyKeyExists(%s, %q) on the pool = %v, %v; want %v", tc.org, tc.key, got, err, tc.want)
		}
	}

	// Inside a transaction it sees the transaction's own writes.
	err := d.DB.RunTx(ctx, nil, func(tx *kitdb.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (organization_id, key, fingerprint, status) VALUES ($1, 'k2', 'fp', 204)`, a); err != nil {
			return err
		}
		got, err := service.IdempotencyKeyExists(ctx, tx, a, "k2")
		if err != nil || !got {
			t.Errorf("IdempotencyKeyExists in tx = %v, %v; want true", got, err)
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if got, err := service.IdempotencyKeyExists(ctx, d.DB, a, "k2"); err != nil || got {
		t.Errorf("after rollback IdempotencyKeyExists = %v, %v; want false", got, err)
	}
}

var errRollback = errors.New("roll back")

func TestDeleteIdempotencyKeys(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	a, b := ids.NewV7(), ids.NewV7()
	storeIdempotencyKey(t, d, a, "k1")
	storeIdempotencyKey(t, d, a, "k2")
	storeIdempotencyKey(t, d, b, "k1")

	// A rolled-back purge deletes nothing.
	err := d.DB.RunTx(ctx, nil, func(tx *kitdb.Tx) error {
		if err := service.DeleteIdempotencyKeys(ctx, tx, a); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if n := idempotencyKeysOf(t, d, a); n != 2 {
		t.Fatalf("after rollback organization a has %d keys, want 2", n)
	}

	if err := d.DB.RunTx(ctx, nil, func(tx *kitdb.Tx) error { return service.DeleteIdempotencyKeys(ctx, tx, a) }); err != nil {
		t.Fatal(err)
	}
	if n := idempotencyKeysOf(t, d, a); n != 0 {
		t.Errorf("organization a has %d keys after the delete, want 0", n)
	}
	if n := idempotencyKeysOf(t, d, b); n != 1 {
		t.Errorf("organization b has %d keys after a's delete, want 1", n)
	}
	// Deleting an organization with no keys is not an error.
	if err := d.DB.RunTx(ctx, nil, func(tx *kitdb.Tx) error { return service.DeleteIdempotencyKeys(ctx, tx, a) }); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaDeleteOrganization(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	a, b := ids.NewV7(), ids.NewV7()
	q := manifest.Quota{ID: "widgets.thingsPerOrganization", Kind: manifest.Allocation}
	perProject := manifest.Quota{ID: "widgets.thingsPerProject", Kind: manifest.Allocation}
	project := ids.NewV7()
	reserve := func(c quota.Counter, n int64) {
		t.Helper()
		if err := d.DB.RunTx(ctx, nil, func(tx *kitdb.Tx) error { return quota.Reserve(ctx, tx, c, n, quota.Unlimited) }); err != nil {
			t.Fatal(err)
		}
	}
	reserve(quota.Counter{Quota: q, OrganizationID: a, ScopeKey: a}, 2)
	reserve(quota.Counter{Quota: perProject, OrganizationID: a, ScopeKey: project, Region: "us-west-1"}, 1)
	reserve(quota.Counter{Quota: q, OrganizationID: b, ScopeKey: b}, 3)

	err := d.DB.RunTx(ctx, nil, func(tx *kitdb.Tx) error {
		if err := quota.DeleteOrganization(ctx, tx, a); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if rows, err := quota.UsageOf(ctx, d.DB, a); err != nil || len(rows) != 2 {
		t.Fatalf("after rollback UsageOf(a) = %v, %v; want 2 rows", rows, err)
	}

	if err := d.DB.RunTx(ctx, nil, func(tx *kitdb.Tx) error { return quota.DeleteOrganization(ctx, tx, a) }); err != nil {
		t.Fatal(err)
	}
	rows, err := quota.UsageOf(ctx, d.DB, a)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Errorf("UsageOf(a) after the delete = %#v, %v; want an empty, non-nil list", rows, err)
	}
	if used, err := quota.Used(ctx, d.DB, q.ID, b, ""); err != nil || used != 3 {
		t.Errorf("organization b's counter after a's delete = %d, %v; want 3", used, err)
	}
	// Counting again after the purge starts from zero.
	reserve(quota.Counter{Quota: q, OrganizationID: a, ScopeKey: a}, 1)
	if used, err := quota.Used(ctx, d.DB, q.ID, a, ""); err != nil || used != 1 {
		t.Errorf("organization a's counter after a new reserve = %d, %v; want 1", used, err)
	}
}
