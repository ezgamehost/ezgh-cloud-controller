package quota_test

import (
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
)

// contracts/vectors/quotas.json: billing periods, value resolution and bucket capacities, as
// scripts/lib/quotas.mjs computes them.
func TestQuotasVectors(t *testing.T) {
	var file struct {
		BillingPeriods []struct {
			Instant   time.Time `json:"instant"`
			AnchorDay int       `json:"anchorDay"`
			Start     time.Time `json:"start"`
			End       time.Time `json:"end"`
		} `json:"billingPeriods"`
		Values struct {
			Now       time.Time        `json:"now"`
			Quotas    []manifest.Quota `json:"quotas"`
			Overrides []quota.Override `json:"overrides"`
			Cases     []struct {
				Name           string         `json:"name"`
				Quota          string         `json:"quota"`
				OrganizationID string         `json:"organizationId"`
				ScopeKey       string         `json:"scopeKey"`
				Region         string         `json:"region"`
				Tier           string         `json:"tier"`
				Expected       quota.Resolved `json:"expected"`
			} `json:"cases"`
		} `json:"values"`
		BucketCapacity []struct {
			Default  int   `json:"default"`
			Burst    *int  `json:"burst"`
			Value    int64 `json:"value"`
			Capacity int64 `json:"capacity"`
		} `json:"bucketCapacity"`
	}
	vectors.Load(t, "quotas.json", &file)
	if len(file.BillingPeriods) == 0 || len(file.Values.Cases) == 0 || len(file.BucketCapacity) == 0 {
		t.Fatal("quotas.json has no cases")
	}
	for _, c := range file.BillingPeriods {
		start, end := quota.BillingPeriod(c.Instant, c.AnchorDay)
		if !start.Equal(c.Start) || !end.Equal(c.End) {
			t.Errorf("billing period of %s, anchor %d: %s – %s, want %s – %s", c.Instant, c.AnchorDay, start, end, c.Start, c.End)
		}
	}
	byID := map[string]manifest.Quota{}
	for _, q := range file.Values.Quotas {
		byID[q.ID] = q
	}
	for _, c := range file.Values.Cases {
		q, ok := byID[c.Quota]
		if !ok {
			t.Fatalf("%s: no quota %s", c.Name, c.Quota)
		}
		got := quota.ResolveValue(q, c.OrganizationID, c.ScopeKey, c.Region, c.Tier, file.Values.Overrides, file.Values.Now)
		if got != c.Expected {
			t.Errorf("%s: %+v, want %+v", c.Name, got, c.Expected)
		}
	}
	for _, c := range file.BucketCapacity {
		q := manifest.Quota{Kind: manifest.Rate, Default: c.Default, Burst: c.Burst}
		if got := quota.BucketCapacity(q, c.Value); got != c.Capacity {
			t.Errorf("capacity of default %d burst %v value %d: %d, want %d", c.Default, c.Burst, c.Value, got, c.Capacity)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"PT1S": time.Second, "PT1M": time.Minute, "PT90S": 90 * time.Second, "PT60M": time.Hour, "PT1H": time.Hour} {
		if got, err := quota.ParseDuration(in); err != nil || got != want {
			t.Errorf("%s = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "PT", "PT0S", "PT2H", "PT61M", "P1D", "PT1H30M", "PT3601S"} {
		if _, err := quota.ParseDuration(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
