package commands_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/commands"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/delivery"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/nodeplane/wire"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/redis"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

const instance = "ins_aaaaaaaaaaaa"

// command is a product's command frame, with a fencing number as databases' has.
type command struct {
	ID         string          `json:"id"`
	Generation int64           `json:"epoch"`
	Verb       string          `json:"verb"`
	Args       json.RawMessage `json:"args"`
}

func (command) Type() string { return "command" }

var codec = wire.NewCodec(command{})

func newCommand(r commands.Request, args json.RawMessage) wire.Message {
	return &command{ID: r.Key, Generation: r.Generation, Verb: r.Verb, Args: args}
}

// pod stands in for the databases-mgmt pod holding a node: it answers from the agent's rule, a
// repeated ID from its last results, without running it twice. There's no broker in sight: nothing
// here but Redis, so these hold from databases-api and with RabbitMQ down.
type pod struct {
	sender *commands.Sender
	dbi    string
	runs   chan *command
	answer map[string]*commands.Result
}

func newPod(t *testing.T) *pod {
	t.Helper()
	url := os.Getenv("REDIS_TEST_URL")
	if url == "" {
		t.Skip("REDIS_TEST_URL isn't set; run scripts/test.sh")
	}
	client, err := redis.New(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	p := &pod{sender: commands.New(client, "test", newCommand), dbi: fmt.Sprintf("dbi_%d", time.Now().UnixNano()), runs: make(chan *command, 8), answer: map[string]*commands.Result{}}
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	name := "pod-" + p.dbi
	bus := &delivery.Bus{Redis: client, Prefix: "test", Pod: name, Logger: testdb.Logger()}
	connected := make(chan struct{}, 1)
	go bus.Run(ctx, delivery.Handlers{OnConnect: func(context.Context) error { connected <- struct{}{}; return nil }, Frame: func(f delivery.NodeFrame) {
		frame, err := codec.Decode(f.Frame)
		if err != nil {
			t.Error(err)
			return
		}
		c := frame.Message.(*command)
		result := p.answer[c.ID]
		if result == nil {
			p.runs <- c
			result = &commands.Result{ID: c.ID, OK: true, Data: json.RawMessage(`{"position":"0/1"}`)}
			p.answer[c.ID] = result
		}
		body, err := json.Marshal(result)
		if err != nil {
			t.Error(err)
			return
		}
		if _, err := client.Publish(ctx, commands.ResultChannel("test", p.dbi, instance, c.ID), string(body)); err != nil {
			t.Error(err)
		}
	}})
	<-connected
	if err := (&delivery.Presence{Redis: client, Prefix: "test"}).Set(ctx, p.dbi, instance, name); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCommandFromTheAPI(t *testing.T) {
	p := newPod(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := p.sender.Send(ctx, commands.Request{Scope: p.dbi, Instance: instance, Generation: 3, Verb: "verb", Key: "k1"})
	if err != nil || !got.OK || string(got.Data) != `{"position":"0/1"}` {
		t.Fatalf("%+v, %v", got, err)
	}
	c := <-p.runs
	if c.ID != "k1" || c.Generation != 3 || c.Verb != "verb" || string(c.Args) != "{}" {
		t.Errorf("%+v", c)
	}
}

// The agent answers a repeated ID without running it, so a retry must keep its ID.
func TestCommandRetryKeepsItsID(t *testing.T) {
	p := newPod(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := commands.Request{Scope: p.dbi, Instance: instance, Verb: "verb", Key: "k1", Args: map[string]bool{"on": true}}
	for range 2 {
		if got, err := p.sender.Send(ctx, r); err != nil || !got.OK {
			t.Fatalf("%+v, %v", got, err)
		}
	}
	if got := len(p.runs); got != 1 {
		t.Errorf("run %d times", got)
	}
}

func TestCommandToDisconnectedNode(t *testing.T) {
	p := newPod(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := p.sender.Send(ctx, commands.Request{Scope: p.dbi, Instance: "ins_gone", Verb: "verb", Key: "k"}); !errors.Is(err, commands.ErrNotConnected) {
		t.Errorf("%v", err)
	}
	// Listed, on a pod that's gone.
	client := p.sender.Redis
	if err := (&delivery.Presence{Redis: client, Prefix: "test"}).Set(ctx, p.dbi, "ins_listed", "pod-gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.sender.Send(ctx, commands.Request{Scope: p.dbi, Instance: "ins_listed", Verb: "verb", Key: "k"}); !errors.Is(err, commands.ErrNotConnected) {
		t.Errorf("%v", err)
	}
}

// TestCommandsSurviveRabbitMQDown: nothing above dials a broker. This keeps it so.
func TestCommandsSurviveRabbitMQDown(t *testing.T) {
	if url := os.Getenv("RABBITMQ_TEST_URL"); url != "" {
		t.Setenv("RABBITMQ_TEST_URL", "amqp://127.0.0.1:1/") // a broker that isn't there
	}
	TestCommandFromTheAPI(t)
}
