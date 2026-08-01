package natstransport_test

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	natstransport "github.com/getrunkite/runkite/internal/transport/nats"
)

// TestNATSQueue_AckFromDifferentConnectionThanDequeue is the NATS
// analogue of redistransport's own crossnode_test.go: proves the
// in-flight tracking this package's own doc comment describes (a
// JetStream KV entry holding the reply subject, not an in-process Msg
// handle) is genuinely usable from a SEPARATE connection than the one
// that called Dequeue -- the real scenario a load-balanced gRPC bridge
// with no session affinity creates, where the control-plane replica
// that eventually Acks a job is not necessarily the one whose Fetch
// call originally received it.
func TestNATSQueue_AckFromDifferentConnectionThanDequeue(t *testing.T) {
	ncA := getNatsConn(t)
	defer ncA.Close()
	ncB := getNatsConn(t)
	defer ncB.Close()

	ctx := context.Background()
	if err := natstransport.FlushAll(ctx, ncA); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	queueA, err := natstransport.NewQueue(ctx, ncA)
	if err != nil {
		t.Fatalf("NewQueue (A): %v", err)
	}
	queueB, err := natstransport.NewQueue(ctx, ncB)
	if err != nil {
		t.Fatalf("NewQueue (B): %v", err)
	}

	job := &transport.RunAssignment{RunID: "run-crossnode-1", RunnerKind: "test-runner", GraphID: "echo"}
	if err := queueA.Enqueue(ctx, job); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// "Replica A" dequeues the job.
	got, err := queueA.Dequeue(ctx, "test-runner", 2*time.Second)
	if err != nil || got == nil {
		t.Fatalf("Dequeue (A): got=%v err=%v", got, err)
	}

	// "Replica B" (a totally separate NATS connection, its own
	// in-process state) is the one that ends up Acking it -- must
	// still work, proving the tracking isn't tied to whichever
	// connection happened to call Fetch.
	accepted, err := queueB.Ack(ctx, got.RunID, got.Generation)
	if err != nil {
		t.Fatalf("Ack (B): %v", err)
	}
	if !accepted {
		t.Fatal("expected Ack from a different connection than Dequeue to be accepted")
	}

	// Confirm it's genuinely gone -- Nack from either connection on an
	// already-Ack'd job must be a no-op, not a resurrection.
	if err := queueA.Nack(ctx, got.RunID); err != nil {
		t.Fatalf("Nack after cross-connection Ack: %v", err)
	}
	extra, _ := queueA.Dequeue(ctx, "test-runner", 300*time.Millisecond)
	if extra != nil {
		t.Fatalf("job resurrected after cross-connection Ack + Nack: %v", extra)
	}
}

// TestNATSQueue_RenewFromDifferentConnectionThanDequeue proves the same
// cross-connection property for Renew (the heartbeat path) -- a
// runner's periodic heartbeat RPC landing on a different control-plane
// replica than the one that originally dequeued its job must still
// extend the lease.
func TestNATSQueue_RenewFromDifferentConnectionThanDequeue(t *testing.T) {
	ncA := getNatsConn(t)
	defer ncA.Close()
	ncB := getNatsConn(t)
	defer ncB.Close()

	ctx := context.Background()
	if err := natstransport.FlushAll(ctx, ncA); err != nil {
		t.Fatalf("FlushAll: %v", err)
	}

	queueA, err := natstransport.NewQueue(ctx, ncA)
	if err != nil {
		t.Fatalf("NewQueue (A): %v", err)
	}
	queueB, err := natstransport.NewQueue(ctx, ncB)
	if err != nil {
		t.Fatalf("NewQueue (B): %v", err)
	}

	job := &transport.RunAssignment{RunID: "run-crossnode-2", RunnerKind: "test-runner", GraphID: "echo"}
	if err := queueA.Enqueue(ctx, job); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, err := queueA.Dequeue(ctx, "test-runner", 2*time.Second)
	if err != nil || got == nil {
		t.Fatalf("Dequeue (A): got=%v err=%v", got, err)
	}

	current, err := queueB.Renew(ctx, got.RunID, got.Generation)
	if err != nil {
		t.Fatalf("Renew (B): %v", err)
	}
	if !current {
		t.Fatal("expected Renew from a different connection than Dequeue to report current=true")
	}

	// Job must still be genuinely in-flight after that cross-connection
	// Renew -- proven via ReclaimStale(0), which only reclaims something
	// still tracked.
	if n, err := queueA.ReclaimStale(ctx, 0); err != nil || n != 1 {
		t.Fatalf("expected the still-in-flight job to be reclaimable after cross-connection Renew, got n=%d err=%v", n, err)
	}
}
