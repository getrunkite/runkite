// Package conformance provides transport-agnostic test suites that every
// JobQueue, EventBroker, and CancelBroker implementation must pass.
//
// Usage in implementation tests:
//
//	func TestInMemoryQueue(t *testing.T) {
//	    conformance.RunJobQueueSuite(t, func() transport.JobQueue {
//	        return inprocess.NewQueue()
//	    })
//	}
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runkite/runkite/internal/transport"
)

// Factory functions that tests use to create fresh instances.
type JobQueueFactory func() transport.JobQueue
type EventBrokerFactory func() transport.EventBroker
type CancelBrokerFactory func() transport.CancelBroker

func makeAssignment(runID, runnerKind, graphID string) *transport.RunAssignment {
	return &transport.RunAssignment{
		RunID:          runID,
		ThreadID:       "thread-" + runID,
		RunnerKind:     runnerKind,
		GraphID:        graphID,
		Input:          json.RawMessage(`{"messages":[{"role":"user","content":"test"}]}`),
		StreamModes:    []string{"values"},
		ConnectorNeeds: []string{},
	}
}

func makeEvent(runID string, seq int64, method string, data interface{}) *transport.RunEvent {
	d, _ := json.Marshal(data)
	return &transport.RunEvent{
		EventID:   fmt.Sprintf("%s_evt_%d", runID, seq),
		Seq:       seq,
		Method:    method,
		Namespace: []string{},
		Data:      d,
		Ts:        time.Now().UnixMilli(),
	}
}

// --------------------------------------------------------------------------
// JobQueue conformance suite
// --------------------------------------------------------------------------

