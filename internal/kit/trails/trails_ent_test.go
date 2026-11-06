package trails_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/outbox"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

const entOrg = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"

func entEvent(id string) *trails.Event {
	ua := "kit-test/1.0"
	project := "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f71"
	return &trails.Event{
		EventVersion:   "1.0",
		EventID:        id,
		EventTime:      "2026-10-01T12:00:00.123Z",
		EventType:      trails.APICall,
		EventCategory:  "Management",
		EventSource:    "widgets.ezghcloud.com",
		EventName:      "CreateWidget",
		OrganizationID: entOrg,
		ProjectID:      &project,
		UserIdentity:   trails.ServiceIdentity("organizations.ezghcloud.com"),
		UserAgent:      &ua,
		// HTML-looking text and unicode stay unescaped, as httpapi.Marshal writes them.
		RequestParameters: map[string]any{"name": `a <b> & "c" é`, "nested": map[string]any{"n": 1.5, "list": []any{}}},
		ResponseElements:  map[string]any{"id": "w-1"},
		Resources:         []trails.Resource{{ResourceName: "ezgrn:widgets:::" + entOrg + ":widget/w-1", Type: "Widget"}},
	}
}

// stored reads the row Insert wrote, and what the pre-ent statement ($1::text::jsonb) would
// have stored for want, both as jsonb's text: what the relay sends.
type storedRow struct {
	typeOf, text, oldPath  string
	createdAt, availableAt time.Time
	attempts               int
	lastError              *string
	rejectedAt             *time.Time
}

func readStored(t *testing.T, d *testdb.DB, id string, want []byte) storedRow {
	t.Helper()
	var r storedRow
	err := d.Pool.QueryRow(context.Background(), `
		SELECT jsonb_typeof(event), event::text, $2::text::jsonb::text, created_at, available_at,
		       attempts, last_error, rejected_at
		FROM audit_outbox WHERE id = $1`, id, string(want)).
		Scan(&r.typeOf, &r.text, &r.oldPath, &r.createdAt, &r.availableAt, &r.attempts, &r.lastError, &r.rejectedAt)
	if err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	return r
}

// The event is stored as a JSON object, exactly as the raw text-cast insert stored it, with the
// row's bookkeeping columns at their database defaults.
func TestEntInsertStoresTheEncodedObject(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	e := entEvent("0199a3c2-5b1e-7d40-9f3a-000000000001")
	if err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error { return trails.Insert(ctx, tx, e) }); err != nil {
		t.Fatal(err)
	}
	want, err := httpapi.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	r := readStored(t, d, e.EventID, want)
	if r.typeOf != "object" {
		t.Fatalf("jsonb_typeof = %q", r.typeOf)
	}
	if r.text != r.oldPath {
		t.Fatalf("stored event differs from the text-cast insert:\n got %s\nwant %s", r.text, r.oldPath)
	}
	got, err := canonical.Decode([]byte(r.text))
	if err != nil {
		t.Fatal(err)
	}
	exp, err := canonical.Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	same(t, "stored event", got, exp)
	var back trails.Event
	if err := json.Unmarshal([]byte(r.text), &back); err != nil {
		t.Fatal(err)
	}
	if back.EventID != e.EventID || back.RequestParameters["name"] != `a <b> & "c" é` || *back.ProjectID != *e.ProjectID {
		t.Fatalf("event didn't round-trip: %+v", back)
	}
	if r.attempts != 0 || r.lastError != nil || r.rejectedAt != nil {
		t.Fatalf("bookkeeping: attempts=%d last_error=%v rejected_at=%v", r.attempts, r.lastError, r.rejectedAt)
	}
	// Both default to the transaction's now(); the clock stays the database's.
	if !r.createdAt.Equal(r.availableAt) || r.createdAt.Location() != time.UTC || time.Since(r.createdAt) > time.Minute {
		t.Fatalf("created_at %v, available_at %v", r.createdAt, r.availableAt)
	}

	// An event without the optional parts writes them as null and stays an object.
	bare := &trails.Event{EventVersion: "1.0", EventID: "0199a3c2-5b1e-7d40-9f3a-000000000002", EventType: trails.ServiceEvent, OrganizationID: entOrg}
	if err := trails.InsertAlone(ctx, d.DB, bare); err != nil {
		t.Fatal(err)
	}
	want, _ = httpapi.Marshal(bare)
	r = readStored(t, d, bare.EventID, want)
	if r.typeOf != "object" || r.text != r.oldPath || !strings.Contains(r.text, `"requestParameters": null`) {
		t.Fatalf("bare event stored as %s %s", r.typeOf, r.text)
	}
}

