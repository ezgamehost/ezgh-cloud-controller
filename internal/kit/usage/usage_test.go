package usage_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/usage"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, ok := usage.ParseTime(s)
	if !ok {
		t.Fatalf("not a usage time: %s", s)
	}
	return at
}

// contracts/vectors/usage-id.vectors.json: ID times, IDs of every kind and corrections.
func TestUsageIDVectors(t *testing.T) {
	var file struct {
		Times []struct{ Input, IDTime string } `json:"times"`
		IDs   []struct {
			Name  string `json:"name"`
			Kind  string `json:"kind"`
			Input struct {
				ResourceName string            `json:"resourceName"`
				Meter        string            `json:"meter"`
				ProjectID    *string           `json:"projectId"`
				Dimensions   map[string]string `json:"dimensions"`
				Tags         map[string]string `json:"tags"`
				WindowStart  string            `json:"windowStart"`
				OccurredAt   string            `json:"occurredAt"`
			} `json:"input"`
			ID string `json:"id"`
		} `json:"ids"`
		Corrections []struct {
			OriginalID string `json:"originalId"`
			N          int    `json:"n"`
			ID         string `json:"id"`
		} `json:"corrections"`
	}
	vectors.Load(t, "usage-id.vectors.json", &file)
	for _, c := range file.Times {
		if got, err := usage.IDTimeOf(c.Input); err != nil || got != c.IDTime {
			t.Errorf("IDTime(%s) = %s, %v; want %s", c.Input, got, err, c.IDTime)
		}
	}
	for _, c := range file.IDs {
		in := c.Input
		var got string
		var err error
		switch c.Kind {
		case "interval":
			got, err = usage.IntervalID(in.ResourceName, in.Meter, mustTime(t, in.WindowStart))
		case "delta":
			got, err = usage.DeltaID(in.ResourceName, in.Meter, in.ProjectID, in.Dimensions, in.Tags, mustTime(t, in.WindowStart))
		case "gauge":
			got, err = usage.GaugeID(in.ResourceName, in.Meter, in.ProjectID, in.Dimensions, in.Tags, mustTime(t, in.OccurredAt))
		default:
			t.Fatalf("kind %s", c.Kind)
		}
		if err != nil || got != c.ID {
			t.Errorf("%s:\n got %s (%v)\nwant %s", c.Name, got, err, c.ID)
		}
	}
	for _, c := range file.Corrections {
		if got, err := usage.CorrectionID(c.OriginalID, c.N); err != nil || got != c.ID {
			t.Errorf("correction %d of %s = %s", c.N, c.OriginalID, got)
		}
	}
	if len(file.IDs) == 0 || len(file.Times) == 0 || len(file.Corrections) == 0 {
		t.Fatal("vectors missing")
	}
	if _, err := usage.CorrectionID("x", 0); err == nil {
		t.Error("correction 0 accepted")
	}
}

// contracts/vectors/interval-splits.json.
func TestIntervalSplitVectors(t *testing.T) {
	var file struct {
		ResourceName string `json:"resourceName"`
		Meter        string `json:"meter"`
		Cases        []struct {
			Name, Start, End string
			Expected         []struct{ Start, End, Quantity, ID string } `json:"expected"`
		} `json:"cases"`
	}
	vectors.Load(t, "interval-splits.json", &file)
	for _, c := range file.Cases {
		windows := usage.SplitAtHours(mustTime(t, c.Start), mustTime(t, c.End))
		if len(windows) != len(c.Expected) {
			t.Errorf("%s: %d windows, want %d", c.Name, len(windows), len(c.Expected))
			continue
		}
		for i, w := range windows {
			e := c.Expected[i]
			id, err := usage.IntervalID(file.ResourceName, file.Meter, w.Start)
			if usage.IDTime(w.Start) != e.Start || usage.IDTime(w.End) != e.End || w.Quantity.String() != e.Quantity || err != nil || id != e.ID {
				t.Errorf("%s[%d]: %s–%s %s %s", c.Name, i, usage.IDTime(w.Start), usage.IDTime(w.End), w.Quantity, id)
			}
		}
	}
}

// contracts/vectors/instants.json, the usage form.
func TestUsageTimeVectors(t *testing.T) {
	var file struct {
		Usage struct{ Valid, Invalid []string } `json:"usage"`
	}
	vectors.Load(t, "instants.json", &file)
	for _, s := range file.Usage.Valid {
		if _, ok := usage.ParseTime(s); !ok {
			t.Errorf("refused %s", s)
		}
	}
	for _, s := range file.Usage.Invalid {
		if _, ok := usage.ParseTime(s); ok {
			t.Errorf("accepted %s", s)
		}
	}
}