// RunJobQueueSuite runs all conformance tests for a JobQueue implementation.
func RunJobQueueSuite(t *testing.T, factory JobQueueFactory) {
	t.Run("TR-001_single_enqueue_dequeue", func(t *testing.T) {
		q := factory()
		ctx := context.Background()
		job := makeAssignment("run-001", "test-runner", "echo")

		if err := q.Enqueue(ctx, job); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}

		got, err := q.Dequeue(ctx, "test-runner", 5*time.Second)
		if err != nil {
			t.Fatalf("Dequeue failed: %v", err)
		}
		if got == nil {
			t.Fatal("Dequeue returned nil")
		}
		if got.RunID != "run-001" {
			t.Errorf("RunID = %q, want %q", got.RunID, "run-001")
		}
		if got.GraphID != "echo" {
			t.Errorf("GraphID = %q, want %q", got.GraphID, "echo")
		}
	})

	t.Run("TR-002_FIFO_ordering", func(t *testing.T) {
		q := factory()
		ctx := context.Background()

		for i := 0; i < 5; i++ {
			job := makeAssignment(fmt.Sprintf("run-%03d", i), "test-runner", "echo")
			if err := q.Enqueue(ctx, job); err != nil {
				t.Fatalf("Enqueue %d failed: %v", i, err)
			}
		}

		for i := 0; i < 5; i++ {
			got, err := q.Dequeue(ctx, "test-runner", 5*time.Second)
			if err != nil {
				t.Fatalf("Dequeue %d failed: %v", i, err)
			}
			expected := fmt.Sprintf("run-%03d", i)
			if got.RunID != expected {
				t.Errorf("Dequeue %d: RunID = %q, want %q", i, got.RunID, expected)
			}
		}
	})

	t.Run("TR-003_multiple_consumers_no_duplicates", func(t *testing.T) {
		q := factory()
		ctx := context.Background()
		n := 20

		for i := 0; i < n; i++ {
			job := makeAssignment(fmt.Sprintf("run-%03d", i), "test-runner", "echo")
			if err := q.Enqueue(ctx, job); err != nil {
				t.Fatalf("Enqueue %d failed: %v", i, err)
			}
		}

		var mu sync.Mutex
		seen := make(map[string]int)
		var wg sync.WaitGroup

		for c := 0; c < 4; c++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					got, err := q.Dequeue(ctx, "test-runner", 500*time.Millisecond)
					if err != nil || got == nil {
						return
					}
					mu.Lock()
					seen[got.RunID]++
					mu.Unlock()
				}
			}()
		}

		wg.Wait()

		if len(seen) != n {
			t.Errorf("Got %d unique jobs, want %d", len(seen), n)
		}
		for runID, count := range seen {
			if count != 1 {
				t.Errorf("Job %s delivered %d times, want 1", runID, count)
			}
		}
	})

	t.Run("TR-004_large_payload", func(t *testing.T) {
		q := factory()
		ctx := context.Background()

		largeInput := make([]byte, 1024*1024) // 1MB
		for i := range largeInput {
			largeInput[i] = byte('A' + (i % 26))
		}
		job := makeAssignment("run-large", "test-runner", "echo")
		job.Input = json.RawMessage(fmt.Sprintf(`{"data":"%s"}`, string(largeInput)))

		if err := q.Enqueue(ctx, job); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}

		got, err := q.Dequeue(ctx, "test-runner", 5*time.Second)
		if err != nil {
			t.Fatalf("Dequeue failed: %v", err)
		}
		if got == nil {
			t.Fatal("Dequeue returned nil for large payload")
		}
		if len(got.Input) != len(job.Input) {
			t.Errorf("Input length = %d, want %d", len(got.Input), len(job.Input))
		}
	})

	t.Run("TR-005_dequeue_timeout_empty_queue", func(t *testing.T) {
		q := factory()
		ctx := context.Background()

		start := time.Now()
		got, err := q.Dequeue(ctx, "test-runner", 200*time.Millisecond)
		elapsed := time.Since(start)

		if err != nil {
			t.Fatalf("Dequeue failed: %v", err)
		}
		if got != nil {
			t.Error("Dequeue should return nil on timeout")
		}
		if elapsed < 150*time.Millisecond {
			t.Error("Dequeue returned too quickly (should block until timeout)")
		}
	})

	// TR-006 maps to test_plan TR-021 (cancel before dequeue / poison).
	t.Run("TR-006_dequeue_after_cancel", func(t *testing.T) {
		q := factory()
		ctx := context.Background()

		job := makeAssignment("run-cancel-before", "test-runner", "echo")
		if err := q.Enqueue(ctx, job); err != nil {
			t.Fatalf("Enqueue failed: %v", err)
		}

		if err := q.Cancel(ctx, "run-cancel-before"); err != nil {
			t.Fatalf("Cancel failed: %v", err)
		}

		got, err := q.Dequeue(ctx, "test-runner", 500*time.Millisecond)
		if err != nil {
			t.Fatalf("Dequeue failed: %v", err)
		}
		if got != nil {
			t.Error("Canceled job should not be delivered")
		}
	})

	// Canceled job must be skipped without starving the next job in the queue.
	t.Run("TR-006b_cancel_skips_to_next_job", func(t *testing.T) {
		q := factory()
		ctx := context.Background()

		_ = q.Enqueue(ctx, makeAssignment("run-skip-a", "test-runner", "echo"))
		_ = q.Enqueue(ctx, makeAssignment("run-skip-b", "test-runner", "echo"))
		if err := q.Cancel(ctx, "run-skip-a"); err != nil {
			t.Fatalf("Cancel failed: %v", err)
		}

		got, err := q.Dequeue(ctx, "test-runner", time.Second)
		if err != nil {
			t.Fatalf("Dequeue failed: %v", err)
		}
		if got == nil {
			t.Fatal("expected run-skip-b after skipping canceled run-skip-a")
		}
		if got.RunID != "run-skip-b" {
			t.Errorf("RunID = %q, want run-skip-b", got.RunID)
		}
	})

	t.Run("TR-007_queue_length", func(t *testing.T) {
		q := factory()
		ctx := context.Background()

		for i := 0; i < 3; i++ {
			job := makeAssignment(fmt.Sprintf("run-len-%d", i), "test-runner", "echo")
			_ = q.Enqueue(ctx, job)
		}

		n, err := q.Len(ctx)
		if err != nil {
			t.Fatalf("Len failed: %v", err)
		}
		if n != 3 {
			t.Errorf("Len = %d, want 3", n)
		}

		_, _ = q.Dequeue(ctx, "test-runner", time.Second)

		n, err = q.Len(ctx)
		if err != nil {
			t.Fatalf("Len failed: %v", err)
		}
		if n != 2 {
			t.Errorf("Len = %d after dequeue, want 2", n)
		}
	})

	t.Run("TR-008_runner_kind_isolation", func(t *testing.T) {
		q := factory()
		ctx := context.Background()

		jobA := makeAssignment("run-a", "runner-alpha", "echo")
		jobB := makeAssignment("run-b", "runner-beta", "echo")

		_ = q.Enqueue(ctx, jobA)
		_ = q.Enqueue(ctx, jobB)

		gotA, _ := q.Dequeue(ctx, "runner-alpha", time.Second)
		gotB, _ := q.Dequeue(ctx, "runner-beta", time.Second)

		if gotA == nil || gotA.RunID != "run-a" {
			t.Errorf("runner-alpha got %v, want run-a", gotA)
		}
		if gotB == nil || gotB.RunID != "run-b" {
			t.Errorf("runner-beta got %v, want run-b", gotB)
		}

		// runner-alpha should not see runner-beta's jobs
		gotExtra, _ := q.Dequeue(ctx, "runner-alpha", 200*time.Millisecond)
		if gotExtra != nil {
			t.Error("runner-alpha should not see runner-beta jobs")
		}
	})
}

