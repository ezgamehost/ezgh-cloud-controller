package operations

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/operation"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
)

// Handler does one type of operation.
type Handler interface {
	// Run does the work, outside any transaction (it may take minutes, or call other systems).
	// It must be safe to run twice: a worker that vanishes mid-run is replaced. An
	// *httpapi.Error fails the operation with that error; any other error is retried with backoff,
	// up to MaxAttempts, and then fails it as internal_error.
	Run(ctx context.Context, op *Operation) error
	// Succeed applies the result in the transaction that marks the operation succeeded (the
	// widget becomes running, its usage starts).
	Succeed(ctx context.Context, tx *db.Tx, op *Operation) error
	// Undo puts the target back in the transaction that marks the operation failed or cancelled
	// (cause is nil for a cancel): the resource returns to where it was, or a create that can't be
	// undone leaves it failed.
	Undo(ctx context.Context, tx *db.Tx, op *Operation, cause *httpapi.Error) error
}

// Canceller is a Handler that can't always undo a queued operation (EZGH Git's delete, once its
// objects may be gone): CanCancel says whether a queued op can still be cancelled. It's the
// operation's `cancellable` on GET and in lists, and a cancel it refuses is 409
// operation_not_cancellable (contracts/operations.md), before Undo runs. q is the pool or the
// cancel's transaction.
type Canceller interface {
	CanCancel(ctx context.Context, q db.Querier, op *Operation) (bool, error)
}

// Worker settings.
const (
	// Lease is how long a claim holds an operation without a heartbeat.
	Lease = 30 * time.Second
	// MaxAttempts is how many times an operation is run (a vanished worker's run counts).
	MaxAttempts = 3
	claimBatch  = 10
	idleWait    = time.Second
)

// Waiting is Run's answer when the work goes on somewhere else, a product's own queue, say, and all
// there is to do is look again: the operation goes back to the queue, due After from now, and this
// run doesn't count as an attempt. So one long operation doesn't hold up a worker's batch. It's
// Queued meanwhile, so a cancel can take it: Undo must cope with the work that's gone on elsewhere.
type Waiting struct{ After time.Duration }

func (Waiting) Error() string { return "operations: waiting" }

// Worker runs queued operations.
type Worker struct {
	DB       *db.DB
	Handlers map[string]Handler
	Logger   *slog.Logger
}

// Run works until ctx is done: reap operations whose worker vanished, claim a batch, run it.
// On shutdown it stops claiming and lets the operations in hand finish; their leases let another
// replica take over any that don't.
func (w *Worker) Run(ctx context.Context) {
	for {
		if err := w.Reap(ctx); err != nil && ctx.Err() == nil {
			w.Logger.Error("operations: reaping lapsed leases failed", "error", err.Error())
		}
		n, err := w.Once(ctx)
		if err != nil && ctx.Err() == nil {
			w.Logger.Error("operations: a pass failed", "error", err.Error())
		}
		if ctx.Err() != nil {
			return
		}
		if n >= claimBatch {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(idleWait):
		}
	}
}

// Once claims up to a batch of due queued operations and runs them concurrently. It returns how
// many it claimed.
func (w *Worker) Once(ctx context.Context) (int, error) {
	rows, err := w.DB.QueryContext(ctx, `
		UPDATE operations SET status = 'running', started_at = coalesce(started_at, now()), updated_at = now(),
			lease_until = now() + make_interval(secs => $2), attempts = attempts + 1
		WHERE id IN (
			SELECT id FROM operations WHERE status = 'queued' AND available_at <= now()
			ORDER BY available_at LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING `+columns, claimBatch, Lease.Seconds())
	if err != nil {
		return 0, err
	}
	var claimed []*Operation
	for rows.Next() {
		op, err := scan(rows)
		if err != nil {
			_ = rows.Close()
			return 0, err
		}
		claimed = append(claimed, op)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, op := range claimed {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.work(ctx, op)
		}()
	}
	wg.Wait()
	return len(claimed), nil
}

