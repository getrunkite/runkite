package redistransport_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/transport"
	redistransport "github.com/sharanharsoor/runkite/internal/transport/redis"
)

// This file exercises exactly the two failure modes found via live
// multi-instance testing: in-flight
// tracking used to be a plain Go map per Queue instance, so it only worked
// correctly when Dequeue/Ack/ReclaimStale all happened to be called on the
// SAME instance. Every test here deliberately uses TWO SEPARATE Queue
// instances against the same Redis, simulating two control-plane replicas,
// to prove the shared state actually is shared.

func TestSharedInflight_AckFromDifferentInstanceThanDequeue(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	ctx := context.Background()

	// Two independent Queue instances, same Redis -- the exact shape of
	// two runkite serve replicas behind a load-balanced gRPC bridge with
	// no session affinity: GetJob can land on one, the completing call on
	// the other.
	instanceA := redistransport.NewQueue(rdb)
	instanceB := redistransport.NewQueue(rdb)

	job := &transport.RunAssignment{RunID: "run-cross-ack", ThreadID: "t1", GraphID: "echo", RunnerKind: "test-runner"}
	if err := instanceA.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}

	got, err := instanceA.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || got == nil {
		t.Fatalf("instanceA dequeue: got=%v err=%v", got, err)
	}

	// The Ack for this job arrives via instanceB, not the instance that
	// dequeued it -- must still work, not silently no-op.
	if accepted, err := instanceB.Ack(ctx, got.RunID, 0); err != nil || !accepted {
		t.Fatalf("instanceB.Ack: accepted=%v err=%v", accepted, err)
	}

	// If the Ack landed correctly in shared state, reclaiming (from
	// EITHER instance) must find nothing to reclaim -- the old bug would
	// have instanceA's local map still holding the entry, since its own
	// Ack was never called.
	n, err := instanceA.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed %d after cross-instance Ack, want 0 (this is exactly the duplicate-execution bug)", n)
	}
}

func TestSharedInflight_ReclaimFromDifferentInstanceThanDequeue(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	ctx := context.Background()

	instanceA := redistransport.NewQueue(rdb)
	instanceB := redistransport.NewQueue(rdb)

	job := &transport.RunAssignment{RunID: "run-cross-reclaim", ThreadID: "t1", GraphID: "echo", RunnerKind: "test-runner"}
	if err := instanceA.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}

	got, err := instanceA.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || got == nil {
		t.Fatalf("instanceA dequeue: got=%v err=%v", got, err)
	}

	// Simulate instanceA crashing (its process, and thus any local state
	// it might have held, is simply gone -- we never call anything on it
	// again). instanceB, a surviving replica, must still be able to
	// reclaim the job instanceA dequeued and never Ack'd.
	n, err := instanceB.ReclaimStale(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("instanceB reclaimed %d jobs dequeued by (now-crashed) instanceA, want 1 (this is exactly the silent-loss bug)", n)
	}

	// The reclaimed job must actually be redeliverable.
	again, err := instanceB.Dequeue(ctx, "test-runner", time.Second)
	if err != nil || again == nil {
		t.Fatalf("expected reclaimed job back, got=%v err=%v", again, err)
	}
	if again.RunID != "run-cross-reclaim" {
		t.Fatalf("got run %s, want run-cross-reclaim", again.RunID)
	}
}

func TestSharedInflight_ConcurrentReclaimAcrossInstancesReclaimsExactlyOnce(t *testing.T) {
	rdb := getRedisClient(t)
	defer rdb.Close()
	redistransport.FlushAll(context.Background(), rdb)
	ctx := context.Background()

	dequeuer := redistransport.NewQueue(rdb)
	job := &transport.RunAssignment{RunID: "run-concurrent-reclaim", ThreadID: "t1", GraphID: "echo", RunnerKind: "test-runner"}
	if err := dequeuer.Enqueue(ctx, job); err != nil {
		t.Fatal(err)
	}
	if got, err := dequeuer.Dequeue(ctx, "test-runner", time.Second); err != nil || got == nil {
		t.Fatalf("dequeue: got=%v err=%v", got, err)
	}

	// Simulate every replica's reaper ticker firing at (approximately) the
	// same instant -- reclaimStaleScript's atomicity is exactly what must
	// prevent this from re-enqueueing the same job more than once. Without
	// it, N replicas racing here would put N copies of the job back on the
	// queue.
	const replicas = 8
	instances := make([]*redistransport.Queue, replicas)
	for i := range instances {
		instances[i] = redistransport.NewQueue(rdb)
	}

	var wg sync.WaitGroup
	totals := make([]int, replicas)
	for i, inst := range instances {
		wg.Add(1)
		go func(i int, inst *redistransport.Queue) {
			defer wg.Done()
			n, err := inst.ReclaimStale(ctx, 0)
			if err != nil {
				t.Errorf("replica %d reclaim: %v", i, err)
				return
			}
			totals[i] = n
		}(i, inst)
	}
	wg.Wait()

	sum := 0
	for _, n := range totals {
		sum += n
	}
	if sum != 1 {
		t.Fatalf("total reclaimed across %d concurrent replicas = %d, want exactly 1 (duplicate reclaim would silently double-dispatch a real job)", replicas, sum)
	}

	// Queue must contain exactly one copy of the job, not N.
	length, err := dequeuer.Len(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if length != 1 {
		t.Fatalf("queue length after concurrent reclaim = %d, want 1", length)
	}
}
