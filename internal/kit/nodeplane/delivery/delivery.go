// Package delivery takes a change from its commit to a node's stream, lifted from databases-stack
// (9ff61c1, itself lb-stack's at b63684f; network-stack's managed-l4-lb-design.md, "How a change
// reaches a node"): the product's outbox relay publishes a Pointer to one of its shard queues, the
// pod consuming that shard (Dispatcher) hands the latest generation to the pods holding the scope's
// nodes (Bus, Presence), and the pointer settles when they've all acked. A scope is whatever a
// product's nodes belong to (a database, a cluster); a node is its instance. Every Redis key and
// channel starts with the product's Prefix, so two products can share nothing by accident.
package delivery

import (
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/rabbitmq"
)

// Pointer is what goes through a shard queue: the config itself never does.
type Pointer struct {
	Scope      string `json:"scope"`
	Generation int64  `json:"generation"`
}

// Shard is the queue a scope's pointers go to: FNV-1a(scope) mod n.
func Shard(scope string, n int) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(scope))
	return int(h.Sum32() % uint32(n))
}

// RoutingKey is the routing key of scope's shard: change.<shard>.
func RoutingKey(scope string, n int) string { return "change." + strconv.Itoa(Shard(scope, n)) }

// ShardQueues are a product's n shard queues, <prefix>.changes.<0…n-1>, all consumed by every mgmt
// pod: one at a time is active on each.
func ShardQueues(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = prefix + ".changes." + strconv.Itoa(i)
	}
	return out
}

// BrokerURL is a process's RabbitMQ URL on vhost: RABBITMQ_URL is the server alone, and its user
// comes from the operator's Secret, as RABBITMQ_USERNAME and RABBITMQ_PASSWORD.
func BrokerURL(e *config.Env, vhost string) (string, error) {
	base := e.Required("RABBITMQ_URL", "amqp://127.0.0.1:5672")
	user := e.Required("RABBITMQ_USERNAME", "guest")
	password := e.Required("RABBITMQ_PASSWORD", "guest")
	if err := e.Err(); err != nil {
		return "", err
	}
	url, err := rabbitmq.URL(base, user, password, vhost)
	if err != nil {
		return "", fmt.Errorf("RABBITMQ_URL: %w", err)
	}
	return url, nil
}
