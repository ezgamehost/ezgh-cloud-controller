package metering_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/metering"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/outbox"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/trails"
)

const credential = "pk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

// seed writes n minimal usage events straight into the outbox (the relay doesn't validate; it
// ships what's there), each an hour-window at a distinct minute so their order is known.
func seed(t *testing.T, pool *pgxpool.Pool, n int, prefix string) []string {
	t.Helper()
	var idsOut []string
	for i := range n {
		id := fmt.Sprintf("%s:%05d", prefix, i)
		start := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format("2006-01-02T15:04:05Z")
		event := fmt.Sprintf(`{"id":%q,"organizationId":"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70","window":{"start":%q,"end":"2026-09-29T00:00:00Z"},"quantity":"1"}`, id, start)
		if _, err := pool.Exec(context.Background(), `INSERT INTO usage_outbox (id, event) VALUES ($1, $2::text::jsonb)`, id, event); err != nil {
			t.Fatal(err)
		}
		idsOut = append(idsOut, id)
	}
	return idsOut
}

type batch struct{ IDs []string }

// fakeIngest records every batch and answers with respond.
type fakeIngest struct {
	mu      sync.Mutex
	batches []batch
	auth    []string
	respond func(ids []string) (int, any)
}

func (f *fakeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed struct {
		Events []struct {
			ID string `json:"id"`
		} `json:"events"`
	}
	_ = json.Unmarshal(body, &parsed)
	ids := make([]string, len(parsed.Events))
	for i, e := range parsed.Events {
		ids[i] = e.ID
	}
	f.mu.Lock()
	f.batches = append(f.batches, batch{IDs: ids})
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	status, answer := f.respond(ids)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(answer)
}

func accepted(ids []string) (int, any) {
	return 200, map[string]any{"accepted": len(ids), "duplicates": 0, "rejected": []any{}}
}

func relay(t *testing.T, d *kitdb.DB, f http.Handler) *outbox.Relay {
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return metering.NewRelay(d, server.URL, credential, testdb.Logger())
}

type row struct {
	attempts          int
	lastError         *string
	rejected, delayed bool
}

