package redistransport_test

import (
	"context"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	redistransport "github.com/runkite/runkite/internal/transport/redis"
)

// Verifies CancelBus's cross-node claim the same way TestCrossNodeDelivery
// verifies the Broker's: two independent CancelBus instances (separate
// *goredis.Client connections, simulating two node processes) against the
// same Redis. Node B subscribes for a cancel signal; node A publishes it.
// The previous CancelBus implementation only ever registered subscribers in
// a local Go map and never called rdb.Subscribe() at all, so this would have
// hung forever before the fix.
func TestCrossNodeCancelDelivery(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set")
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}

	clientA := goredis.NewClient(opt)
	clientB := goredis.NewClient(opt)
	defer clientA.Close()
	defer clientB.Close()

	ctx := context.Background()
	_ = redistransport.FlushAll(ctx, clientA)

	busA := redistransport.NewCancelBus(clientA) // "node A" -- e.g. instance that received the cancel HTTP request
	busB := redistransport.NewCancelBus(clientB) // "node B" -- e.g. instance actually running the job

	runID := "cross-node-cancel-1"

	ch, err := busB.SubscribeCancel(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}

	if err := busA.PublishCancel(ctx, runID); err != nil {
		t.Fatal(err)
	}

	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("channel closed without a cancel signal")
		}
		t.Log("node B received node A's cancel signal")
	case <-time.After(3 * time.Second):
		t.Fatal("CROSS-NODE CANCEL FAILED: node B never received node A's PublishCancel within 3s")
	}
}
