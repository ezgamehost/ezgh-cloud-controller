package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// PurgeIdempotencyKeys deletes every expired key, in batches (no one statement deletes a whole
// day's keys of a busy product), and keeps the live ones.
func TestPurgeIdempotencyKeysInBatches(t *testing.T) {
	d := testdb.New(t).DB
	ctx := context.Background()
	expired := service.PurgeBatch*2 + 7
	if _, err := d.ExecContext(ctx, `INSERT INTO idempotency_keys (organization_id, key, fingerprint, status, created_at)
		SELECT $1::uuid, 'old-' || i, 'f', 200, now() - interval '49 hours' FROM generate_series(1, $2::int) AS i
		UNION ALL SELECT $1::uuid, 'new-' || i, 'f', 200, now() FROM generate_series(1, 3) AS i`, orgID, expired); err != nil {
		t.Fatal(err)
	}
	n, err := service.PurgeIdempotencyKeys(ctx, d, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(expired) {
		t.Fatalf("purged %d, want %d", n, expired)
	}
	var left int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 3 {
		t.Fatalf("%d keys left, want the 3 live ones", left)
	}
	// In a transaction (a caller's), the same.
	if err := d.RunTx(ctx, nil, func(tx *db.Tx) error {
		_, err := service.PurgeIdempotencyKeys(ctx, tx, 48*time.Hour)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
