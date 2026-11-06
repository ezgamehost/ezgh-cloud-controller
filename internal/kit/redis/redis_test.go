package redis_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
)

func client(t *testing.T) *redis.Client {
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("REDIS_TEST_URL isn't set")
		}
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	c, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// Concurrent commands each get their own connection's answer, and scripts run by SHA after the
// first EVAL.
func TestPoolAndScripts(t *testing.T) {
	c := client(t)
	ctx := context.Background()
	key := "kit-test:" + t.Name()
	if _, err := c.Do(ctx, "DEL", key); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Do(ctx, "INCR", key); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n, err := c.Do(ctx, "GET", key); err != nil || n != "50" {
		t.Fatalf("GET = %v, %v", n, err)
	}
	script := redis.NewScript(`return redis.call('INCRBY', KEYS[1], ARGV[1])`)
	for want := int64(52); want <= 54; want += 2 {
		if n, err := c.Eval(ctx, script, []string{key}, "2"); err != nil || n != want {
			t.Fatalf("Eval = %v, %v; want %d", n, err, want)
		}
	}
	_, err := c.Do(ctx, "NOTACOMMAND")
	var reply redis.ReplyError
	if !errors.As(err, &reply) {
		t.Fatalf("an error reply: %v", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal("the connection survives an error reply:", err)
	}
	_, _ = c.Do(ctx, "DEL", key)
}

func TestNewErrorHidesPassword(t *testing.T) {
	for _, raw := range []string{"redis://:s3cr%t@host:6379", "http://:s3cret@host"} {
		_, err := redis.New(raw)
		if err == nil {
			t.Fatalf("New(%q) succeeded", raw)
		}
		if strings.Contains(err.Error(), "s3cr") {
			t.Fatalf("error leaks the password: %v", err)
		}
	}
}
