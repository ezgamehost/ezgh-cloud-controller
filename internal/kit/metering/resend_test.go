package metering_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/metering"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// contracts/metering.md, "Watermarks": after correcting an hour that already had its watermark,
// the producer sends that hour's watermark again with the new counts; and withdrawing a deleted
// organization's unsendable usage changes the counts the same way.

func watermarks(t *testing.T, d *testdb.DB, url string) *metering.Watermarks {
	t.Helper()
	return &metering.Watermarks{
		DB: d.DB, Client: &metering.Client{URL: url, Credential: credential}, Producer: "widgets", Region: "us-west-1",
		Meters: []string{"widgets.widget.storage", "widgets.widget.uptime"}, Logger: testdb.Logger(),
	}
}

func uptimeCounts(t *testing.T, body map[string]any) (float64, string) {
	t.Helper()
	u := body["counts"].(map[string]any)["widgets.widget.uptime"].(map[string]any)
	return u["events"].(float64), u["quantity"].(string)
}

func TestWatermarksResendAnHourWhoseTotalsChanged(t *testing.T) {
	d := testdb.New(t)
	p := producer(t)
	hour := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	if _, err := insert(t, p, d.DB, uptime(t, hour, "3600")); err != nil {
		t.Fatal(err)
	}
	run(t, d, `DELETE FROM usage_outbox`)
	rec := &ingestRecorder{}
	server := httptest.NewServer(rec)
	defer server.Close()
	w := watermarks(t, d, server.URL)
	run(t, d, `INSERT INTO usage_watermarks (region, complete_through) VALUES ('us-west-1', $1)`, hour)
	now := hour.Add(2*time.Hour + 5*time.Minute)
	if n, err := w.Once(context.Background(), now); err != nil || n != 2 {
		t.Fatalf("sent %d, %v", n, err)
	}
	// Nothing changed: checking again sends nothing.
	if n, err := w.Once(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("re-sent %d unchanged, %v", n, err)
	}
	// A late interval for 10:00-11:00, after its watermark went out.
	if _, err := insert(t, p, d.DB, uptime(t, hour.Add(30*time.Minute), "100")); err != nil {
		t.Fatal(err)
	}
	// Not while it's still in the outbox: ingest would compare before it arrived.
	if n, err := w.Once(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("re-sent %d with the change pending, %v", n, err)
	}
	run(t, d, `DELETE FROM usage_outbox`)
	if n, err := w.Once(context.Background(), now); err != nil || n != 1 {
		t.Fatalf("re-sent %d, %v", n, err)
	}
	last := rec.bodies[len(rec.bodies)-1]
	if last["completeThrough"] != "2026-09-28T11:00:00Z" {
		t.Fatalf("re-sent %v", last)
	}
	if events, quantity := uptimeCounts(t, last); events != 2 || quantity != "3700" {
		t.Fatalf("re-sent counts %v %v", events, quantity)
	}
	// The watermark itself never moved back.
	var through time.Time
	if err := d.Pool.QueryRow(context.Background(), `SELECT complete_through FROM usage_watermarks`).Scan(&through); err != nil || !through.Equal(hour.Add(2*time.Hour)) {
		t.Fatalf("watermark %v %v", through, err)
	}
	if n, err := w.Once(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("re-sent %d again, %v", n, err)
	}
}

// Hours watermarked before the kit kept per-hour sends (control totals' changed_at -infinity)
// are never re-sent on upgrade.
func TestWatermarksDontResendHoursFromBeforeTheUpgrade(t *testing.T) {
	d := testdb.New(t)
	hour := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	run(t, d, `INSERT INTO usage_control_totals (usage_hour, meter, events, quantity) VALUES ($1, 'widgets.widget.uptime', 3, 10)`, hour)
	run(t, d, `INSERT INTO usage_watermarks (region, complete_through) VALUES ('us-west-1', $1)`, hour.Add(time.Hour))
	rec := &ingestRecorder{}
	server := httptest.NewServer(rec)
	defer server.Close()
	if n, err := watermarks(t, d, server.URL).Once(context.Background(), hour.Add(time.Hour+5*time.Minute)); err != nil || n != 0 || len(rec.bodies) != 0 {
		t.Fatalf("sent %d (%d posts), %v", n, len(rec.bodies), err)
	}
}

// Withdraw drops an organization's usage that can't be sent any more (its organization was
// deleted: ingest refuses it whatever its time) from the outbox and the control totals, so the
// watermark's counts are what ingest has; the affected hours are re-sent.
func TestWithdrawDropsAnOrganizationsUnsendableUsage(t *testing.T) {
	d := testdb.New(t)
	p := producer(t)
	hour := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	for _, e := range []struct {
		at  time.Time
		qty string
	}{{hour, "60"}, {hour.Add(time.Minute), "30"}, {hour.Add(2 * time.Minute), "10"}} {
		if _, err := insert(t, p, d.DB, uptime(t, e.at, e.qty)); err != nil {
			t.Fatal(err)
		}
	}
	// One is still pending, one was refused as organization_closed, one for another reason.
	closedID, _ := uptimeID(t, hour.Add(time.Minute))
	otherID, _ := uptimeID(t, hour.Add(2*time.Minute))
	run(t, d, `UPDATE usage_outbox SET rejected_at = now(), last_error = $2 WHERE id = $1`, closedID,
		`[{"path":"organizationId","message":"organization_closed: the organization is closed"}]`)
	run(t, d, `UPDATE usage_outbox SET rejected_at = now(), last_error = $2 WHERE id = $1`, otherID,
		`[{"path":"quantity","message":"must be a decimal"}]`)
	rec := &ingestRecorder{}
	server := httptest.NewServer(rec)
	defer server.Close()
	w := watermarks(t, d, server.URL)
	run(t, d, `INSERT INTO usage_watermarks (region, complete_through) VALUES ('us-west-1', $1)`, hour.Add(time.Hour))
	run(t, d, `INSERT INTO usage_watermark_hours (region, complete_through, counts, sent_at, checked_at) VALUES ('us-west-1', $1, '{}', now(), now())`, hour.Add(time.Hour))

	var got metering.Withdrawn
	if err := d.DB.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error {
		var err error
		got, err = metering.Withdraw(context.Background(), tx, org, "organization_closed", "unknown_project")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.Events != 2 || got.Counts["widgets.widget.uptime"] != (metering.Counts{Events: 2, Quantity: "90"}) {
		t.Fatalf("withdrew %+v", got)
	}
	var left []string
	rows, _ := d.Pool.Query(context.Background(), `SELECT id FROM usage_outbox`)
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		left = append(left, id)
	}
	rows.Close()
	if len(left) != 1 || left[0] != otherID {
		t.Fatalf("left %v", left)
	}
	// Withdrawing again finds nothing.
	if err := d.DB.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error {
		again, err := metering.Withdraw(context.Background(), tx, org, "organization_closed")
		if again.Events != 0 {
			t.Errorf("withdrew again %+v", again)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The hour's watermark is re-sent with what's left: the other event, kept for a person.
	if n, err := w.Once(context.Background(), hour.Add(time.Hour+5*time.Minute)); err != nil || n != 1 {
		t.Fatalf("re-sent %d, %v", n, err)
	}
	if events, quantity := uptimeCounts(t, rec.bodies[0]); events != 1 || quantity != "10" {
		t.Fatalf("re-sent counts %v %v", events, quantity)
	}
}

func uptimeID(t *testing.T, start time.Time) (string, error) {
	t.Helper()
	return uptime(t, start, "1").ID, nil
}

func run(t *testing.T, d *testdb.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.Pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}
