package logging

import (
	"context"
	"sync"
	"testing"
)

// Review focus 4: a goroutine that outlives the request can still log and annotate, without a race
// and without changing what Finish returned.
func TestScopeAfterFinish(t *testing.T) {
	s := NewScope("req-1")
	ctx := WithScope(context.Background(), s)
	SetRoute(ctx, "/v1/widgets")
	SetCaller(ctx, Caller{Type: "user", ID: "usr_1"})
	route, caller, _ := s.Finish()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			SetCaller(ctx, Caller{Type: "bot", ID: "bot_1"})
			s.SetErrorCode("late")
			_ = contextOf(ctx)
		})
	}
	wg.Wait()
	r2, c2, code := s.Finish()
	if route != "/v1/widgets" || caller.ID != "usr_1" || r2 != route || c2 != caller || code != "" {
		t.Errorf("after finish: %q %v %q", r2, c2, code)
	}
	if SetCaller(context.Background(), Caller{Type: "user"}); ScopeFrom(context.Background()) != nil {
		t.Error("no scope expected")
	}
}
