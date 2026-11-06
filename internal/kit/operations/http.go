package operations

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/instant"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/operation"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/ent/predicate"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/entdb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resource"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
)

// Register serves a product's operations (contracts/operations.md, "Endpoints"):
//
//	GET    /v1/organizations/:orgId/operations/:operationId   poll (not recorded in Trails)
//	DELETE /v1/organizations/:orgId/operations/:operationId   cancel (CancelOperation)
//	GET    /v1/organizations/:orgId/operations                 list (ListOperations)
//
// handlers undo a cancelled operation's target, as the worker does for a failed one. It also sets
// the service's ReplayOperation, so an Idempotency-Key replay answers the operation as it is now.
func Register(s *service.Service, handlers map[string]Handler) {
	a := &api{handlers: handlers}
	s.Handle("GET", "/v1/organizations/:orgId/operations/:operationId", a.get)
	s.Handle("DELETE", "/v1/organizations/:orgId/operations/:operationId", a.cancel)
	s.Handle("GET", "/v1/organizations/:orgId/operations", a.list)
	s.ReplayOperation = func(ctx context.Context, q db.Querier, organizationID, operationID string) (any, error) {
		op, err := Get(ctx, q, organizationID, operationID)
		if err != nil || op == nil {
			return nil, err
		}
		return op.View(), nil
	}
}

type api struct {
	handlers map[string]Handler
}

// getAction is the target's get action, which reading an operation needs: the first action, in
// the manifest's order, on the target's resource type whose ID ends in .get (Widget →
// widgets.widgets.get). The resource type is the one whose prefix the target's slug has, since a
// Trails type needn't be its name (gtr_ → repository, Trails type GitRepository); a target whose
// slug names none falls back to the type's name.
func getAction(c *service.Call, op *Operation) (string, error) {
	m := c.Service.Manifest
	typeName := op.TargetType
	if prefix, _, ok := strings.Cut(resource.LastSlug(op.TargetResourceName), "_"); ok {
		for _, t := range m.ResourceTypes {
			if t.Prefix == prefix {
				typeName = t.Name
			}
		}
	}
	for _, a := range m.Actions {
		if strings.EqualFold(a.Resource, typeName) && strings.HasSuffix(a.ID, ".get") {
			return a.ID, nil
		}
	}
	return "", httpapi.Internal(fmt.Errorf("the manifest has no .get action for %s", op.TargetType))
}

// listAction is the manifest's action for listing operations, which a missing operation is
// checked with (so membership is decided before answering 404).
func listAction(c *service.Call) string {
	if e, ok := c.Service.Manifest.Endpoint("GET", "/v1/organizations/:orgId/operations"); ok {
		return e.Action
	}
	return c.Service.Manifest.Namespace + ".operations.list"
}

// visible finds the operation and checks the caller may see it: the target type's get action on
// the target. One the caller can't see is 404, exactly like a missing one, after the membership
// check.
func visible(c *service.Call, cancel bool) (*Operation, bool, error) {
	id := c.Param("operationId")
	op, err := Get(c.Ctx, c.Service.DB, c.OrganizationID, id)
	if err != nil {
		return nil, false, httpapi.Unavailable("The database is unavailable", err)
	}
	if op == nil {
		return nil, false, c.AuthorizeMissing(listAction(c), "Operation")
	}
	c.ProjectID = op.ProjectID
	get, err := getAction(c, op)
	if err != nil {
		return nil, false, err
	}
	// One question to org-api: may the caller see it, and (to cancel) do its action.
	actions := []string{get}
	if cancel && op.Action != get {
		actions = append(actions, op.Action)
	}
	perm, err := c.Check(actions, op.TargetResourceName)
	if err != nil {
		return nil, false, err
	}
	if !perm.Allows(get) {
		return nil, false, httpapi.NotFound("Operation")
	}
	return op, !cancel || perm.Allows(op.Action), nil
}

func (a *api) get(c *service.Call) error {
	op, _, err := visible(c, false)
	if err != nil {
		return err
	}
	if !op.Terminal() {
		c.W.Header().Set("Retry-After", strconv.Itoa(PollSeconds))
	}
	v, err := a.view(c, op)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, v)
	return nil
}

