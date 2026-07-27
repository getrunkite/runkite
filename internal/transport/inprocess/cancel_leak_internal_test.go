package inprocess

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestCancelBusContextCancelDrainsSubsMap proves the in-process
// CancelBus releases map entries on ctx cancel (normal completion),
// not only on PublishCancel. Same bug class as the Redis Pub/Sub leak.
func TestCancelBusContextCancelDrainsSubsMap(t *testing.T) {
	bus := NewCancelBus()
	const n = 50
	cancels := make([]context.CancelFunc, n)

	for i := 0; i < n; i++ {
		subCtx, cancel := context.WithCancel(context.Background())
		cancels[i] = cancel
		if _, err := bus.SubscribeCancel(subCtx, fmt.Sprintf("run-%d", i)); err != nil {
			t.Fatalf("SubscribeCancel: %v", err)
		}
	}

	bus.mu.Lock()
	before := len(bus.subs)
	bus.mu.Unlock()
	if before != n {
		t.Fatalf("expected %d subs map entries before cancel, got %d", n, before)
	}

	for _, cancel := range cancels {
		cancel()
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		bus.mu.Lock()
		size := len(bus.subs)
		bus.mu.Unlock()
		if size == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}

	bus.mu.Lock()
	leaked := len(bus.subs)
	bus.mu.Unlock()
	t.Fatalf("LEAK: CancelBus.subs still has %d entries after ctx cancel, want 0", leaked)
}
