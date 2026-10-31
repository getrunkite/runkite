package natstransport_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/getrunkite/runkite/internal/transport"
	natstransport "github.com/getrunkite/runkite/internal/transport/nats"
)

func TestNATSQueueConsumerNameCollisionDoesNotRetargetAnotherKind(t *testing.T) {
	ncA, ncB := getNatsConn(t), getNatsConn(t)
	defer ncA.Close()
	defer ncB.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := natstransport.FlushAll(ctx, ncA); err != nil {
		t.Fatal(err)
	}
	queueA, err := natstransport.NewQueue(ctx, ncA)
	if err != nil {
		t.Fatal(err)
	}
	queueB, err := natstransport.NewQueue(ctx, ncB)
	if err != nil {
		t.Fatal(err)
	}
	first := &transport.RunAssignment{RunID: "kind-first", RunnerKind: "a.b", GraphID: "first"}
	if err := queueA.Enqueue(ctx, first); err != nil {
		t.Fatal(err)
	}
	// Both legacy names map to jobs-a_b. Refuse the conflicting config rather
	// than changing a durable consumer another control plane already uses.
	other := &transport.RunAssignment{RunID: "kind-other", RunnerKind: "a_b", GraphID: "other"}
	if err := queueB.Enqueue(ctx, other); err == nil {
		t.Error("colliding kind silently reconfigured an existing durable consumer")
	}
	js, err := jetstream.New(ncA)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.Consumer(ctx, "RUNKITE_JOBS", "jobs-a_b")
	if err != nil {
		t.Fatal(err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Config.FilterSubject != "rk.jobs.a.b" {
		t.Errorf("original consumer filter changed to %q", info.Config.FilterSubject)
	}
	got, err := queueA.Dequeue(ctx, "a.b", 2*time.Second)
	if err != nil || got == nil || got.RunID != first.RunID || got.RunnerKind != first.RunnerKind {
		t.Fatalf("original queue received another kind: job=%v err=%v", got, err)
	}
	if accepted, err := queueA.Ack(ctx, got.RunID, got.Generation); err != nil || !accepted {
		t.Fatalf("ack original: accepted=%t err=%v", accepted, err)
	}
	// The same native config is still idempotent across control planes. A
	// create-only API must not block an ordinary second replica of one kind.
	second := &transport.RunAssignment{RunID: "kind-second", RunnerKind: "a.b", GraphID: "second"}
	if err := queueB.Enqueue(ctx, second); err != nil {
		t.Fatalf("same-kind consumer recreation was not idempotent: %v", err)
	}
	got, err = queueB.Dequeue(ctx, "a.b", 2*time.Second)
	if err != nil || got == nil || got.RunID != second.RunID {
		t.Fatalf("same-kind replica cannot fetch its work: job=%v err=%v", got, err)
	}
	if accepted, err := queueB.Ack(ctx, got.RunID, got.Generation); err != nil || !accepted {
		t.Fatalf("ack replica: accepted=%t err=%v", accepted, err)
	}
	if job, err := queueB.Dequeue(ctx, "a_b", 10*time.Millisecond); err == nil || job != nil {
		t.Fatalf("conflicting kind unexpectedly accessed work: job=%v err=%v", job, err)
	}
}
