// Package operations is the platform's long-running operations (contracts/operations.md): an
// asynchronous endpoint answers 202 with an operation at once, the client polls it, and a worker
// runs the work from the operations table, never from the request's memory.
//
//   - The target exists from the start: the product inserts its row in a transitional status in
//     the request's transaction, next to the operation.
//   - One active operation per resource: a partial unique index refuses a second queued or
//     running one (409 operation_in_progress).
//   - Every operation reaches exactly one terminal status, succeeded, failed or cancelled, and
//     never changes again.
//   - The worker claims queued operations with FOR UPDATE SKIP LOCKED and a lease, heartbeats
//     while working, and puts an operation whose worker vanished back to queued once, then fails
//     it with worker_lost. Work must be safe to run twice.
package operations

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/ids"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/instant"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/operation"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/predicate"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

// Statuses.
const (
	Queued    = "queued"
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Retention is how long a finished operation stays readable; then it's 404.
const Retention = 7 * 24 * time.Hour

// PollSeconds is the Retry-After suggested on a 202 and on an unfinished operation.
const PollSeconds = 2

// Operation is one row of the operations table.
type Operation struct {
	ID                 string
	OrganizationID     string
	ProjectID          *string
	Type               string
	Action             string
	Status             string
	TargetID           string
	TargetResourceName string
	TargetType         string
	CreatedBy          authz.Principal
	// Input is what the product needs to run it (the new size, the status to go back to).
	Input      json.RawMessage
	Error      map[string]any
	CreatedAt  time.Time
	UpdatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	Attempts   int
}

// Terminal reports whether the operation has finished.
func (o *Operation) Terminal() bool {
	return o.Status == Succeeded || o.Status == Failed || o.Status == Cancelled
}

// Cancellable is whether DELETE may still cancel it: while it's queued. Once a worker runs it,
// the product can't take the work back half-done.
func (o *Operation) Cancellable() bool { return o.Status == Queued }

// PublicStatus is the status customers see. An operation a worker started and parked while other
// work runs (Waiting) is queued again so it stays cancellable, but to the customer it is running.
func (o *Operation) PublicStatus() string {
	if o.Status == Queued && o.StartedAt != nil {
		return Running
	}
	return o.Status
}

// Target is the resource an operation acts on.
type Target struct {
	ID           string `json:"id"`
	ResourceName string `json:"resourceName"`
	Type         string `json:"type"`
}

// View is the operation as the API returns it (schemas/operation.schema.json).
type View struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organizationId"`
	ProjectID      *string         `json:"projectId"`
	Type           string          `json:"type"`
	Action         string          `json:"action"`
	Status         string          `json:"status"`
	Target         Target          `json:"target"`
	Cancellable    bool            `json:"cancellable"`
	CreatedBy      authz.Principal `json:"createdBy"`
	CreatedAt      string          `json:"createdAt"`
	UpdatedAt      string          `json:"updatedAt"`
	StartedAt      *string         `json:"startedAt"`
	FinishedAt     *string         `json:"finishedAt"`
	Error          map[string]any  `json:"error"`
}

// View renders the operation.
func (o *Operation) View() View {
	return View{
		ID: o.ID, OrganizationID: o.OrganizationID, ProjectID: o.ProjectID, Type: o.Type, Action: o.Action,
		Status: o.PublicStatus(), Target: Target{ID: o.TargetID, ResourceName: o.TargetResourceName, Type: o.TargetType},
		Cancellable: o.Cancellable(), CreatedBy: o.CreatedBy,
		CreatedAt: instant.Format(o.CreatedAt), UpdatedAt: instant.Format(o.UpdatedAt),
		StartedAt: instant.FormatPtr(o.StartedAt), FinishedAt: instant.FormatPtr(o.FinishedAt), Error: o.Error,
	}
}

// Location is the operation's path-absolute URL.
func (o *Operation) Location() string {
	return "/v1/organizations/" + o.OrganizationID + "/operations/" + o.ID
}

// Accepted is the 202 an asynchronous endpoint answers with.
func (o *Operation) Accepted() *service.Response {
	return &service.Response{Status: 202, Body: o.View(), Location: o.Location(), RetryAfter: PollSeconds, OperationID: o.ID}
}

// New describes an operation to start.
type New struct {
	OrganizationID string
	ProjectID      *string
	// Type is the eventName of the call that started it (CreateWidget); Action the IAM action it
	// was authorized with.
	Type, Action string
	Target       Target
	CreatedBy    authz.Principal
	Input        any
}