// Insert still fits the event before writing it: an oversized event is stored truncated.
func TestEntInsertFitsTheEvent(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	e := entEvent("0199a3c2-5b1e-7d40-9f3a-000000000003")
	e.RequestParameters = map[string]any{"blob": strings.Repeat("x", 70*1024)}
	if err := trails.InsertAlone(ctx, d.DB, e); err != nil {
		t.Fatal(err)
	}
	var params, responses string
	if err := d.Pool.QueryRow(ctx, `SELECT (event->'requestParameters')::text, (event->'responseElements')::text FROM audit_outbox WHERE id = $1`, e.EventID).
		Scan(&params, &responses); err != nil {
		t.Fatal(err)
	}
	if params != `{"truncated": true}` || responses != `{"truncated": true}` {
		t.Fatalf("stored requestParameters %s, responseElements %s", params, responses)
	}
}

// The row commits and rolls back with the transaction it was written in; a repeated eventId is
// the primary key's 23505, reachable through ent's error.
func TestEntInsertIsTransactional(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	boom := errors.New("boom")
	rolled := entEvent("0199a3c2-5b1e-7d40-9f3a-000000000004")
	err := d.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		if err := trails.Insert(ctx, tx, rolled); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("RunTx = %v", err)
	}
	count := func(id string) int {
		var n int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM audit_outbox WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(rolled.EventID); n != 0 {
		t.Fatalf("rolled-back event left %d rows", n)
	}

	e := entEvent("0199a3c2-5b1e-7d40-9f3a-000000000005")
	if err := trails.InsertAlone(ctx, d.DB, e); err != nil {
		t.Fatal(err)
	}
	if n := count(e.EventID); n != 1 {
		t.Fatalf("committed event has %d rows", n)
	}
	err = trails.InsertAlone(ctx, d.DB, e)
	if !db.IsUnique(err, "audit_outbox_pkey") {
		t.Fatalf("duplicate eventId = %v", err)
	}
	if pg, ok := db.PgError(err); !ok || pg.Code != "23505" {
		t.Fatalf("duplicate eventId isn't a 23505: %v", err)
	}
	if n := count(e.EventID); n != 1 {
		t.Fatalf("duplicate left %d rows", n)
	}
}

// What Insert stores is what the relay posts, byte for byte, and an accepted event is deleted.
func TestEntRelayPostsTheStoredEvent(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	e := entEvent("0199a3c2-5b1e-7d40-9f3a-000000000006")
	if err := trails.InsertAlone(ctx, d.DB, e); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := d.Pool.QueryRow(ctx, `SELECT event::text FROM audit_outbox WHERE id = $1`, e.EventID).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	var posted []json.RawMessage
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("X-Internal-Token")
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		posted = parsed.Events
		_, _ = w.Write([]byte(`{"accepted":1,"rejected":[]}`))
	}))
	defer srv.Close()

	n, err := trails.NewRelay(d.DB, srv.URL, "internal-token", testdb.Logger()).Once(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Once = %d, %v", n, err)
	}
	if token != "internal-token" {
		t.Fatalf("X-Internal-Token = %q", token)
	}
	if len(posted) != 1 || string(posted[0]) != stored {
		t.Fatalf("posted %s\nstored %s", posted, stored)
	}
	c, err := outbox.CountsOf(ctx, d.DB, "audit_outbox")
	if err != nil || c.Pending != 0 || c.Rejected != 0 {
		t.Fatalf("after relaying: %+v, %v", c, err)
	}
}

// A Relay, CountsOf or Requeue given anything but an outbox table fails before any SQL runs
// (the nil *db.DB would panic if a statement were attempted).
func TestEntOutboxRefusesOtherTables(t *testing.T) {
	ctx := context.Background()
	for _, table := range []string{"", "quota_usage", "AUDIT_OUTBOX", `audit_outbox"; DROP TABLE audit_outbox; --`, "public.audit_outbox"} {
		if _, err := outbox.CountsOf(ctx, nil, table); !errors.Is(err, outbox.ErrUnknownTable) {
			t.Errorf("CountsOf(%q) = %v", table, err)
		}
		if _, err := outbox.Requeue(ctx, nil, table, []string{"x"}); !errors.Is(err, outbox.ErrUnknownTable) {
			t.Errorf("Requeue(%q) = %v", table, err)
		}
		if n, err := (&outbox.Relay{Table: table}).Once(ctx); n != 0 || !errors.Is(err, outbox.ErrUnknownTable) {
			t.Errorf("Relay{Table: %q}.Once = %d, %v", table, n, err)
		}
	}

	// The two outbox tables still work.
	d := testdb.New(t)
	for _, table := range []string{"audit_outbox", "usage_outbox"} {
		if c, err := outbox.CountsOf(ctx, d.DB, table); err != nil || c.Pending != 0 {
			t.Fatalf("CountsOf(%s) = %+v, %v", table, c, err)
		}
		if n, err := outbox.Requeue(ctx, d.DB, table, []string{}); err != nil || n != 0 {
			t.Fatalf("Requeue(%s) = %d, %v", table, n, err)
		}
	}
}
