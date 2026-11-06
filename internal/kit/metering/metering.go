// Package metering is a producer's side of billing-stack's metering (contracts/metering.md):
// writing usage events to the usage outbox with their control totals, relaying them to
// metering-ingest, watermarks, and organization closures.
//
// A product reports quantities, never money. Every usage event is written in the same
// transaction as the state change it comes from, with a deterministic ID, so the final interval
// of a stopped widget exists exactly when the stop committed, and a checkpoint that runs twice
// writes nothing the second time.
package metering

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/decimal"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/outbox"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/usage"
)

// InvalidEvent is an event that failed validation. It aborts the transaction it was written in:
// ingest would reject it anyway, and failing loudly at the source is how the bug gets fixed.
type InvalidEvent struct {
	ID     string
	Issues []usage.Issue
}

func (e *InvalidEvent) Error() string {
	var parts []string
	for _, i := range e.Issues {
		parts = append(parts, fmt.Sprintf("%s: %s", i.Path, i.Message))
	}
	return fmt.Sprintf("usage event %s is invalid: %s", e.ID, strings.Join(parts, "; "))
}

// Producer writes a service's usage events.
type Producer struct {
	// Name is the producer (the manifest's metering.producer), Region the deployment's.
	Name    string
	Region  string
	Regions []string
	Meters  usage.Registry
	// Now is the clock validation checks "not in the future" against; nil is time.Now.
	Now func() time.Time
}

func (p *Producer) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Insert validates e and writes it to usage_outbox in tx, the transaction of the change it comes
// from. It returns false when the outbox already has an event with this ID (a checkpoint that ran
// twice) and writes nothing. Control totals count only rows actually inserted.
func (p *Producer) Insert(ctx context.Context, tx *db.Tx, e *usage.Event) (bool, error) {
	if e.SpecVersion == "" {
		e.SpecVersion = usage.SpecVersion
	}
	if e.Dimensions == nil {
		e.Dimensions = map[string]string{}
	}
	if e.Tags == nil {
		e.Tags = map[string]string{}
	}
	issues, err := usage.ValidateEvent(e, usage.Options{Producer: p.Name, Regions: p.Regions, Meters: p.Meters, Now: p.now()})
	if err != nil {
		return false, err
	}
	if len(issues) > 0 {
		return false, &InvalidEvent{ID: e.ID, Issues: issues}
	}
	body, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	inserted, err := db.RowsAffected(tx.ExecContext(ctx, `INSERT INTO usage_outbox (id, event) VALUES ($1, $2::text::jsonb) ON CONFLICT (id) DO NOTHING`, e.ID, string(body)))
	if err != nil {
		return false, err
	}
	if inserted == 0 {
		return false, nil
	}
	hour := e.UsageTime().Truncate(time.Hour)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO usage_control_totals (usage_hour, meter, events, quantity, changed_at) VALUES ($1, $2, 1, $3::numeric, now())
		ON CONFLICT (usage_hour, meter) DO UPDATE
		SET events = usage_control_totals.events + 1, quantity = usage_control_totals.quantity + excluded.quantity,
			changed_at = now()`,
		hour, e.Meter, e.Quantity)
	return err == nil, err
}

// credentialHeader sets the producer key.
func credentialHeader(credential string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+credential) }
}

// NewRelay is the usage relay to metering-ingest's POST /internal/v1/usage/events, with the
// producer's pk_ credential. Accepted and duplicate events are deleted (both are in metering's
// hands); rejected ones are kept and never resent; a 413 splits the batch; 400, 401, 503 and no
// answer back off and resend the whole batch, which is safe.
func NewRelay(d *db.DB, meteringURL, credential string, logger *slog.Logger) *outbox.Relay {
	return &outbox.Relay{
		DB:        d,
		Table:     "usage_outbox",
		Name:      "usage relay",
		Target:    "metering-ingest",
		URL:       meteringURL + "/internal/v1/usage/events",
		Authorize: credentialHeader(credential),
		// In the order the usage happened; ties by ID, so a correction follows its original.
		Order: func(event json.RawMessage) string {
			var e usage.Event
			_ = json.Unmarshal(event, &e)
			return fmt.Sprintf("%016d %s", e.UsageTime().UnixMilli(), e.ID)
		},
		BatchSize: outbox.DefaultBatchSize,
		Logger:    logger,
		Explain: func(issues []outbox.Issue) string {
			for _, i := range issues {
				if strings.HasPrefix(i.Message, "idempotency_conflict") {
					return "idempotency_conflict: metering-ingest already has a different event with this id. That's a producer bug: the same id must always mean the same usage. Never give the event a new id"
				}
			}
			return ""
		},
	}
}

// Client calls metering-ingest's other producer endpoints.
type Client struct {
	URL        string
	Credential string
	HTTP       *http.Client
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	credentialHeader(c.Credential)(req)
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("metering-ingest unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		return nil
	}
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &RefusedError{Status: resp.StatusCode, Body: string(answer)}
}

// RefusedError is metering-ingest answering something other than success.
type RefusedError struct {
	Status int
	Body   string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("metering-ingest answered %d: %s", e.Status, e.Body)
}

// Watermark is one "complete through" report (schemas/watermark.schema.json).
type Watermark struct {
	Producer        string            `json:"producer"`
	Region          string            `json:"region"`
	CompleteThrough string            `json:"completeThrough"`
	Counts          map[string]Counts `json:"counts"`
}

// Counts are one meter's control totals for the hour ending at completeThrough.
type Counts struct {
	Events   int64  `json:"events"`
	Quantity string `json:"quantity"`
}

// SendWatermark posts a watermark.
func (c *Client) SendWatermark(ctx context.Context, w Watermark) error {
	return c.post(ctx, "/internal/v1/usage/watermarks", w)
}

// Closure says the producer is done with a deleted organization
// (schemas/organization-closure.schema.json).
type Closure struct {
	Producer        string `json:"producer"`
	OrganizationID  string `json:"organizationId"`
	CompleteThrough string `json:"completeThrough"`
}

// SendClosure posts a closure.
func (c *Client) SendClosure(ctx context.Context, cl Closure) error {
	return c.post(ctx, "/internal/v1/usage/organization-closures", cl)
}

// ControlTotals reads the totals for the hour that starts at hour, for each meter given (zero for
// a meter with no events that hour: "nothing" is a count too).
func ControlTotals(ctx context.Context, q db.Querier, hour time.Time, meters []string) (map[string]Counts, error) {
	counts := map[string]Counts{}
	for _, m := range meters {
		counts[m] = Counts{Events: 0, Quantity: "0"}
	}
	rows, err := q.QueryContext(ctx, `SELECT meter, events, quantity::text FROM usage_control_totals WHERE usage_hour = $1`, hour)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var meter, quantity string
		var events int64
		if err := rows.Scan(&meter, &events, &quantity); err != nil {
			return nil, err
		}
		canonical, ok := decimal.Canonical(quantity)
		if !ok {
			return nil, fmt.Errorf("control totals: %s isn't a decimal", quantity)
		}
		if _, owned := counts[meter]; owned || len(meters) == 0 {
			counts[meter] = Counts{Events: events, Quantity: canonical}
		}
	}
	return counts, rows.Err()
}