type issueList []usage.Issue

func sorted(list []usage.Issue) issueList {
	out := slices.Clone(list)
	slices.SortFunc(out, func(a, b usage.Issue) int { return strings.Compare(a.Path+a.Message, b.Path+b.Message) })
	if out == nil {
		out = []usage.Issue{}
	}
	return out
}

// contracts/vectors/usage-events.json: service-kit's validator (the one metering-ingest runs),
// issue for issue: the same paths and the same messages.
func TestUsageEventVectors(t *testing.T) {
	var file struct {
		Context struct {
			Producer string          `json:"producer"`
			Regions  []string        `json:"regions"`
			Now      string          `json:"now"`
			Meters   json.RawMessage `json:"meters"`
		} `json:"context"`
		Valid      []vectorCase `json:"valid"`
		Invalid    []vectorCase `json:"invalid"`
		IngestOnly []vectorCase `json:"ingestOnly"`
	}
	vectors.Load(t, "usage-events.json", &file)
	meters, err := usage.ParseRegistry(file.Context.Meters)
	if err != nil {
		t.Fatal(err)
	}
	now, err := time.Parse(time.RFC3339Nano, file.Context.Now)
	if err != nil {
		t.Fatal(err)
	}
	opts := usage.Options{Producer: file.Context.Producer, Regions: file.Context.Regions, Meters: meters, Now: now}
	check := func(section string, c vectorCase, want []usage.Issue) {
		event, err := canonical.Decode(c.Event)
		if err != nil {
			t.Fatalf("%s %q: %v", section, c.Name, err)
		}
		got := sorted(usage.Validate(event, opts))
		wantSorted := sorted(want)
		if !slices.Equal(got, wantSorted) {
			t.Errorf("%s %q:\n got %v\nwant %v", section, c.Name, got, wantSorted)
		}
	}
	for _, c := range file.Valid {
		check("valid", c, nil)
	}
	for _, c := range file.Invalid {
		if len(c.Issues) == 0 {
			t.Errorf("invalid %q lists no issues", c.Name)
		}
		check("invalid", c, c.Issues)
	}
	// Ingest refuses these with what only it knows (the directory, the billing period, stored
	// events); a producer's own checks pass them.
	for _, c := range file.IngestOnly {
		check("ingestOnly", c, nil)
	}
	if len(file.Valid) == 0 || len(file.Invalid) == 0 {
		t.Fatal("vectors missing")
	}
}

type vectorCase struct {
	Name   string          `json:"name"`
	Event  json.RawMessage `json:"event"`
	Issues []usage.Issue   `json:"issues"`
}

// An event built by the kit validates, and marshals with projectId and correctionOf present.
func TestBuiltEventShape(t *testing.T) {
	project := "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
	name := "ezgh:us-west-1:org_k3f9a0x2m7qp:prj_g7h8i9j0k1l2:wgt_4k2m9x0a7qpe"
	start := mustTime(t, "2026-09-28T10:00:00Z")
	id, _ := usage.IntervalID(name, "widgets.widget.uptime", start)
	e := &usage.Event{
		SpecVersion: usage.SpecVersion, ID: id, Producer: "widgets", Meter: "widgets.widget.uptime",
		OrganizationID: "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", ProjectID: &project, ResourceName: name, Region: "us-west-1",
		Window:   &usage.EventWindow{Start: usage.IDTime(start), End: usage.IDTime(start.Add(time.Hour))},
		Quantity: "3600", Dimensions: map[string]string{"size": "small"}, Tags: map[string]string{},
	}
	raw, _ := json.Marshal(e)
	if !strings.Contains(string(raw), `"correctionOf":null`) || strings.Contains(string(raw), "occurredAt") {
		t.Errorf("marshaled as %s", raw)
	}
	registry, _ := usage.ParseRegistry([]byte(`[{"id":"widgets.widget.uptime","version":1,"service":"widgets","displayName":"Widget uptime","producers":["widgets"],"kind":"interval","unit":"second","aggregation":"sum","window":"hour","resourceType":"wgt","global":false,"dimensions":{"size":{"pricing":true,"required":true,"values":["small","large"]}},"maxQuantity":"3600","expectedCadence":"PT1H","lossTolerant":false,"reconciliationTolerance":"0","status":"shadow"}]`))
	issues, err := usage.ValidateEvent(e, usage.Options{Producer: "widgets", Regions: []string{"us-west-1"}, Meters: registry, Now: start.Add(2 * time.Hour)})
	if err != nil || len(issues) != 0 {
		t.Errorf("issues %v, %v", issues, err)
	}
}
