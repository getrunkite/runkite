package inprocess_test

import (
	"context"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/transport"
	"github.com/sharanharsoor/runkite/internal/transport/inprocess"
)

func TestReclaimStale_ReenqueuesUnackedJob(t *testing.T) {
	q := inprocess.NewQueue()
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-stale",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}

	got, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || got == nil {
		t.Fatalf("dequeue: got=%v err=%v", got, err)
	}

	// Not Ack'd — reclaim with a zero maxAge should pick it up immediately.
	n, err := q.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}

	again, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || again == nil {
		t.Fatalf("expected reclaimed job, got=%v err=%v", again, err)
	}
	if again.RunID != "run-stale" {
		t.Fatalf("got run %s, want run-stale", again.RunID)
	}
	// Fencing: a reclaimed job's generation must be bumped so the
	// ORIGINAL runner's late Heartbeat/ReportStatus (if its blip was
	// transient and it finishes anyway) presents a stale value and
	// gets rejected instead of clobbering this new attempt.
	if again.Generation != 1 {
		t.Fatalf("generation after one reclaim = %d, want 1 (started at 0, ReclaimStale increments once)", again.Generation)
	}
}

func TestAck_PreventsReclaim(t *testing.T) {
	q := inprocess.NewQueue()
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-acked",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	_ = q.Enqueue(ctx, job)
	got, _ := q.Dequeue(ctx, "test-runner", time.Second)
	if got == nil {
		t.Fatal("expected job")
	}
	if accepted, err := q.Ack(ctx, got.RunID, 0); err != nil || !accepted {
		t.Fatalf("Ack: accepted=%v err=%v", accepted, err)
	}

	n, err := q.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d after Ack, want 0", n)
	}
}
