package redistransport_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	redistransport "github.com/getrunkite/runkite/internal/transport/redis"
)

// TestDequeue_PendingOrphanRecoveredByReclaim is the F-08 regression:
// a crash between BLMOVE (job left the ready queue) and promote
// (durable inflight HSET/ZADD) used to lose the job forever. Now the
// job sits on rk:inflight:pending; ReclaimStale's drain promotes it
// into normal inflight so Ack/reclaim work.
func TestDequeue_PendingOrphanRecoveredByReclaim(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	ctx := context.Background()

	job := &transport.RunAssignment{
		RunID: "run-pending-orphan", ThreadID: "t1", GraphID: "echo",
		RunnerKind: "test-runner", Generation: 1,
	}
	payload, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the crash window: job is on pending only, nowhere else.
	if err := rdb.LPush(ctx, "rk:inflight:pending", payload).Err(); err != nil {
		t.Fatal(err)
	}

	q := redistransport.NewQueue(rdb)
	n, err := q.ReclaimStale(ctx, time.Hour) // maxAge irrelevant for drain; no zset entries yet
	if err != nil {
		t.Fatalf("ReclaimStale: %v", err)
	}
	if n != 0 {
		// drain promotes into inflight but does not requeue (job isn't stale yet)
		t.Fatalf("reclaimed=%d, want 0 (orphan was promoted, not requeued)", n)
	}

	pendingLen, err := rdb.LLen(ctx, "rk:inflight:pending").Result()
	if err != nil {
		t.Fatal(err)
	}
	if pendingLen != 0 {
		t.Fatalf("pending still has %d items after drain", pendingLen)
	}

	// Durable inflight must now hold the job.
	got, err := rdb.HGet(ctx, "rk:inflight:data", job.RunID).Result()
	if err != nil {
		t.Fatalf("inflight data missing after drain: %v", err)
	}
	if got != string(payload) {
		t.Fatalf("inflight payload mismatch")
	}

	acked, err := q.Ack(ctx, job.RunID, job.Generation)
	if err != nil || !acked {
		t.Fatalf("Ack after drain: acked=%v err=%v", acked, err)
	}
}

// TestDequeue_BLMoveThenPromote_HappyPath exercises the full new path
// end to end (not just seeded pending): Enqueue → Dequeue must leave
// pending empty and inflight recorded, then Ack clears it.
func TestDequeue_BLMoveThenPromote_HappyPath(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	ctx := context.Background()

	q := redistransport.NewQueue(rdb)
	job := &transport.RunAssignment{
		RunID: "run-blmove-happy", ThreadID: "t1", GraphID: "echo",
		RunnerKind: "test-runner", Generation: 1,
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, err := q.Dequeue(ctx, "test-runner", 3*time.Second)
	if err != nil || got == nil {
		t.Fatalf("Dequeue: got=%v err=%v", got, err)
	}
	if got.RunID != job.RunID {
		t.Fatalf("RunID = %q", got.RunID)
	}

	pendingLen, _ := rdb.LLen(ctx, "rk:inflight:pending").Result()
	if pendingLen != 0 {
		t.Fatalf("pending should be empty after successful promote, got %d", pendingLen)
	}
	if ok, _ := rdb.HExists(ctx, "rk:inflight:data", job.RunID).Result(); !ok {
		t.Fatal("expected job in inflight:data after Dequeue")
	}

	acked, err := q.Ack(ctx, job.RunID, job.Generation)
	if err != nil || !acked {
		t.Fatalf("Ack: acked=%v err=%v", acked, err)
	}
}

// TestDequeue_CanceledJobDiscardedFromPending proves a canceled run_id
// that lands on pending (via BLMOVE) is dropped, not promoted.
func TestDequeue_CanceledJobDiscardedFromPending(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	ctx := context.Background()

	q := redistransport.NewQueue(rdb)
	job := &transport.RunAssignment{
		RunID: "run-canceled-pending", ThreadID: "t1", GraphID: "echo",
		RunnerKind: "test-runner", Generation: 1,
	}
	if err := q.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := q.Cancel(ctx, job.RunID); err != nil {
		t.Fatal(err)
	}

	got, err := q.Dequeue(ctx, "test-runner", 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil (canceled discard), got %+v", got)
	}
	if ok, _ := rdb.HExists(ctx, "rk:inflight:data", job.RunID).Result(); ok {
		t.Fatal("canceled job must not be promoted into inflight")
	}
	pendingLen, _ := rdb.LLen(ctx, "rk:inflight:pending").Result()
	if pendingLen != 0 {
		t.Fatalf("canceled job must not remain on pending, len=%d", pendingLen)
	}
}
