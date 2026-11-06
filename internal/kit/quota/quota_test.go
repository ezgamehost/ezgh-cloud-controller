package quota_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	kitdb "github.com/ezgamehost/ezgh-cloud-controller/internal/kit/db"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

func reserve(db *testdb.DB, c quota.Counter, amount int64, limit quota.Limit) error {
	return db.DB.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error {
		return quota.Reserve(context.Background(), tx, c, amount, limit)
	})
}

// Concurrent requests, each with the value Quotas resolved for it in its check, never get past it:
// they serialize on the counter row.
func TestAllocationIsExactUnderConcurrencyWithAResolvedValue(t *testing.T) {
	db := testdb.New(t)
	fake, e, _ := setup(t)
	fake.SetValue(perProject.ID, project, 7)
	fake.SetValue(perMinute.ID, "", 1000)
	fake.SetValue(inFlight.ID, "", 1000)
	counter := quota.Counter{Quota: perProject, OrganizationID: org, ScopeKey: project, ResourceName: prjName}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := e.Check(context.Background(), e.Plan("POST", create), request)
			if err == nil {
				defer g.Release()
				limit, found := g.Limit(perProject.ID, project)
				if !found || limit.Value != 7 || !limit.Enforced {
					t.Errorf("limit %+v", limit)
				}
				err = reserve(db, counter, 1, limit)
			}
			mu.Lock()
			defer mu.Unlock()
			var he *httpapi.Error
			switch {
			case err == nil:
				ok++
			case errors.As(err, &he) && he.Code == "quota_exceeded" && he.Status == 409:
				refused++
				q := he.Extra["quota"].(map[string]any)
				if !equal(q, map[string]any{"id": perProject.ID, "kind": "allocation", "scope": "project", "scopeResourceName": prjName, "limit": int64(7), "used": int64(7)}) {
					t.Errorf("quota %v", q)
				}
				if he.Message != "This project already has 7 widgets, and can have at most 7 ("+perProject.ID+")" {
					t.Errorf("message %q", he.Message)
				}
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 7 || refused != 13 {
		t.Errorf("%d reserved, %d refused", ok, refused)
	}
	if used, err := quota.Used(context.Background(), db.DB, perProject.ID, project, ""); err != nil || used != 7 {
		t.Errorf("used %d (%v)", used, err)
	}
}

// Failing open still counts, without the condition: usage stays exact, and once Quotas answers
// again, creates are refused until usage is back under the value.
func TestAllocationFailingOpenStillCounts(t *testing.T) {
	db := testdb.New(t)
	fake, e, _ := setup(t)
	fake.SetValue(perProject.ID, project, 2)
	counter := quota.Counter{Quota: perProject, OrganizationID: org, ScopeKey: project}
	fake.Fail(503, "")
	for range 3 {
		g, _ := e.Check(context.Background(), e.Plan("POST", create), request)
		limit, _ := g.Limit(perProject.ID, project)
		if err := reserve(db, counter, 1, limit); err != nil {
			t.Fatalf("failing open refused: %v", err)
		}
	}
	fake.Fail(0, "")
	g, err := e.Check(context.Background(), e.Plan("POST", create), request)
	if err != nil {
		t.Fatal(err)
	}
	g.Release()
	limit, _ := g.Limit(perProject.ID, project)
	err = reserve(db, counter, 1, limit)
	var he *httpapi.Error
	if !errors.As(err, &he) || he.Extra["quota"].(map[string]any)["used"] != int64(3) {
		t.Fatalf("over the value once Quotas is back: %v", err)
	}
	if used, _ := quota.Used(context.Background(), db.DB, perProject.ID, project, ""); used != 3 {
		t.Errorf("used %d", used)
	}
}

func TestReleaseRegionsAndUsage(t *testing.T) {
	db := testdb.New(t)
	running := perProject
	running.ID, running.Scope, running.PerRegion = "widgets.runningPerOrganization", "organization", true
	west := quota.Counter{Quota: running, OrganizationID: org, ScopeKey: org, Region: "us-west-1"}
	east := west
	east.Region = "us-east-1"
	limit := quota.Limit{Value: 3, Enforced: true}
	for _, c := range []quota.Counter{west, west, east} {
		if err := reserve(db, c, 1, limit); err != nil {
			t.Fatal(err)
		}
	}
	err := reserve(db, west, 2, limit)
	var he *httpapi.Error
	if !errors.As(err, &he) || he.Extra["quota"].(map[string]any)["region"] != "us-west-1" {
		t.Fatalf("2 more in us-west-1: %v", err)
	}
	if err := reserve(db, quota.Counter{Quota: largePerOrg, OrganizationID: org, ScopeKey: org}, 1, limit); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error {
		return quota.Release(context.Background(), tx, largePerOrg.ID, org, "", 1)
	}); err != nil {
		t.Fatal(err)
	}
	// Another organization's counters aren't listed.
	if err := reserve(db, quota.Counter{Quota: largePerOrg, OrganizationID: project, ScopeKey: project}, 1, limit); err != nil {
		t.Fatal(err)
	}
	usage, err := quota.UsageOf(context.Background(), db.DB, org)
	if err != nil {
		t.Fatal(err)
	}
	want := []quota.UsageRow{
		{Quota: largePerOrg.ID, ScopeKey: org, Region: "", Used: 0},
		{Quota: running.ID, ScopeKey: org, Region: "us-east-1", Used: 1},
		{Quota: running.ID, ScopeKey: org, Region: "us-west-1", Used: 2},
	}
	if len(usage) != len(want) {
		t.Fatalf("usage %+v", usage)
	}
	for i := range want {
		if usage[i] != want[i] {
			t.Errorf("usage[%d] = %+v, want %+v", i, usage[i], want[i])
		}
	}
	// Going below zero is a bug the CHECK refuses.
	if err := db.DB.RunTx(context.Background(), nil, func(tx *kitdb.Tx) error {
		return quota.Release(context.Background(), tx, largePerOrg.ID, org, "", 1)
	}); err == nil {
		t.Error("released below zero")
	}
	// The kit's migration made organization_id NOT NULL on a database without older counters.
	var nullable string
	if err := db.Pool.QueryRow(context.Background(), `SELECT is_nullable FROM information_schema.columns WHERE table_name = 'quota_usage' AND column_name = 'organization_id'`).Scan(&nullable); err != nil || nullable != "NO" {
		t.Errorf("organization_id nullable %q (%v)", nullable, err)
	}
}