// view is op's View with its handler's say on whether it can still be cancelled (Canceller).
func (a *api) view(c *service.Call, op *Operation) (View, error) {
	v := op.View()
	if !v.Cancellable {
		return v, nil
	}
	can, err := a.canCancel(c, c.Service.DB, op)
	if err != nil {
		return v, err
	}
	v.Cancellable = can
	return v, nil
}

// canCancel asks a Canceller handler whether queued op can still be cancelled; other handlers can
// always undo a queued operation.
func (a *api) canCancel(c *service.Call, q db.Querier, op *Operation) (bool, error) {
	h, ok := a.handlers[op.Type].(Canceller)
	if !ok {
		return true, nil
	}
	can, err := h.CanCancel(c.Ctx, q, op)
	if err != nil {
		return false, httpapi.Unavailable("The operation can't be read right now", err)
	}
	return can, nil
}

// cancel is DELETE on an operation: authorized with the operation's own action on its target.
// Cancelling is answered 202 with the operation, as every asynchronous change is.
func (a *api) cancel(c *service.Call) error {
	op, allowed, err := visible(c, true)
	if err != nil {
		return err
	}
	if !allowed {
		return httpapi.AccessDenied(op.Action, c.Permissions().APIKeyID != nil)
	}
	return c.Transact(func(tx *db.Tx) (*service.Response, error) {
		current, err := lockForCancel(c.Ctx, tx, op.ID)
		if err != nil {
			return nil, err
		}
		switch {
		case current.Terminal():
			return nil, httpapi.Conflict("operation_finished", "The operation has already finished")
		case !current.Cancellable():
			return nil, httpapi.Conflict("operation_not_cancellable", "The operation is already running and can't be cancelled")
		}
		if can, err := a.canCancel(c, tx, current); err != nil || !can {
			if err != nil {
				return nil, err
			}
			return nil, httpapi.Conflict("operation_not_cancellable", "The operation can no longer be undone, so it can't be cancelled")
		}
		if h, ok := a.handlers[current.Type]; ok {
			if err := h.Undo(c.Ctx, tx, current, nil); err != nil {
				return nil, err
			}
		}
		if err := finish(c.Ctx, tx, current, Cancelled, nil); err != nil {
			return nil, err
		}
		return &service.Response{Status: http.StatusAccepted, Body: current.View(), Location: current.Location()}, nil
	})
}

// lockForCancel locks the operation a cancel acts on, whatever its status: the FOR UPDATE
// serialises the cancel against the worker's completion (TestEntStatements pins it). q is the
// cancel's transaction; a missing row is ent's not-found error (500, as sql.ErrNoRows was).
func lockForCancel(ctx context.Context, q db.Querier, id string) (*Operation, error) {
	e, err := entdb.Client(q).Operation.Query().Where(operation.ID(id)).ForUpdate().Only(ctx)
	if err != nil {
		return nil, err
	}
	return fromEnt(e)
}

var statuses = []string{Queued, Running, Succeeded, Failed, Cancelled}

