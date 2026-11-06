package metering_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/metering"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/usage"
)

const (
	org     = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	project = "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	widget  = "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:wgt_4k2m9x0a7qpe"
)

func producer(t *testing.T) *metering.Producer {
	t.Helper()
	registry, err := usage.ParseRegistry(vectors.File(t, "examples/widgets.meter-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &metering.Producer{Name: "widgets", Region: "us-west-1", Regions: []string{"us-west-1"}, Meters: registry}
}

func uptime(t *testing.T, start time.Time, seconds string) *usage.Event {
	t.Helper()
	id, err := usage.IntervalID(widget, "widgets.widget.uptime", start)
	if err != nil {
		t.Fatal(err)
	}
	p := project
	return &usage.Event{
		ID: id, Producer: "widgets", Meter: "widgets.widget.uptime", OrganizationID: org, ProjectID: &p,
		ResourceName: widget, Region: "us-west-1",
		Window:   &usage.EventWindow{Start: usage.IDTime(start), End: usage.IDTime(start.Truncate(time.Hour).Add(time.Hour))},
		Quantity: seconds, Dimensions: map[string]string{"size": "small"},
	}
}

func insert(t *testing.T, p *metering.Producer, pool interface {
	RunTx(context.Context, *sql.TxOptions, func(*kitdb.Tx) error) error
}, e *usage.Event) (bool, error) {
	t.Helper()
	var inserted bool
	err := pool.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error {
		var err error
		inserted, err = p.Insert(context.Background(), tx, e)
		return err
	})
	if err != nil {
		return false, err
	}
	return inserted, nil
}

// An event is written once per ID, and control totals count only rows actually inserted.
func TestInsertIsIdempotentAndCounts(t *testing.T) {
	db := testdb.New(t)
	p := producer(t)
	hour := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	for i := range 2 {
		inserted, err := insert(t, p, db.DB, uptime(t, hour, "3600"))
		if err != nil || inserted != (i == 0) {
			t.Fatalf("pass %d: inserted %v, %v", i, inserted, err)
		}
	}
	if _, err := insert(t, p, db.DB, uptime(t, hour.Add(-30*time.Minute), "1800")); err != nil {
		t.Fatal(err)
	}
	counts, err := metering.ControlTotals(context.Background(), db.DB, hour, []string{"widgets.widget.uptime", "widgets.widget.storage"})
	if err != nil {
		t.Fatal(err)
	}
	if c := counts["widgets.widget.uptime"]; c.Events != 1 || c.Quantity != "3600" {
		t.Errorf("uptime %+v", c)
	}
	if c := counts["widgets.widget.storage"]; c.Events != 0 || c.Quantity != "0" {
		t.Errorf("storage %+v", c)
	}
	// Stored as a JSON object, not a string inside jsonb.
	var kind string
	_ = db.Pool.QueryRow(context.Background(), `SELECT jsonb_typeof(event) FROM usage_outbox LIMIT 1`).Scan(&kind)
	if kind != "object" {
		t.Errorf("event stored as %s", kind)
	}
}

// An invalid event never commits.
func TestInsertRefusesInvalidEvents(t *testing.T) {
	db := testdb.New(t)
	e := uptime(t, time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC), "3600")
	e.Dimensions = map[string]string{"size": "medium"}
	_, err := insert(t, producer(t), db.DB, e)
	var invalid *metering.InvalidEvent
	if !errors.As(err, &invalid) || invalid.Issues[0].Code() != "invalid_dimension" {
		t.Fatalf("got %v", err)
	}
	if left := rows(t, db.Pool); len(left) != 0 {
		t.Error("an invalid event was written")
	}
}

type ingestRecorder struct {
	mu     sync.Mutex
	paths  []string
	bodies []map[string]any
	status int
}

