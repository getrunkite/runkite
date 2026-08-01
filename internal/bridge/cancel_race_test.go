package bridge

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

// TestGetJob_CancelImmediatelyAfterDispatch guards against a regression where
// GetJob subscribed to the run's cancel channel in a background goroutine
// AFTER already returning the job to the caller. CancelBroker is a transient
// pub/sub with no buffering/replay (true for both inprocess and redis
// implementations) -- a cancel published before the subscription registers
// is silently lost. This reproduced a ~80% loss rate before the fix (cancel
// subscription must happen synchronously inside GetJob, before it returns).
func TestGetJob_CancelImmediatelyAfterDispatch(t *testing.T) {
	iterations := 100
	var missed atomic.Int64

	for i := 0; i < iterations; i++ {
		queue := inprocess.NewQueue()
		cancelBus := inprocess.NewCancelBus()
		srv := NewServer(queue, inprocess.NewBroker(), cancelBus, nil)

		runID := "race-run"
		assignment := &transport.RunAssignment{RunID: runID, ThreadID: "t", RunnerKind: "k", GraphID: "g", Input: json.RawMessage(`{}`)}
		if err := queue.Enqueue(context.Background(), assignment); err != nil {
			t.Fatal(err)
		}

		// Runner is already watching, as it would be at startup via WatchCancels.
		watchCh := make(chan string, 1)
		srv.addWatcher("k", watchCh)

		// Sequenced (not concurrent): a client cannot cancel a run before
		// the runner has been dispatched it, i.e. before GetJob returns.
		// This is the tightest realistic race -- cancel fired the instant
		// after dispatch.
		resp, err := srv.GetJob(context.Background(), &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1})
		if err != nil || !resp.HasJob {
			t.Fatalf("GetJob failed: %v %v", resp, err)
		}
		_ = cancelBus.PublishCancel(context.Background(), runID)

		select {
		case got := <-watchCh:
			if got != runID {
				t.Errorf("wrong run id: %s", got)
			}
		case <-time.After(200 * time.Millisecond):
			missed.Add(1)
		}

		srv.removeWatcher("k", watchCh)
	}

	if missed.Load() > 0 {
		t.Errorf("cancel signal lost in %d/%d iterations -- GetJob must subscribe to the cancel channel synchronously before returning", missed.Load(), iterations)
	}
}
