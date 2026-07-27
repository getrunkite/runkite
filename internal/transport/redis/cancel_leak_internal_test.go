package redistransport

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// TestCancelBusContextCancelReleasesPubSub is the hard proof that
// SubscribeCancel releases its Redis subscription when ctx is cancelled
// without a real cancel message -- the normal-completion path that used
// to leak forever (see CancelBus.SubscribeCancel's doc comment and
// TR-024). TR-024 only asserts the return channel does not false-fire;
// this inspects Redis itself via PUBSUB NUMSUB.
func TestCancelBusContextCancelReleasesPubSub(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set")
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := goredis.NewClient(opt)
	defer rdb.Close()

	ctx := context.Background()
	_ = FlushAll(ctx, rdb)

	bus := NewCancelBus(rdb)
	const n = 20
	channels := make([]string, n)
	cancels := make([]context.CancelFunc, n)

	for i := 0; i < n; i++ {
		runID := fmt.Sprintf("cancel-leak-%d", i)
		channels[i] = cancelChannel(runID)
		subCtx, cancel := context.WithCancel(ctx)
		cancels[i] = cancel
		if _, err := bus.SubscribeCancel(subCtx, runID); err != nil {
			t.Fatalf("SubscribeCancel(%s): %v", runID, err)
		}
	}

	// All n subscriptions should be live.
	deadline := time.Now().Add(2 * time.Second)
	for {
		total := pubsubNumSubTotal(t, rdb, channels)
		if total >= int64(n) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected >= %d live pubsub subscriptions before cancel, got %d", n, total)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, cancel := range cancels {
		cancel()
	}

	deadline = time.Now().Add(3 * time.Second)
	for {
		total := pubsubNumSubTotal(t, rdb, channels)
		if total == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("LEAK: %d/%d Redis Pub/Sub subscriptions still live after ctx cancel (want 0)", total, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func pubsubNumSubTotal(t *testing.T, rdb *goredis.Client, channels []string) int64 {
	t.Helper()
	res, err := rdb.PubSubNumSub(context.Background(), channels...).Result()
	if err != nil {
		t.Fatalf("PUBSUB NUMSUB: %v", err)
	}
	var total int64
	for _, n := range res {
		total += n
	}
	return total
}