func rows(t *testing.T, pool *pgxpool.Pool) map[string]row {
	t.Helper()
	rs, err := pool.Query(context.Background(), `SELECT id, attempts, last_error, rejected_at IS NOT NULL, available_at > now() FROM usage_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]row{}
	for rs.Next() {
		var id string
		var r row
		if err := rs.Scan(&id, &r.attempts, &r.lastError, &r.rejected, &r.delayed); err != nil {
			t.Fatal(err)
		}
		out[id] = r
	}
	return out
}

func once(t *testing.T, r *outbox.Relay) int {
	t.Helper()
	n, err := r.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRelayAcceptedEventsAreDeleted(t *testing.T) {
	db := testdb.New(t)
	ids := seed(t, db.Pool, 5, "a")
	f := &fakeIngest{respond: accepted}
	r := relay(t, db.DB, f)
	if n := once(t, r); n != 5 {
		t.Fatalf("claimed %d", n)
	}
	if left := rows(t, db.Pool); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if f.auth[0] != "Bearer "+credential {
		t.Errorf("sent Authorization %q", f.auth[0])
	}
	if strings.Join(f.batches[0].IDs, ",") != strings.Join(ids, ",") {
		t.Errorf("sent out of usage order: %v", f.batches[0].IDs)
	}
}

func TestRelayDuplicatesAreDeletedToo(t *testing.T) {
	db := testdb.New(t)
	seed(t, db.Pool, 4, "d")
	f := &fakeIngest{respond: func(ids []string) (int, any) {
		return 200, map[string]any{"accepted": 1, "duplicates": 3, "rejected": []any{}}
	}}
	once(t, relay(t, db.DB, f))
	if left := rows(t, db.Pool); len(left) != 0 {
		t.Errorf("duplicates weren't deleted: %v", left)
	}
}

func TestRelayRejectedEventsAreKeptAndNeverResent(t *testing.T) {
	db := testdb.New(t)
	ids := seed(t, db.Pool, 3, "r")
	f := &fakeIngest{respond: func(sent []string) (int, any) {
		return 200, map[string]any{"accepted": len(sent) - 1, "duplicates": 0, "rejected": []any{
			map[string]any{"index": 1, "id": sent[1], "issues": []any{map[string]any{"path": "dimensions.size", "message": "invalid_dimension: expected one of small, large"}}},
		}}
	}}
	r := relay(t, db.DB, f)
	once(t, r)
	left := rows(t, db.Pool)
	if len(left) != 1 || !left[ids[1]].rejected || left[ids[1]].lastError == nil || !strings.Contains(*left[ids[1]].lastError, "invalid_dimension") {
		t.Fatalf("left %v", left)
	}
	if n := once(t, r); n != 0 {
		t.Errorf("a rejected event was claimed again")
	}
	// A person requeues it once the cause is fixed.
	if n, err := outbox.Requeue(context.Background(), db.DB, "usage_outbox", []string{ids[1]}); err != nil || n != 1 {
		t.Fatalf("requeue: %d, %v", n, err)
	}
	f.respond = accepted
	if n := once(t, r); n != 1 {
		t.Errorf("requeued event not claimed")
	}
}

// A rejection that doesn't match the batch (wrong id at the index) can't be trusted: that event
// is retried, not rejected or deleted.
func TestRelayMismatchedRejectionIsRetried(t *testing.T) {
	db := testdb.New(t)
	ids := seed(t, db.Pool, 2, "m")
	f := &fakeIngest{respond: func(sent []string) (int, any) {
		return 200, map[string]any{"accepted": 1, "duplicates": 0, "rejected": []any{
			map[string]any{"index": 0, "id": "someone-else", "issues": []any{map[string]any{"path": "", "message": "x"}}},
		}}
	}}
	once(t, relay(t, db.DB, f))
	left := rows(t, db.Pool)
	if len(left) != 1 || left[ids[0]].rejected || !left[ids[0]].delayed {
		t.Errorf("left %v", left)
	}
}

// 413: the batch is split in halves down to single events; a single event still refused is
// rejected as payload_too_large, never retried forever.
func TestRelaySplitsOn413(t *testing.T) {
	db := testdb.New(t)
	ids := seed(t, db.Pool, 7, "s")
	tooBig := ids[4]
	f := &fakeIngest{respond: func(sent []string) (int, any) {
		for _, id := range sent {
			if id == tooBig {
				return 413, map[string]any{"error": map[string]any{"code": "payload_too_large", "message": "x"}}
			}
		}
		if len(sent) > 2 {
			return 413, map[string]any{"error": map[string]any{"code": "payload_too_large", "message": "x"}}
		}
		return accepted(sent)
	}}
	once(t, relay(t, db.DB, f))
	left := rows(t, db.Pool)
	if len(left) != 1 || !left[tooBig].rejected || !strings.Contains(*left[tooBig].lastError, "payload_too_large") {
		t.Fatalf("left %v", left)
	}
	delivered := map[string]int{}
	for _, b := range f.batches {
		if len(b.IDs) <= 2 && !contains(b.IDs, tooBig) {
			for _, id := range b.IDs {
				delivered[id]++
			}
		}
	}
	for _, id := range ids {
		if id != tooBig && delivered[id] != 1 {
			t.Errorf("%s delivered %d times", id, delivered[id])
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// 503 (nothing stored), 401 (a bad credential) and 400 back off and keep every event, to resend
// the whole batch later, which is safe.
func TestRelayBacksOffOnFailures(t *testing.T) {
	for _, status := range []int{503, 401, 400, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			db := testdb.New(t)
			ids := seed(t, db.Pool, 3, "f")
			f := &fakeIngest{respond: func([]string) (int, any) {
				return status, map[string]any{"error": map[string]any{"code": "unavailable", "message": "x"}}
			}}
			r := relay(t, db.DB, f)
			once(t, r)
			left := rows(t, db.Pool)
			for _, id := range ids {
				x := left[id]
				if x.rejected || !x.delayed || x.attempts != 1 || x.lastError == nil || !strings.Contains(*x.lastError, fmt.Sprint(status)) {
					t.Errorf("%s: %+v", id, x)
				}
			}
			if n := once(t, r); n != 0 {
				t.Errorf("claimed during backoff")
			}
			// The backoff is 5 s after the first attempt.
			var wait float64
			_ = db.Pool.QueryRow(context.Background(), `SELECT extract(epoch FROM min(available_at) - now()) FROM usage_outbox`).Scan(&wait)
			if wait < 3 || wait > 6 {
				t.Errorf("backoff %v s", wait)
			}
		})
	}
}

// No answer at all is an outage too.
func TestRelayUnreachable(t *testing.T) {
	db := testdb.New(t)
	seed(t, db.Pool, 1, "u")
	r := metering.NewRelay(db.DB, "http://127.0.0.1:1", credential, testdb.Logger())
	once(t, r)
	for _, x := range rows(t, db.Pool) {
		if x.rejected || !x.delayed {
			t.Errorf("%+v", x)
		}
	}
}

// Several relays at once never send an event twice concurrently: FOR UPDATE SKIP LOCKED and the
// lease hand each row to one claimant.
func TestConcurrentClaimsNeverOverlap(t *testing.T) {
	db := testdb.New(t)
	ids := seed(t, db.Pool, 1000, "c")
	var mu sync.Mutex
	seen := map[string]int{}
	f := &fakeIngest{respond: func(sent []string) (int, any) {
		mu.Lock()
		for _, id := range sent {
			seen[id]++
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		return accepted(sent)
	}}
	server := httptest.NewServer(f)
	defer server.Close()
	var wg sync.WaitGroup
	var claimed atomic.Int64
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := metering.NewRelay(db.DB, server.URL, credential, testdb.Logger())
			r.BatchSize = 37
			for {
				n, err := r.Once(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
				claimed.Add(int64(n))
			}
		}()
	}
	wg.Wait()
	if claimed.Load() != int64(len(ids)) {
		t.Errorf("claimed %d events for %d rows", claimed.Load(), len(ids))
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Fatalf("%s sent %d times", id, seen[id])
		}
	}
	if left := rows(t, db.Pool); len(left) != 0 {
		t.Errorf("%d left", len(left))
	}
}

// A relay that dies mid-batch holds its rows only for the lease; then another takes them.
func TestLeaseHandsOverAfterACrash(t *testing.T) {
	db := testdb.New(t)
	seed(t, db.Pool, 3, "l")
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	blocked := &fakeIngest{respond: func(sent []string) (int, any) {
		started <- struct{}{}
		<-release
		return 503, nil
	}}
	a := relay(t, db.DB, blocked)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.Once(context.Background())
	}()
	<-started
	b := relay(t, db.DB, &fakeIngest{respond: accepted})
	if n := once(t, b); n != 0 {
		t.Fatalf("claimed %d rows another relay holds", n)
	}
	// The first relay "crashed": its lease lapses.
	if _, err := db.Pool.Exec(context.Background(), `UPDATE usage_outbox SET available_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n := once(t, b); n != 3 {
		t.Fatalf("after the lease, claimed %d", n)
	}
	close(release)
	<-done
}

// The audit relay is the same machinery with audit-api's credential.
func TestAuditRelaySendsTheInternalToken(t *testing.T) {
	db := testdb.New(t)
	event := `{"eventId":"0199a3d0-3c4d-7e5f-a061-7c8d9e0f1a2b","eventTime":"2026-09-28T09:58:02.114Z"}`
	if _, err := db.Pool.Exec(context.Background(), `INSERT INTO audit_outbox (id, event) VALUES ('0199a3d0-3c4d-7e5f-a061-7c8d9e0f1a2b', $1::text::jsonb)`, event); err != nil {
		t.Fatal(err)
	}
	var token string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token = r.Header.Get("X-Internal-Token")
		_, _ = w.Write([]byte(`{"accepted":1,"rejected":[]}`))
	}))
	defer server.Close()
	r := auditRelay(db.DB, server.URL)
	if n := once(t, r); n != 1 || token != "local-internal-token" {
		t.Errorf("claimed %d, token %q", n, token)
	}
}

func auditRelay(d *kitdb.DB, url string) *outbox.Relay {
	return trails.NewRelay(d, url, "local-internal-token", testdb.Logger())
}
