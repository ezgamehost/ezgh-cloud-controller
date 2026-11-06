package signals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

const group = "test-controller"

// One signal that always fails is dropped in the end, and never holds up the ones behind it.
func TestPoisonSignalDoesntStall(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx := context.Background()
	stream := fmt.Sprintf("databases:test:poison:%d", time.Now().UnixNano())
	p := &Publisher{Redis: client, Stream: stream}
	var mu sync.Mutex
	tried, good := 0, 0
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	c := &Consumer{Redis: client, Stream: stream, Group: "test-controller", Name: "a", Logger: testdb.Logger(), SweepEvery: 20 * time.Millisecond, Handle: func(_ context.Context, s Signal) error {
		mu.Lock()
		defer mu.Unlock()
		if s.Node == "ins_bad" {
			tried++
			return errors.New("never")
		}
		good++
		return nil
	}}
	go func() { defer close(done); c.Run(run) }()
	t.Cleanup(func() { stop(); <-done })
	if err := p.Publish(ctx, Signal{Node: "ins_bad"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Publish(ctx, Signal{Node: "ins_good"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		finished := tried == maxAttempts && good == 1
		mu.Unlock()
		if finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d tries, %d good", tried, good)
		}
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		reply, err := client.Do(ctx, "XPENDING", stream, group)
		if err == nil && reply.([]any)[0] == int64(0) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still pending: %v, %v", reply, err)
		}
	}
}

// What a replica that vanished was reading is taken over.
func TestVanishedReplicaIsClaimed(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx := context.Background()
	stream := fmt.Sprintf("databases:test:claim:%d", time.Now().UnixNano())
	if _, err := client.Do(ctx, "XGROUP", "CREATE", stream, group, "0", "MKSTREAM"); err != nil {
		t.Fatal(err)
	}
	if err := (&Publisher{Redis: client, Stream: stream}).Publish(ctx, Signal{Node: "ins_a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(ctx, "XREADGROUP", "GROUP", group, "gone", "COUNT", "1", "STREAMS", stream, ">"); err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 1)
	c := &Consumer{Redis: client, Stream: stream, Group: "test-controller", Name: "b", Logger: testdb.Logger(), SweepEvery: 20 * time.Millisecond, ClaimAfter: 1,
		Handle: func(_ context.Context, s Signal) error { seen <- s.Node; return nil }}
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(run) }()
	t.Cleanup(func() { stop(); <-done })
	select {
	case <-seen:
	case <-time.After(3 * time.Second):
		t.Fatal("never claimed")
	}
}

func TestStreamDeliversAndRetries(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx := context.Background()
	stream := fmt.Sprintf("databases:test:%d", time.Now().UnixNano())
	p := &Publisher{Redis: client, Stream: stream}
	// Before anything reads: a controller that comes up later still hears it.
	if err := p.Publish(ctx, Signal{Kind: "pressure", Node: "ins_early"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var failOnce sync.Once
	handle := func(_ context.Context, s Signal) error {
		var failed bool
		failOnce.Do(func() { failed = true })
		if failed {
			return errors.New("not yet")
		}
		mu.Lock()
		defer mu.Unlock()
		seen[s.Node]++
		return nil
	}
	run, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	c := &Consumer{Redis: client, Stream: stream, Group: "test-controller", Name: "a", Handle: handle, Logger: testdb.Logger(), SweepEvery: 20 * time.Millisecond}
	go func() { defer close(done); c.Run(run) }()
	t.Cleanup(func() { stop(); <-done })
	for i := range 5 {
		if err := p.Publish(ctx, Signal{Kind: "pressure", Node: fmt.Sprintf("ins_%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// The failed one comes back, once the session restarts, and every one is acknowledged. The ack
	// follows Handle, so it's waited for, not read once the sixth is seen.
	for deadline := time.Now().Add(8 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		mu.Lock()
		n := len(seen)
		mu.Unlock()
		reply, err := client.Do(ctx, "XPENDING", stream, group)
		if err != nil {
			t.Fatal(err)
		}
		pending := reply.([]any)[0]
		if n == 6 && pending == int64(0) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 6, %v pending", n, pending)
		}
	}
}

// A signal stuck on Private CA holds up what issues, and never what failover reads: each has its own
// stream, and so its own reader.
func TestIssuanceNeverHoldsUpNodeSignals(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	stream := fmt.Sprintf("databases:test:apart:%d", time.Now().UnixNano())
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	stuck := make(chan struct{})
	defer close(stuck)
	got := make(chan string, 1)
	issue := &Consumer{Redis: client, Stream: IssueStream(stream), Group: "test-controller", Name: "a", Logger: testdb.Logger(), Handle: func(context.Context, Signal) error { <-stuck; return nil }}
	nodes := &Consumer{Redis: client, Stream: stream, Group: "test-controller", Name: "a", Logger: testdb.Logger(), Handle: func(_ context.Context, s Signal) error { got <- s.Kind; return nil }}
	wg.Go(func() { issue.Run(ctx) })
	wg.Go(func() { nodes.Run(ctx) })
	p := &Publisher{Redis: client, Stream: stream}
	for _, kind := range []string{KindEnroll, "csr", KindSealKey, KindSilent} {
		if err := p.Publish(ctx, Signal{Kind: kind, Scope: "dbi_a"}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case kind := <-got:
		if kind != KindSilent {
			t.Errorf("%s reached the node stream", kind)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a silent node was held up")
	}
}

func TestStatusHash(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	ctx := context.Background()
	dbi := fmt.Sprintf("dbi_%d", time.Now().UnixNano())
	at := time.Now().UTC().Truncate(time.Millisecond)
	type status struct {
		State       string     `json:"state"`
		DownSince   *time.Time `json:"downSince"`
		SilentSince *time.Time `json:"silentSince"`
	}
	want := status{State: "running", DownSince: &at}
	key := "test:status:" + dbi
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(ctx, "HSET", key, "ins_a", string(body)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadStatus[status](ctx, client, key)
	if err != nil || got["ins_a"].State != "running" || !got["ins_a"].DownSince.Equal(at) || got["ins_a"].SilentSince != nil {
		t.Errorf("%+v, %v", got, err)
	}
}

// A signal that failed comes back however busy the stream is: a sweep goes by the clock.
func TestBusyStreamStillSweeps(t *testing.T) {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	var wg sync.WaitGroup
	defer wg.Wait()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	stream := fmt.Sprintf("databases:test:busy:%d", time.Now().UnixNano())
	retried := make(chan struct{})
	var once sync.Once
	first := true // the Consumer's one goroutine
	c := &Consumer{Redis: client, Stream: stream, Group: "test-controller", Name: "a", Logger: testdb.Logger(), SweepEvery: 100 * time.Millisecond, Handle: func(_ context.Context, s Signal) error {
		if s.Node != "ins_fails" {
			return nil
		}
		if first {
			first = false
			return errors.New("once")
		}
		once.Do(func() { close(retried) })
		return nil
	}}
	wg.Go(func() { c.Run(ctx) })
	p := &Publisher{Redis: client, Stream: stream}
	if err := p.Publish(ctx, Signal{Node: "ins_fails"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.After(5 * time.Second); ; {
		select {
		case <-retried:
			return
		case <-deadline:
			t.Fatal("never retried on a busy stream")
		case <-time.After(10 * time.Millisecond): // always under SweepEvery
			if err := p.Publish(ctx, Signal{Node: "ins_busy"}); err != nil {
				t.Fatal(err)
			}
		}
	}
}