// list is an organization's operations, newest first, keyset-paged.
func (a *api) list(c *service.Call) error {
	if err := c.Authorize(c.Endpoint.Action, ""); err != nil {
		return err
	}
	q := c.R.URL.Query()
	var issues []httpapi.Issue
	limit, limitErr := httpapi.Limit(q)
	if limitErr != nil {
		issues = append(issues, limitErr.Issues...)
	}
	status := q.Get("status")
	if status != "" && !slices.Contains(statuses, status) {
		issues = append(issues, httpapi.Issue{Path: "status", Message: "must be one of " + strings.Join(statuses, ", ")})
	}
	target := q.Get("targetResourceName")
	for key := range q {
		if !slices.Contains([]string{"limit", "cursor", "status", "targetResourceName"}, key) {
			issues = append(issues, httpapi.Issue{Path: key, Message: "isn't a parameter of this list"})
		}
	}
	if len(issues) > 0 {
		return httpapi.Invalid(issues)
	}
	// The scope binds a cursor to this list and these filters.
	scope := "operations:status=" + status + ":target=" + target
	var after *keyset
	if raw := q.Get("cursor"); raw != "" {
		var key []string
		if err := c.Service.Cursors.Decode(c.OrganizationID, scope, raw, &key); err != nil || len(key) != 2 {
			return httpapi.InvalidCursor()
		}
		at, ok := instant.ParseResponse(key[0])
		if !ok {
			return httpapi.InvalidCursor()
		}
		after = &keyset{at: at, id: key[1]}
	}
	// Any error reading the page is 503, as GET's read is: ent's All returns the query's error
	// and a failure while reading its rows alike (the raw loop answered the latter 500).
	rows, err := listPage(c.Ctx, c.Service.DB, c.OrganizationID, status, target, after, limit+1)
	if err != nil {
		return httpapi.Unavailable("The database is unavailable", err)
	}
	page := make([]*Operation, 0, len(rows))
	for _, e := range rows {
		op, err := fromEnt(e)
		if err != nil {
			return httpapi.Internal(err)
		}
		page = append(page, op)
	}
	var next *string
	if len(page) > limit {
		page = page[:limit]
		last := page[len(page)-1]
		cursor, err := c.Service.Cursors.Encode(c.OrganizationID, scope, []string{instant.Format(last.CreatedAt), last.ID})
		if err != nil {
			return httpapi.Internal(err)
		}
		next = &cursor
	}
	views := make([]View, len(page))
	for i, op := range page {
		v, err := a.view(c, op)
		if err != nil {
			return err
		}
		views[i] = v
	}
	c.JSON(http.StatusOK, map[string]any{"operations": views, "nextCursor": next})
	return nil
}

// keyset is a list cursor's position: the last operation shown's createdAt (to the millisecond)
// and id.
type keyset struct {
	at time.Time
	id string
}

// listPage is up to n of an organization's readable operations, newest first, after the cursor's
// position when there is one, and filtered by status and target resource name when they aren't "".
func listPage(ctx context.Context, q db.Querier, organizationID, status, target string, after *keyset, n int) ([]*ent.Operation, error) {
	query := entdb.Client(q).Operation.Query().
		Where(operation.OrganizationID(organizationID), retained).
		Order(newestFirst, operation.ByID(entsql.OrderDesc())).
		Limit(n)
	// The filter matches PublicStatus: a parked operation (queued, started) lists as running.
	switch status {
	case "":
	case Queued:
		query.Where(operation.Status(Queued), operation.StartedAtIsNil())
	case Running:
		query.Where(operation.Or(operation.Status(Running), operation.And(operation.Status(Queued), operation.StartedAtNotNil())))
	default:
		query.Where(operation.Status(status))
	}
	if target != "" {
		query.Where(operation.TargetResourceName(target))
	}
	if after != nil {
		query.Where(before(after.at, after.id))
	}
	return query.All(ctx)
}

// created_at is stored to the microsecond but shown to the millisecond: the list orders and
// pages on the millisecond, as its cursor has it.

// newestFirst orders by date_trunc('milliseconds', created_at) DESC (then id DESC).
func newestFirst(s *entsql.Selector) {
	s.OrderExprFunc(func(b *entsql.Builder) {
		b.WriteString("date_trunc('milliseconds', ").Ident(s.C(operation.FieldCreatedAt)).WriteString(") DESC")
	})
}

// before is the keyset after the cursor's operation: (date_trunc('milliseconds', created_at), id)
// < (at, id::uuid).
func before(at time.Time, id string) predicate.Operation {
	return func(s *entsql.Selector) {
		s.Where(entsql.P(func(b *entsql.Builder) {
			b.WriteString("(date_trunc('milliseconds', ").Ident(s.C(operation.FieldCreatedAt)).WriteString("), ").
				Ident(s.C(operation.FieldID)).WriteString(") < (").Arg(at).WriteString(", ").Arg(id).WriteString("::uuid)")
		}))
	}
}
