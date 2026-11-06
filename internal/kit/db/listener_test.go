package db_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// A Listener's Wait ends at its context's deadline, not CancelDeadlineDelay later (an idle
// connection has no statement for a server-side cancel to end), and the connection still
// receives notifications afterwards: the relays pace their retries and pings by these waits.
func TestListenerWaitEndsAtItsDeadline(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	l, err := d.DB.OpenListener(ctx, "kit-test listener")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Listen(ctx, "kit_test_listener"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		start := time.Now()
		_, err := l.Wait(wctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Wait = %v", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("Wait took %s past a 50 ms deadline", took)
		}
	}
	if _, err := d.DB.ExecContext(ctx, `SELECT pg_notify('kit_test_listener', 'x')`); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if ch, err := l.Wait(wctx); err != nil || ch != "kit_test_listener" {
		t.Fatalf("Wait after the timeouts = %q, %v", ch, err)
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := l.Ping(pctx); err != nil {
		t.Fatal(err)
	}
}