// work runs one claimed operation to its end (or back to the queue).
func (w *Worker) work(ctx context.Context, op *Operation) {
	log := w.Logger.With("operationId", op.ID, "type", op.Type, "organizationId", op.OrganizationID)
	h, ok := w.Handlers[op.Type]
	if !ok {
		w.fail(ctx, op, httpapi.Internal(fmt.Errorf("no handler for %s operations", op.Type)), log)
		return
	}
	// Heartbeat while the work runs, so a long run keeps its lease.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		t := time.NewTicker(Lease / 3)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if err := renew(context.WithoutCancel(runCtx), w.DB, op.ID); err != nil {
					log.Warn("operations: heartbeat failed", "error", err.Error())
				}
			}
		}
	}()

	runErr := h.Run(runCtx, op)
	stop()
	// Settle even during shutdown: the work is done (or failed) and only the bookkeeping is left.
	settle := context.WithoutCancel(ctx)
	var apiErr *httpapi.Error
	var waiting Waiting
	switch {
	case errors.As(runErr, &waiting):
		if err := park(settle, w.DB, op.ID, waiting.After); err != nil {
			log.Error("operations: requeueing failed; the lease will lapse", "error", err.Error())
		}
	case runErr == nil:
		err := w.DB.RunTx(settle, nil, func(tx *db.Tx) error {
			current, err := lockRunning(settle, tx, op.ID)
			if err != nil || current == nil {
				return err // reaped meanwhile: the reaper decided its fate
			}
			if err := h.Succeed(settle, tx, current); err != nil {
				return err
			}
			return finish(settle, tx, current, Succeeded, nil)
		})
		if err != nil {
			log.Error("operations: completing failed; it'll be retried", "error", err.Error())
			w.retry(settle, op, err.Error(), log)
			return
		}
		log.Info("operation succeeded")
	case errors.As(runErr, &apiErr):
		w.fail(settle, op, apiErr, log)
	case ctx.Err() != nil:
		// Shutting down mid-run: hand it back at once rather than waiting out the lease.
		w.retry(settle, op, "the worker shut down", log)
	default:
		log.Warn("operations: run failed", "error", runErr.Error(), "attempt", op.Attempts)
		if op.Attempts >= MaxAttempts {
			w.fail(settle, op, httpapi.Internal(runErr), log)
			return
		}
		w.retry(settle, op, runErr.Error(), log)
	}
}

// retry puts a running operation back in the queue after a backoff: 5 s, 10 s, 20 s….
func (w *Worker) retry(ctx context.Context, op *Operation, reason string, log *slog.Logger) {
	if err := backOff(ctx, w.DB, op.ID); err != nil {
		log.Error("operations: requeueing failed; the lease will lapse", "error", err.Error(), "reason", reason)
	}
}

// fail finishes a running operation as failed, with the target put back by its handler.
func (w *Worker) fail(ctx context.Context, op *Operation, cause *httpapi.Error, log *slog.Logger) {
	if cause.Status >= 500 && cause.Cause != nil {
		log.Error("operation failed", "code", cause.Code, "error", cause.Cause.Error())
	}
	err := w.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
		current, err := lockRunning(ctx, tx, op.ID)
		if err != nil || current == nil {
			return err
		}
		if h, ok := w.Handlers[current.Type]; ok {
			if err := h.Undo(ctx, tx, current, cause); err != nil {
				return err
			}
		}
		return finish(ctx, tx, current, Failed, cause)
	})
	if err != nil {
		log.Error("operations: failing an operation failed; the lease will lapse", "error", err.Error())
	}
}

