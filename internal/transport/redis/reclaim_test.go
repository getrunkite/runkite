package redistransport_test

import (
	"context"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/transport"
	redistransport "github.com/sharanharsoor/runkite/internal/transport/redis"
)

// These mirror internal/transport/inprocess/reclaim_test.go so both queue
// backends have equivalent coverage for the crash-recovery mechanism
// (in-flight tracking + Ack/Nack + ReclaimStale) -- this is what prevents a
// crashed runner's zombie GetJob long-poll from permanently losing a job it
// stole from the queue but can never deliver.

func TestRedisReclaimStale_ReenqueuesUnackedJob(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
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

	// Not Ack'd -- reclaim with a zero maxAge should pick it up immediately.
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
}

func TestRedisAck_PreventsReclaim(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
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
	if err := q.Ack(ctx, got.RunID); err != nil {
		t.Fatal(err)
	}

	n, err := q.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d after Ack, want 0", n)
	}
}

func TestRedisNack_ReenqueuesImmediately(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-nacked",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	_ = q.Enqueue(ctx, job)
	got, _ := q.Dequeue(ctx, "test-runner", time.Second)
	if got == nil {
		t.Fatal("expected job")
	}

	if err := q.Nack(ctx, got.RunID); err != nil {
		t.Fatal(err)
	}

	// Nack should make the job available again without waiting for reclaim.
	again, err := q.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || again == nil {
		t.Fatalf("expected job back after Nack, got=%v err=%v", again, err)
	}
	if again.RunID != "run-nacked" {
		t.Fatalf("got run %s, want run-nacked", again.RunID)
	}
}

// TestRedisReclaimStale_SkipsCanceledJob guards a correctness property that
// falls out of ReclaimStale reusing Enqueue's own cancel check: a job
// canceled while it's in flight (runner has it, hasn't Ack'd yet) must not
// come back to life via reclaim after the cancellation.
func TestRedisReclaimStale_SkipsCanceledJob(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-cancel-inflight",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	_ = q.Enqueue(ctx, job)
	got, _ := q.Dequeue(ctx, "test-runner", time.Second)
	if got == nil {
		t.Fatal("expected job")
	}

	// Run gets canceled while in flight (e.g. client canceled, runner died).
	if err := q.Cancel(ctx, got.RunID); err != nil {
		t.Fatal(err)
	}

	n, err := q.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d for a canceled in-flight job, want 0 (canceled jobs must not resurrect)", n)
	}

	// Confirm it's truly gone, not just delayed.
	again, err := q.Dequeue(ctx, "test-runner", 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("expected no job after canceling an in-flight run, got %v", again)
	}
}

// TestRedisReclaimStale_RespectsMaxAge confirms jobs younger than maxAge
// are left alone -- ReclaimStale must not be a "reclaim everything" hammer.
func TestRedisReclaimStale_RespectsMaxAge(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	q := redistransport.NewQueue(rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID:      "run-fresh",
		ThreadID:   "t1",
		GraphID:    "echo",
		RunnerKind: "test-runner",
	}
	_ = q.Enqueue(ctx, job)
	got, _ := q.Dequeue(ctx, "test-runner", time.Second)
	if got == nil {
		t.Fatal("expected job")
	}

	// Job was just dequeued -- a generous maxAge must not reclaim it yet.
	n, err := q.ReclaimStale(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d fresh in-flight jobs, want 0", n)
	}
}