// Create inserts a queued operation in the request's transaction. A second active operation on
// the same target is 409 operation_in_progress, naming the one in the way.
func Create(ctx context.Context, tx *db.Tx, n New) (*Operation, error) {
	input, err := json.Marshal(n.Input)
	if err != nil {
		return nil, err
	}
	if n.Input == nil {
		input = []byte("{}")
	}
	// The operation's createdBy is {type, id} (operation.schema.json): never the answer's slug.
	createdBy, _ := json.Marshal(authz.Principal{Type: n.CreatedBy.Type, ID: n.CreatedBy.ID})
	id := ids.NewV7()
	// A savepoint, so the unique violation this may hit doesn't abort the request's transaction.
	var op *Operation
	err = tx.Savepoint(ctx, "operation_create", func() error {
		row := tx.QueryRowContext(ctx, `
		INSERT INTO operations (id, organization_id, project_id, type, action, status, target_id, target_resource_name, target_type, created_by, input)
		VALUES ($1, $2, $3, $4, $5, 'queued', $6, $7, $8, $9::text::jsonb, $10::text::jsonb)
		RETURNING `+columns, id, n.OrganizationID, n.ProjectID, n.Type, n.Action, n.Target.ID, n.Target.ResourceName, n.Target.Type, string(createdBy), string(input))
		var err error
		op, err = scan(row)
		return err
	})
	if err != nil {
		if db.IsUnique(err, "operations_one_active_per_target_idx") {
			return nil, InProgress(ctx, tx, n.Target.ID)
		}
		return nil, err
	}
	return op, nil
}

// InProgress is the 409 for a change to a resource with an operation queued or running.
func InProgress(ctx context.Context, q db.Querier, targetID string) error {
	active, _ := entdb.Client(q).Operation.Query().Where(operation.TargetID(targetID), isActive).OnlyID(ctx)
	e := httpapi.Conflict("operation_in_progress", "Another operation on this resource hasn't finished yet")
	if active != "" {
		e.Message = "Operation " + active + " on this resource hasn't finished yet"
		e.Extra = map[string]any{"operationId": active}
	}
	return e
}

// Active is the resource's queued or running operation, if any.
func Active(ctx context.Context, q db.Querier, targetID string) (*Operation, error) {
	return found(entdb.Client(q).Operation.Query().Where(operation.TargetID(targetID), isActive).Only(ctx))
}

// The status predicates are written as literals, as the SQL had them, not bound: a generic plan
// can then still prove the partial indexes' predicates (operations_one_active_per_target_idx is
// the only index on target_id). Constants only.
var (
	// isActive is status IN ('queued', 'running').
	isActive predicate.Operation = func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(operation.FieldStatus)).WriteString(" IN ('queued', 'running')")
		}))
	}
	// isRunning is status = 'running'.
	isRunning predicate.Operation = func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(operation.FieldStatus)).WriteString(" = 'running'")
		}))
	}
	// leaseLapsed is lease_until < now(), on the database's clock.
	leaseLapsed predicate.Operation = func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(operation.FieldLeaseUntil)).WriteString(" < now()")
		}))
	}
	// retained is an operation still readable: unfinished, or finished within Retention.
	retained predicate.Operation = func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("(").Ident(s.C(operation.FieldFinishedAt)).WriteString(" IS NULL OR ").
				Ident(s.C(operation.FieldFinishedAt)).WriteString(" > now() - make_interval(secs => ").
				Arg(Retention.Seconds()).WriteString("))")
		}))
	}
	// expired is an operation that finished more than Retention ago.
	expired predicate.Operation = func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.Ident(s.C(operation.FieldFinishedAt)).WriteString(" < now() - make_interval(secs => ").
				Arg(Retention.Seconds()).WriteByte(')')
		}))
	}
)

// found is a single-row ent read as an Operation: nil when there's no row.
func found(e *ent.Operation, err error) (*Operation, error) {
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return fromEnt(e)
}

// fromEnt is an ent row as an Operation, its jsonb read as the raw SQL's ::text did.
func fromEnt(e *ent.Operation) (*Operation, error) {
	o := &Operation{
		ID: e.ID, OrganizationID: e.OrganizationID, ProjectID: e.ProjectID, Type: e.Type, Action: e.Action,
		Status: e.Status, TargetID: e.TargetID, TargetResourceName: e.TargetResourceName, TargetType: e.TargetType,
		Input: json.RawMessage(e.Input), CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
		StartedAt: e.StartedAt, FinishedAt: e.FinishedAt, Attempts: int(e.Attempts),
	}
	if err := json.Unmarshal(e.CreatedBy, &o.CreatedBy); err != nil {
		return nil, fmt.Errorf("operation %s: created_by: %w", o.ID, err)
	}
	if e.Error != nil {
		if err := json.Unmarshal(e.Error, &o.Error); err != nil {
			return nil, fmt.Errorf("operation %s: error: %w", o.ID, err)
		}
	}
	return o, nil
}

