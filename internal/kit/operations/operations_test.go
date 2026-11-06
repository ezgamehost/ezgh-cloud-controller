package operations_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/operations"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

const org = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"

// handler records what the worker asks of it.
type handler struct {
	runs, succeeded, undone atomic.Int64
	run                     func(op *operations.Operation) error
}

func (h *handler) Run(_ context.Context, op *operations.Operation) error {
	h.runs.Add(1)
	if h.run != nil {
		return h.run(op)
	}
	return nil
}

func (h *handler) Succeed(context.Context, *db.Tx, *operations.Operation) error {
	h.succeeded.Add(1)
	return nil
}

func (h *handler) Undo(context.Context, *db.Tx, *operations.Operation, *httpapi.Error) error {
	h.undone.Add(1)
	return nil
}

func create(t *testing.T, d *db.DB, target string) (*operations.Operation, error) {
	t.Helper()
	var op *operations.Operation
	err := d.RunTx(context.Background(), nil, func(tx *db.Tx) error {
		var err error
		op, err = operations.Create(context.Background(), tx, operations.New{
			OrganizationID: org, Type: "CreateWidget", Action: "widgets.widgets.create",
			Target:    operations.Target{ID: target, ResourceName: "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:wgt_4k2m9x0a7qpe", Type: "Widget"},
			CreatedBy: authz.Principal{Type: "user", ID: "u1"}, Input: map[string]string{"size": "small"},
		})
		return err
	})
	return op, err
}

func status(t *testing.T, d *db.DB, id string) *operations.Operation {
	t.Helper()
	op, err := operations.Get(context.Background(), d, org, id)
	if err != nil || op == nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return op
}

func TestOneActiveOperationPerResource(t *testing.T) {
	db := testdb.New(t)
	target := ids.NewV7()
	first, err := create(t, db.DB, target)
	if err != nil {
		t.Fatal(err)
	}
	_, err = create(t, db.DB, target)
	var e *httpapi.Error
	if !errors.As(err, &e) || e.Code != "operation_in_progress" || e.Extra["operationId"] != first.ID {
		t.Fatalf("got %v", err)
	}
	v := first.View()
	if v.Status != "queued" || !v.Cancellable || v.StartedAt != nil || v.Error != nil || v.Target.Type != "Widget" {
		t.Errorf("view %+v", v)
	}
}

func worker(d *db.DB, h operations.Handler) *operations.Worker {
	return &operations.Worker{DB: d, Handlers: map[string]operations.Handler{"CreateWidget": h}, Logger: testdb.Logger()}
}

func TestWorkerSucceeds(t *testing.T) {
	db := testdb.New(t)
	op, _ := create(t, db.DB, ids.NewV7())
	h := &handler{}
	if n, err := worker(db.DB, h).Once(context.Background()); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	got := status(t, db.DB, op.ID)
	if got.Status != operations.Succeeded || got.FinishedAt == nil || got.StartedAt == nil || h.succeeded.Load() != 1 || got.Cancellable() {
		t.Errorf("%+v", got)
	}
	// Terminal means final: a second pass finds nothing.
	if n, _ := worker(db.DB, h).Once(context.Background()); n != 0 {
		t.Error("claimed a finished operation")
	}
}

func TestWorkerFailsWithTheProductsError(t *testing.T) {
	db := testdb.New(t)
	op, _ := create(t, db.DB, ids.NewV7())
	h := &handler{run: func(*operations.Operation) error {
		return &httpapi.Error{Status: 503, Code: "capacity_unavailable", Message: "No capacity"}
	}}
	_, _ = worker(db.DB, h).Once(context.Background())
	got := status(t, db.DB, op.ID)
	if got.Status != operations.Failed || got.Error["code"] != "capacity_unavailable" || h.undone.Load() != 1 {
		t.Errorf("%+v", got)
	}
}

func TestWorkerRetriesThenFails(t *testing.T) {
	db := testdb.New(t)
	op, _ := create(t, db.DB, ids.NewV7())
	h := &handler{run: func(*operations.Operation) error { return errors.New("flaky") }}
	w := worker(db.DB, h)
	for attempt := 1; attempt <= operations.MaxAttempts; attempt++ {
		if n, _ := w.Once(context.Background()); n != 1 {
			t.Fatalf("attempt %d not claimed", attempt)
		}
		// Skip the backoff.
		_, _ = db.Pool.Exec(context.Background(), `UPDATE operations SET available_at = now() WHERE id = $1`, op.ID)
	}
	got := status(t, db.DB, op.ID)
	if got.Status != operations.Failed || got.Error["code"] != "internal_error" || h.runs.Load() != int64(operations.MaxAttempts) {
		t.Errorf("%+v runs %d", got, h.runs.Load())
	}
}

// A run that hands off isn't a failed one: it comes back when it's due, however often, with its
// attempts untouched, and then finishes as any other does.
func TestWorkerWaiting(t *testing.T) {
	db := testdb.New(t)
	op, _ := create(t, db.DB, ids.NewV7())
	h := &handler{}
	h.run = func(*operations.Operation) error {
		if h.runs.Load() <= 2*int64(operations.MaxAttempts) {
			return operations.Waiting{After: time.Millisecond}
		}
		return nil
	}
	w := worker(db.DB, h)
	for h.succeeded.Load() == 0 {
		if _, err := w.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := status(t, db.DB, op.ID)
		if got.Status == operations.Failed {
			t.Fatalf("%+v", got)
		}
		// Parked, it's queued (still cancellable) but shows as running.
		if v := got.View(); got.Status == operations.Queued && (v.Status != operations.Running || !v.Cancellable) {
			t.Fatalf("parked view %+v", v)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := status(t, db.DB, op.ID); got.Status != operations.Succeeded || got.Attempts != 1 {
		t.Errorf("%+v", got)
	}
}

// A worker that vanished (its lease lapsed) is replaced once; the second time the operation fails
// with worker_lost.
func TestReapLapsedLeases(t *testing.T) {
	db := testdb.New(t)
	op, _ := create(t, db.DB, ids.NewV7())
	h := &handler{}
	w := worker(db.DB, h)
	vanish := func() {
		_, err := db.Pool.Exec(context.Background(), `UPDATE operations SET status = 'running', attempts = attempts + 1, started_at = now(), lease_until = now() - interval '1 second' WHERE id = $1`, op.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	vanish()
	if err := w.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := status(t, db.DB, op.ID); got.Status != operations.Queued {
		t.Fatalf("after one lapse: %s", got.Status)
	}
	vanish()
	if err := w.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := status(t, db.DB, op.ID)
	if got.Status != operations.Failed || got.Error["code"] != "worker_lost" || h.undone.Load() != 1 {
		t.Errorf("%+v", got)
	}
}

// Workers on several replicas never run one operation twice at once.
func TestConcurrentWorkersClaimEachOperationOnce(t *testing.T) {
	db := testdb.New(t)
	const n = 60
	for range n {
		if _, err := create(t, db.DB, ids.NewV7()); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	h := &handler{run: func(op *operations.Operation) error {
		mu.Lock()
		seen[op.ID]++
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		return nil
	}}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := worker(db.DB, h)
			for {
				claimed, err := w.Once(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if claimed == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("ran %d of %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("%s ran %d times", id, count)
		}
	}
}