func (r *ingestRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.bodies = append(r.bodies, body)
	r.mu.Unlock()
	if r.status != 0 {
		w.WriteHeader(r.status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Watermarks go out oldest first, once the hour's own work is done and the outbox holds nothing
// pending from before it, with that hour's control totals; the watermark never moves back.
func TestWatermarks(t *testing.T) {
	db := testdb.New(t)
	p := producer(t)
	hour := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	if _, err := insert(t, p, db.DB, uptime(t, hour, "3600")); err != nil {
		t.Fatal(err)
	}
	rec := &ingestRecorder{}
	server := httptest.NewServer(rec)
	defer server.Close()
	ready := true
	w := &metering.Watermarks{
		DB: db.DB, Client: &metering.Client{URL: server.URL, Credential: credential}, Producer: "widgets", Region: "us-west-1",
		Meters: []string{"widgets.widget.storage", "widgets.widget.uptime"}, Logger: testdb.Logger(),
		Ready: func(context.Context, time.Time) (bool, error) { return ready, nil },
	}
	// Seed the last one sent: 10:00. Now it's 12:05, so 11:00 and 12:00 are due.
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO usage_watermarks (region, complete_through) VALUES ('us-west-1', $1)`, hour); err != nil {
		t.Fatal(err)
	}
	now := hour.Add(2*time.Hour + 5*time.Minute)
	// The 10:00 event is still in the outbox: nothing is complete through 11:00 yet.
	if n, err := w.Once(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("sent %d, %v with usage pending", n, err)
	}
	if _, err := db.Pool.Exec(context.Background(), `DELETE FROM usage_outbox`); err != nil {
		t.Fatal(err)
	}
	ready = false
	if n, _ := w.Once(context.Background(), now); n != 0 {
		t.Fatal("sent before the hour's work was done")
	}
	ready = true
	if n, err := w.Once(context.Background(), now); err != nil || n != 2 {
		t.Fatalf("sent %d, %v", n, err)
	}
	first := rec.bodies[0]
	if first["completeThrough"] != "2026-09-28T11:00:00Z" || rec.paths[0] != "/internal/v1/usage/watermarks" {
		t.Errorf("first watermark %v", first)
	}
	counts := first["counts"].(map[string]any)
	if u := counts["widgets.widget.uptime"].(map[string]any); u["events"] != float64(1) || u["quantity"] != "3600" {
		t.Errorf("counts %v", counts)
	}
	if s := counts["widgets.widget.storage"].(map[string]any); s["events"] != float64(0) {
		t.Errorf("counts %v", counts)
	}
	if rec.bodies[1]["completeThrough"] != "2026-09-28T12:00:00Z" {
		t.Errorf("second %v", rec.bodies[1])
	}
	if n, _ := w.Once(context.Background(), now); n != 0 {
		t.Error("sent again")
	}
}

// A closure goes out once the organization's usage has all been relayed.
func TestClosures(t *testing.T) {
	db := testdb.New(t)
	p := producer(t)
	at := time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC)
	if _, err := insert(t, p, db.DB, uptime(t, at.Add(-30*time.Minute), "1800")); err != nil {
		t.Fatal(err)
	}
	err := db.DB.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error {
		return metering.MarkClosurePending(context.Background(), tx, org, at)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := &ingestRecorder{}
	server := httptest.NewServer(rec)
	defer server.Close()
	c := &metering.Closures{DB: db.DB, Client: &metering.Client{URL: server.URL, Credential: credential}, Producer: "widgets", Logger: testdb.Logger()}
	if n, _ := c.Once(context.Background()); n != 0 {
		t.Fatal("sent with usage pending")
	}
	_, _ = db.Pool.Exec(context.Background(), `DELETE FROM usage_outbox`)
	if n, err := c.Once(context.Background()); err != nil || n != 1 {
		t.Fatalf("sent %d, %v", n, err)
	}
	if b := rec.bodies[0]; b["organizationId"] != org || b["completeThrough"] != "2026-09-28T10:30:00Z" || b["producer"] != "widgets" {
		t.Errorf("closure %v", b)
	}
	if n, _ := c.Once(context.Background()); n != 0 {
		t.Error("sent twice")
	}
}