// columns and scan read the raw statements' RETURNING (Create, and the worker's claim).
const columns = `id::text, organization_id::text, project_id::text, type, action, status, target_id::text,
	target_resource_name, target_type, created_by::text, input::text, error::text, created_at, updated_at,
	started_at, finished_at, attempts`

// scanner is a *sql.Row or *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scan(row scanner) (*Operation, error) {
	var o Operation
	var createdBy string
	var input string
	var errText *string
	if err := row.Scan(&o.ID, &o.OrganizationID, &o.ProjectID, &o.Type, &o.Action, &o.Status, &o.TargetID,
		&o.TargetResourceName, &o.TargetType, &createdBy, &input, &errText, &o.CreatedAt, &o.UpdatedAt,
		&o.StartedAt, &o.FinishedAt, &o.Attempts); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(createdBy), &o.CreatedBy); err != nil {
		return nil, fmt.Errorf("operation %s: created_by: %w", o.ID, err)
	}
	o.Input = json.RawMessage(input)
	if errText != nil {
		if err := json.Unmarshal([]byte(*errText), &o.Error); err != nil {
			return nil, fmt.Errorf("operation %s: error: %w", o.ID, err)
		}
	}
	return &o, nil
}

// Get reads one of an organization's operations; nil when there's none, or it finished more than
// Retention ago.
func Get(ctx context.Context, q db.Querier, organizationID, id string) (*Operation, error) {
	return found(entdb.Client(q).Operation.Query().
		Where(operation.OrganizationID(organizationID), operation.ID(id), retained).Only(ctx))
}

// lockRunning locks a running operation for its worker's completion; nil when it isn't running
// any more (its lease lapsed and it was reaped). q is the completing transaction: the FOR UPDATE
// serialises completion against a cancel and Abandon (TestEntStatements pins it).
func lockRunning(ctx context.Context, q db.Querier, id string) (*Operation, error) {
	return found(entdb.Client(q).Operation.Query().Where(operation.ID(id), isRunning).ForUpdate().Only(ctx))
}

// lockActive locks a resource's queued or running operation for Abandon; nil when it has none.
// q is Abandon's transaction.
func lockActive(ctx context.Context, q db.Querier, targetID string) (*Operation, error) {
	return found(entdb.Client(q).Operation.Query().Where(operation.TargetID(targetID), isActive).ForUpdate().Only(ctx))
}

// finish moves an operation to a terminal status. The CHECKs keep terminal operations
// consistent (finished_at set exactly when terminal, error exactly when failed).
func finish(ctx context.Context, tx *db.Tx, op *Operation, status string, failure *httpapi.Error) error {
	var errJSON *string
	if failure != nil {
		b, _ := httpapi.Marshal(failure.Body())
		s := string(b)
		errJSON = &s
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE operations SET status = $2, error = $3::text::jsonb, finished_at = now(), updated_at = now(), lease_until = NULL
		WHERE id = $1 AND status IN ('queued', 'running')
		RETURNING updated_at, finished_at`, op.ID, status, errJSON).Scan(&op.UpdatedAt, &op.FinishedAt); err != nil {
		return err
	}
	op.Status = status
	if failure != nil {
		op.Error = failure.Body()
	}
	return nil
}

// Purge deletes operations that finished more than Retention ago.
func Purge(ctx context.Context, q db.Querier) (int64, error) {
	n, err := entdb.Client(q).Operation.Delete().Where(expired).Exec(ctx)
	return int64(n), err
}

// PurgeOrganization deletes all of an organization's operations, finished or not: the
// organization is gone and its closure was sent.
func PurgeOrganization(ctx context.Context, tx *db.Tx, organizationID string) error {
	return purgeOrganization(ctx, tx, organizationID)
}

func purgeOrganization(ctx context.Context, q db.Querier, organizationID string) error {
	_, err := entdb.Client(q).Operation.Delete().Where(operation.OrganizationID(organizationID)).Exec(ctx)
	return err
}

// Abandon ends a resource's active operation, if it has one, as failed with cause, without
// undoing anything: the platform took the resource away itself (its organization or project was
// deleted), so there's nothing to put back.
func Abandon(ctx context.Context, tx *db.Tx, targetID string, cause *httpapi.Error) error {
	op, err := lockActive(ctx, tx, targetID)
	if err != nil || op == nil {
		return err
	}
	return finish(ctx, tx, op, Failed, cause)
}