// Reap handles running operations whose lease lapsed (their worker vanished): back to queued the
// first time, failed with worker_lost after that.
func (w *Worker) Reap(ctx context.Context) error {
	lapsed, err := lapsedIDs(ctx, w.DB)
	if err != nil {
		return err
	}
	for _, id := range lapsed {
		err := w.DB.RunTx(ctx, nil, func(tx *db.Tx) error {
			op, err := lockLapsed(ctx, tx, id)
			if err != nil || op == nil {
				return err
			}
			if op.Attempts < 2 {
				w.Logger.Warn("operations: a worker vanished; requeueing", "operationId", op.ID)
				return requeue(ctx, tx, op.ID)
			}
			w.Logger.Error("operations: a worker vanished again; failing it", "operationId", op.ID)
			cause := &httpapi.Error{Status: 500, Code: "worker_lost", Message: "The worker running this operation stopped; nothing more will be done"}
			if h, ok := w.Handlers[op.Type]; ok {
				if err := h.Undo(ctx, tx, op, cause); err != nil {
					return err
				}
			}
			return finish(ctx, tx, op, Failed, cause)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// lockLapsed locks a running operation whose lease lapsed, for the reaper; nil when it isn't one
// any more, or another transaction holds it (SKIP LOCKED: a worker completing it, or another
// reaper). q is the reaper's transaction.
func lockLapsed(ctx context.Context, q db.Querier, id string) (*Operation, error) {
	return found(entdb.Client(q).Operation.Query().Where(operation.ID(id), isRunning, leaseLapsed).
		ForUpdate(entsql.WithLockAction(entsql.SkipLocked)).Only(ctx))
}

// lapsedIDs are up to 100 running operations whose lease lapsed.
func lapsedIDs(ctx context.Context, q db.Querier) ([]string, error) {
	return entdb.Client(q).Operation.Query().Where(isRunning, leaseLapsed).Limit(100).IDs(ctx)
}

// renew extends a running operation's lease to Lease from now: the heartbeat. It's nothing
// once the operation isn't running (it was reaped, or finished).
func renew(ctx context.Context, q db.Querier, id string) error {
	return entdb.Client(q).Operation.Update().Where(operation.ID(id), isRunning).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(operation.FieldLeaseUntil, leaseFromNow()) }).
		Exec(ctx)
}

// backOff puts a running operation back in the queue, due after 5 s, 10 s, 20 s… by its
// attempts so far, up to 5 minutes.
func backOff(ctx context.Context, q db.Querier, id string) error {
	return entdb.Client(q).Operation.Update().Where(operation.ID(id), isRunning).
		SetStatus(Queued).ClearLeaseUntil().
		Modify(func(u *entsql.UpdateBuilder) {
			u.Set(operation.FieldUpdatedAt, entsql.Expr("now()")).
				Set(operation.FieldAvailableAt, entsql.Expr("now() + least(interval '5 minutes', interval '5 seconds' * power(2, least(attempts - 1, 10)))"))
		}).
		Exec(ctx)
}

// park puts a waiting operation back in the queue, due after wait, giving back its attempt.
func park(ctx context.Context, q db.Querier, id string, wait time.Duration) error {
	return entdb.Client(q).Operation.Update().Where(operation.ID(id), isRunning).
		SetStatus(Queued).ClearLeaseUntil().AddAttempts(-1).
		Modify(func(u *entsql.UpdateBuilder) {
			u.Set(operation.FieldUpdatedAt, entsql.Expr("now()")).
				Set(operation.FieldAvailableAt, entsql.ExprFunc(func(b *entsql.Builder) {
					b.WriteString("now() + make_interval(secs => ").Arg(wait.Seconds()).WriteByte(')')
				}))
		}).
		Exec(ctx)
}

// requeue puts a reaped operation back in the queue, due at once.
func requeue(ctx context.Context, q db.Querier, id string) error {
	return entdb.Client(q).Operation.Update().Where(operation.ID(id)).
		SetStatus(Queued).ClearLeaseUntil().
		Modify(func(u *entsql.UpdateBuilder) {
			u.Set(operation.FieldAvailableAt, entsql.Expr("now()")).Set(operation.FieldUpdatedAt, entsql.Expr("now()"))
		}).
		Exec(ctx)
}

// leaseFromNow is now() + Lease, on the database's clock: a heartbeat's new lease_until. A new
// expression each time: rendering one sets its numbering, so it can't be shared between calls.
func leaseFromNow() entsql.Querier {
	return entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("now() + make_interval(secs => ").Arg(Lease.Seconds()).WriteByte(')')
	})
}
