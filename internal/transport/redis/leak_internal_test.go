package redistransport

import (
	"context"
	"os"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/runkite/runkite/internal/transport"
)

// Internal test (same package, not redistransport_test) so it can inspect
// the unexported `tailers` map directly -- the only way to actually prove
// the claimed leak fix rather than trust the description. Runs many
// subscriptions that each complete normally (terminal event, no explicit
// Close call) and verifies the map is fully drained afterward.
func TestBrokerTailersMapDoesNotLeak(t *testing.T) {
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

	b := NewBroker(rdb)

	const n = 25
	for i := 0; i < n; i++ {
		runID := "leak-run"
		ch, err := b.Subscribe(ctx, runID+string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		rid := runID + string(rune('a'+i))
		// Publish a terminal event immediately -- no explicit Close() call,
		// exactly the normal-completion path (not the cancel-timeout path).
		if err := b.Publish(ctx, rid, &transport.RunEvent{
			EventID: "e", Seq: 1, Method: "end", Namespace: []string{}, Data: []byte(`{"status":"success"}`),
		}); err != nil {
			t.Fatal(err)
		}
		// Drain the channel so tailStream's loop actually sees the event
		// and takes its terminal-return path.
		for range ch {
		}
	}

	// Give the last goroutine's deferred removeTailer a moment to run --
	// draining the channel happens concurrently with the defer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		size := len(b.tailers)
		b.mu.Unlock()
		if size == 0 {
			t.Logf("tailers map correctly drained to 0 after %d subscriptions", n)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	b.mu.Lock()
	leaked := len(b.tailers)
	b.mu.Unlock()
	t.Fatalf("LEAK: tailers map still has %d entries after %d subscriptions all completed normally, want 0", leaked, n)
}