// --------------------------------------------------------------------------
// EventBroker conformance suite
// --------------------------------------------------------------------------

// RunEventBrokerSuite runs all conformance tests for an EventBroker implementation.
func RunEventBrokerSuite(t *testing.T, factory EventBrokerFactory) {
	t.Run("TR-010_publish_subscribe", func(t *testing.T) {
		b := factory()
		ctx := context.Background()
		runID := "run-evt-001"

		ch, err := b.Subscribe(ctx, runID)
		if err != nil {
			t.Fatalf("Subscribe failed: %v", err)
		}

		event := makeEvent(runID, 1, "values", map[string]string{"key": "val"})
		if err := b.Publish(ctx, runID, event); err != nil {
			t.Fatalf("Publish failed: %v", err)
		}

		endEvent := makeEvent(runID, 2, "end", map[string]string{"status": "success"})
		if err := b.Publish(ctx, runID, endEvent); err != nil {
			t.Fatalf("Publish end failed: %v", err)
		}

		var received []*transport.RunEvent
		for evt := range ch {
			received = append(received, evt)
		}

		if len(received) != 2 {
			t.Fatalf("Received %d events, want 2", len(received))
		}
		if received[0].Seq != 1 {
			t.Errorf("First event seq = %d, want 1", received[0].Seq)
		}
		if received[1].Method != "end" {
			t.Errorf("Second event method = %q, want 'end'", received[1].Method)
		}
	})

	t.Run("TR-011_fan_out_multiple_subscribers", func(t *testing.T) {
		b := factory()
		ctx := context.Background()
		runID := "run-fanout"

		ch1, _ := b.Subscribe(ctx, runID)
		ch2, _ := b.Subscribe(ctx, runID)

		event := makeEvent(runID, 1, "values", map[string]string{"k": "v"})
		_ = b.Publish(ctx, runID, event)

		endEvent := makeEvent(runID, 2, "end", map[string]string{"status": "success"})
		_ = b.Publish(ctx, runID, endEvent)

		var count1, count2 int
		for range ch1 {
			count1++
		}
		for range ch2 {
			count2++
		}

		if count1 != 2 {
			t.Errorf("Subscriber 1 got %d events, want 2", count1)
		}
		if count2 != 2 {
			t.Errorf("Subscriber 2 got %d events, want 2", count2)
		}
	})

	t.Run("TR-012_event_ordering", func(t *testing.T) {
		b := factory()
		ctx := context.Background()
		runID := "run-order"

		ch, _ := b.Subscribe(ctx, runID)

		for i := int64(1); i <= 10; i++ {
			event := makeEvent(runID, i, "values", map[string]int64{"seq": i})
			_ = b.Publish(ctx, runID, event)
		}
		endEvent := makeEvent(runID, 11, "end", map[string]string{"status": "success"})
		_ = b.Publish(ctx, runID, endEvent)

		var lastSeq int64
		for evt := range ch {
			if evt.Seq <= lastSeq {
				t.Errorf("Out of order: seq %d after %d", evt.Seq, lastSeq)
			}
			lastSeq = evt.Seq
		}
		if lastSeq != 11 {
			t.Errorf("Last seq = %d, want 11", lastSeq)
		}
	})

	t.Run("TR-013_no_cross_run_leakage", func(t *testing.T) {
		b := factory()
		ctx := context.Background()

		chA, _ := b.Subscribe(ctx, "run-A")
		chB, _ := b.Subscribe(ctx, "run-B")

		_ = b.Publish(ctx, "run-A", makeEvent("run-A", 1, "values", map[string]string{"run": "A"}))
		_ = b.Publish(ctx, "run-A", makeEvent("run-A", 2, "end", map[string]string{"status": "success"}))
		_ = b.Publish(ctx, "run-B", makeEvent("run-B", 1, "values", map[string]string{"run": "B"}))
		_ = b.Publish(ctx, "run-B", makeEvent("run-B", 2, "end", map[string]string{"status": "success"}))

		for evt := range chA {
			if evt.EventID != "" && evt.EventID[:5] != "run-A" {
				t.Error("Run A subscriber received run B event")
			}
		}
		for evt := range chB {
			if evt.EventID != "" && evt.EventID[:5] != "run-B" {
				t.Error("Run B subscriber received run A event")
			}
		}
	})

	t.Run("TR-014_replay_from_seq", func(t *testing.T) {
		b := factory()
		ctx := context.Background()
		runID := "run-replay"

		for i := int64(1); i <= 5; i++ {
			_ = b.Publish(ctx, runID, makeEvent(runID, i, "values", map[string]int64{"i": i}))
		}
		_ = b.Publish(ctx, runID, makeEvent(runID, 6, "end", map[string]string{"status": "success"}))

		// Replay from seq 3 -- should get events 4, 5, 6
		events, err := b.Replay(ctx, runID, 3)
		if err != nil {
			t.Fatalf("Replay failed: %v", err)
		}
		if len(events) != 3 {
			t.Fatalf("Replay returned %d events, want 3", len(events))
		}
		if events[0].Seq != 4 {
			t.Errorf("First replay event seq = %d, want 4", events[0].Seq)
		}
	})

	t.Run("TR-015_replay_all", func(t *testing.T) {
		b := factory()
		ctx := context.Background()
		runID := "run-replay-all"

		for i := int64(1); i <= 3; i++ {
			_ = b.Publish(ctx, runID, makeEvent(runID, i, "values", nil))
		}
		_ = b.Publish(ctx, runID, makeEvent(runID, 4, "end", map[string]string{"status": "success"}))

		events, err := b.Replay(ctx, runID, 0)
		if err != nil {
			t.Fatalf("Replay failed: %v", err)
		}
		if len(events) != 4 {
			t.Errorf("Replay all returned %d events, want 4", len(events))
		}
	})

	t.Run("TR-016_burst_events", func(t *testing.T) {
		b := factory()
		ctx := context.Background()
		runID := "run-burst"

		ch, _ := b.Subscribe(ctx, runID)

		n := int64(100)
		for i := int64(1); i <= n; i++ {
			_ = b.Publish(ctx, runID, makeEvent(runID, i, "values", nil))
		}
		_ = b.Publish(ctx, runID, makeEvent(runID, n+1, "end", map[string]string{"status": "success"}))

		var count int64
		for range ch {
			count++
		}
		if count != n+1 {
			t.Errorf("Received %d events, want %d", count, n+1)
		}
	})
}

