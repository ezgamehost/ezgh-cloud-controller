package usage

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
)

// SpecVersion is the usage event format this kit writes.
const SpecVersion = "1.0"

// Event is one usage report: a quantity of one meter, for one resource, at an instant or over a
// window inside one UTC hour. Field order is the contract's; projectId and correctionOf are
// always written (null when empty), occurredAt and window only when set.
type Event struct {
	SpecVersion    string            `json:"specVersion"`
	ID             string            `json:"id"`
	Producer       string            `json:"producer"`
	Meter          string            `json:"meter"`
	OrganizationID string            `json:"organizationId"`
	ProjectID      *string           `json:"projectId"`
	ResourceName   string            `json:"resourceName"`
	Region         string            `json:"region"`
	OccurredAt     *string           `json:"occurredAt,omitempty"`
	Window         *EventWindow      `json:"window,omitempty"`
	Quantity       string            `json:"quantity"`
	Dimensions     map[string]string `json:"dimensions"`
	Tags           map[string]string `json:"tags"`
	CorrectionOf   *string           `json:"correctionOf"`
}

// EventWindow is [start, end), both written as IDTime writes them.
type EventWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// UsageTime is when the usage happened: the window's start or the instant. It decides the
// usage hour (control totals) and the relay's send order.
func (e *Event) UsageTime() time.Time {
	var at string
	if e.Window != nil {
		at = e.Window.Start
	} else if e.OccurredAt != nil {
		at = *e.OccurredAt
	}
	t, _ := ParseTime(at)
	return t
}

// Generic is the event as generic JSON values, which is what validation reads (so it judges
// exactly what would be sent).
func (e *Event) Generic() (any, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	return canonical.Decode(raw)
}

// Meter is one meter in billing-stack's registry (catalog/meters/<namespace>.json, also served at
// metering-ingest's GET /internal/v1/meters).
type Meter struct {
	ID                      string               `json:"id"`
	Version                 int                  `json:"version"`
	Service                 string               `json:"service"`
	DisplayName             string               `json:"displayName"`
	Producers               []string             `json:"producers"`
	Kind                    string               `json:"kind"`
	Unit                    string               `json:"unit"`
	Aggregation             string               `json:"aggregation"`
	Window                  string               `json:"window"`
	ResourceType            string               `json:"resourceType"`
	Global                  bool                 `json:"global"`
	Dimensions              map[string]Dimension `json:"dimensions"`
	MaxQuantity             string               `json:"maxQuantity"`
	MaxGap                  string               `json:"maxGap,omitempty"`
	ExpectedCadence         string               `json:"expectedCadence"`
	LossTolerant            bool                 `json:"lossTolerant"`
	ReconciliationTolerance string               `json:"reconciliationTolerance"`
	Status                  string               `json:"status"`
	DeprecatedAt            string               `json:"deprecatedAt,omitempty"`
}

// Dimension is one of a meter's dimensions. Pricing dimensions select a price, so their values
// are a closed set.
type Dimension struct {
	Pricing  bool     `json:"pricing"`
	Required bool     `json:"required"`
	Values   []string `json:"values,omitempty"`
	Pattern  *string  `json:"pattern,omitempty"`
}

// Meter kinds.
const (
	Delta    = "delta"
	Interval = "interval"
	Gauge    = "gauge"
)

// Registry is the meters a producer validates against, by ID.
type Registry map[string]Meter

// ParseRegistry reads either billing-stack's catalog file (an array of meters) or the registry
// endpoint's { meters, producers }.
func ParseRegistry(data []byte) (Registry, error) {
	var meters []Meter
	if trimmed := strings.TrimSpace(string(data)); strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(data, &meters); err != nil {
			return nil, fmt.Errorf("usage: reading the meter catalog: %w", err)
		}
	} else {
		var list struct {
			Meters []Meter `json:"meters"`
		}
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("usage: reading the meter registry: %w", err)
		}
		meters = list.Meters
	}
	r := Registry{}
	for _, m := range meters {
		if _, dup := r[m.ID]; dup {
			return nil, fmt.Errorf("usage: meter %s is listed twice", m.ID)
		}
		if m.MaxQuantity == "" {
			return nil, fmt.Errorf("usage: meter %s has no maxQuantity", m.ID)
		}
		r[m.ID] = m
	}
	return r, nil
}

// Owned is the meters producer reports, sorted by ID.
func (r Registry) Owned(producer string) []Meter {
	var out []Meter
	for _, m := range r {
		if slices.Contains(m.Producers, producer) {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Meter) int { return strings.Compare(a.ID, b.ID) })
	return out
}
