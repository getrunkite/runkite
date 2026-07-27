package redistransport_test

import (
	"context"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/runkite/runkite/internal/transport"
	redistransport "github.com/runkite/runkite/internal/transport/redis"
)

// Verifies the package's own claim: "the production transport for multi-node
// deployments." Simulates two separate processes/nodes by creating two
// independent Broker instances against the same Redis, using separate
// *goredis.Client connections (as two real processes would). Publisher is
// brokerA, subscriber is brokerB, which never saw the Publish call directly.
func TestCrossNodeDelivery(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set")
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}

	// Two independent connections, simulating two separate node processes.
	clientA := goredis.NewClient(opt)
	clientB := goredis.NewClient(opt)
	defer clientA.Close()
	defer clientB.Close()

	ctx := context.Background()
	_ = redistransport.FlushAll(ctx, clientA)

	brokerA := redistransport.NewBroker(clientA) // "node A" -- the publisher (e.g. gRPC bridge)
	brokerB := redistransport.NewBroker(clientB) // "node B" -- the subscriber (e.g. HTTP/SSE server)

	runID := "cross-node-run-1"

	// Node B subscribes FIRST (as an SSE handler would, before the run starts).
	ch, err := brokerB.Subscribe(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}

	// Node A publishes, with no shared in-memory state with node B.
	event := &transport.RunEvent{EventID: "e1", Seq: 1, Method: "values", Namespace: []string{}, Data: []byte(`{}`)}
	if err := brokerA.Publish(ctx, runID, event); err != nil {
		t.Fatal(err)
	}

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel closed with no event delivered")
		}
		t.Logf("received event from cross-node publish: %+v", got)
	case <-time.After(3 * time.Second):
		t.Fatal("CROSS-NODE DELIVERY FAILED: node B's subscriber never received node A's published event within 3s")
	}
}