// --------------------------------------------------------------------------
// CancelBroker conformance suite
// --------------------------------------------------------------------------

// RunCancelBrokerSuite runs all conformance tests for a CancelBroker implementation.
func RunCancelBrokerSuite(t *testing.T, factory CancelBrokerFactory) {
	t.Run("TR-020_cancel_signal_received", func(t *testing.T) {
		c := factory()
		ctx := context.Background()
		runID := "run-cancel-001"

		ch, err := c.SubscribeCancel(ctx, runID)
		if err != nil {
			t.Fatalf("SubscribeCancel failed: %v", err)
		}

		if err := c.PublishCancel(ctx, runID); err != nil {
			t.Fatalf("PublishCancel failed: %v", err)
		}

		select {
		case <-ch:
			// ok
		case <-time.After(2 * time.Second):
			t.Fatal("Cancel signal not received within timeout")
		}
	})

	t.Run("TR-022_cancel_completed_run_no_error", func(t *testing.T) {
		c := factory()
		ctx := context.Background()

		// Cancel a run that no one is subscribed to -- should not error
		if err := c.PublishCancel(ctx, "run-nonexistent"); err != nil {
			t.Errorf("PublishCancel on nonexistent run should not error: %v", err)
		}
	})

	t.Run("TR-023_multiple_subscribers_all_receive_cancel", func(t *testing.T) {
		c := factory()
		ctx := context.Background()
		runID := "run-cancel-multi"

		ch1, _ := c.SubscribeCancel(ctx, runID)
		ch2, _ := c.SubscribeCancel(ctx, runID)

		_ = c.PublishCancel(ctx, runID)

		var received atomic.Int32

		go func() {
			select {
			case <-ch1:
				received.Add(1)
			case <-time.After(2 * time.Second):
			}
		}()
		go func() {
			select {
			case <-ch2:
				received.Add(1)
			case <-time.After(2 * time.Second):
			}
		}()

		time.Sleep(500 * time.Millisecond)
		if received.Load() != 2 {
			t.Errorf("Only %d/2 subscribers received cancel", received.Load())
		}
	})

	// TR-024/025 are the regression for a real leak found via pprof under
	// load: every caller passed context.Background() into SubscribeCancel
	// (internal/bridge/server.go's GetJob), which never cancels -- so a
	// run that completes normally (never cancelled) leaked its
	// subscription forever. For the Redis-backed implementation, each
	// leaked subscription held a live Pub/Sub connection plus 2
	// background goroutines; confirmed 3646 leaked subscriptions after
	// ~1800 completed runs against a live control plane under
	// concurrency=100 load, the actual root cause of bench/REPORT.md's
	// "Redis transport gets dramatically slower under concurrency"
	// finding. The fix: callers now pass a run-scoped context (cancelled
	// on run completion), and both CancelBroker implementations release
	// their subscription when that context is done.

	t.Run("TR-024_context_cancellation_releases_subscription_without_false_cancel", func(t *testing.T) {
		c := factory()
		ctx, cancel := context.WithCancel(context.Background())
		runID := "run-ctx-cancel-001"

		ch, err := c.SubscribeCancel(ctx, runID)
		if err != nil {
			t.Fatalf("SubscribeCancel failed: %v", err)
		}

		cancel() // simulates the run completing normally, never cancelled

		// The channel must NOT fire just because ctx was cancelled --
		// a caller selecting on both ch and the same ctx needs to be
		// able to tell "real cancel" apart from "I stopped waiting"
		// (see CancelBroker's doc comment). If ctx cancellation alone
		// closed/fired ch, a caller's select could randomly pick that
		// branch and mistake a normal completion for a genuine cancel.
		select {
		case _, ok := <-ch:
			t.Fatalf("ctx cancellation alone must not deliver/close the channel (received ok=%v) -- would be indistinguishable from a real cancel to a caller selecting on both", ok)
		case <-time.After(200 * time.Millisecond):
			// correct: nothing delivered
		}
	})

	t.Run("TR-025_context_cancellation_then_real_cancel_is_still_safe", func(t *testing.T) {
		// A real cancel racing a ctx cancellation (run finishing right
		// as a cancel request lands) must not panic or double-close --
		// exercises both implementations' cleanup path concurrently
		// with PublishCancel's own delivery path.
		c := factory()
		ctx, cancel := context.WithCancel(context.Background())
		runID := "run-ctx-cancel-race"

		if _, err := c.SubscribeCancel(ctx, runID); err != nil {
			t.Fatalf("SubscribeCancel failed: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); _ = c.PublishCancel(context.Background(), runID) }()
		wg.Wait()

		// Give both cleanup paths a moment to run; the real assertion
		// is simply that this doesn't panic (double-close) and a fresh
		// subscribe for a different run_id still works afterward.
		time.Sleep(100 * time.Millisecond)
		freshCh, err := c.SubscribeCancel(context.Background(), "run-after-race")
		if err != nil {
			t.Fatalf("SubscribeCancel after race failed: %v", err)
		}
		_ = c.PublishCancel(context.Background(), "run-after-race")
		select {
		case <-freshCh:
		case <-time.After(2 * time.Second):
			t.Fatal("broker unusable after a subscribe/cancel race on a different run_id")
		}
	})
}
