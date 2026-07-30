package redistransport_test

import (
	"context"
	"testing"
	"time"

	redistransport "github.com/sharanharsoor/runkite/internal/transport/redis"
)

// TestDequeue_ContextCancelDetectedWithinBlockCap_NotFullTimeout proves the
// fix for a bug found by a live e2e flake: a genuine
// Redis race where a runner's dead gRPC connection left Dequeue blocked
// inside a single, long BRPOP call for up to the FULL GetJob timeout
// (~30s in production, matched here with a long timeout to make the old
// bug's window visible) -- during that long single block, a job pushed by
// a concurrent Enqueue at almost the same instant the context finally got
// cancelled could be atomically popped by Redis for delivery to this
// connection, then lost when the connection tore down mid-response
// (Redis had already removed it from the list; Dequeue's caller never
// received it). Capping each individual BRPOP call's blocking duration
// (dequeueBlockCap) and checking ctx.Err() at the top of each loop
// iteration means a cancelled context is noticed within one cap-sized
// tick instead of only whenever the single long BRPOP call happens to
// surface it -- this test proves that reaction-time bound directly,
// without needing to reproduce the exact concurrent-push race live (that
// was reproduced and confirmed separately against the real
// TestVG003b_ResumeSurvivesRunnerRestart e2e flake before this fix).
func TestDequeue_ContextCancelDetectedWithinBlockCap_NotFullTimeout(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	// A long timeout (10s) simulates GetJob's real ~30s long-poll -- the
	// bug was that Dequeue could stay blocked in one BRPOP call for
	// nearly this whole duration despite ctx being cancelled early.
	_, err := q.Dequeue(ctx, "test-runner-cancel", 10*time.Second)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a context-cancellation error, got nil (job=nil implied success?)")
	}
	// Generous vs. the 300ms cancel + ~2s block cap: must return well
	// before the full 10s timeout. 4s catches a regression back to
	// "block for the whole remaining timeout" while tolerating normal
	// scheduling jitter around the ~2s cap.
	if elapsed > 4*time.Second {
		t.Fatalf("Dequeue took %v to notice context cancellation (want well under the 10s timeout) -- "+
			"regression back to blocking for the full remaining duration on one BRPOP call", elapsed)
	}
	t.Logf("Dequeue noticed context cancellation after %v (10s timeout, 300ms cancel delay)", elapsed)
}
