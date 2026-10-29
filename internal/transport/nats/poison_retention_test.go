package natstransport_test

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	natstransport "github.com/getrunkite/runkite/internal/transport/nats"
	"github.com/nats-io/nats.go/jetstream"
)

func TestExhaustedReclaimRetainsDeliveryUntilDurableAcknowledgement(t *testing.T) {
	nc := getNatsConn(t)
	defer nc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := natstransport.FlushAll(ctx, nc); err != nil {
		t.Fatal(err)
	}
	queue, err := natstransport.NewQueue(ctx, nc)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Enqueue(ctx, &transport.RunAssignment{RunID: "exhausted", RunnerKind: "kind", GraphID: "graph", TenantID: "acme", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	job, err := queue.Dequeue(ctx, "kind", time.Second)
	if err != nil || job == nil {
		t.Fatalf("dequeue=%+v %v", job, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		reclaimed, dead, err := queue.ReclaimStale(ctx, 0, int(job.Generation))
		if err != nil || reclaimed != 0 || len(dead) != 1 {
			t.Fatalf("tick=%d reclaimed=%d dead=%+v err=%v", attempt, reclaimed, dead, err)
		}
		pill := dead[0]
		if pill.RunID != job.RunID || pill.Generation != job.Generation+1 || pill.AgentID != "graph" || pill.TenantID != "acme" {
			t.Fatalf("bad poison pill: %+v", pill)
		}
		actual, err := queue.LookupInflight(ctx, job.RunID)
		if err != nil || actual == nil || actual.Generation != pill.Generation-1 {
			t.Fatalf("inflight attempt removed before durable completion: %+v %v", actual, err)
		}
		if err := nc.FlushWithContext(ctx); err != nil {
			t.Fatal(err)
		}
		js, err := jetstream.New(nc)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := js.Stream(ctx, "RUNKITE_JOBS")
		if err != nil {
			t.Fatal(err)
		}
		info, err := stream.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Msgs != 1 {
			t.Fatalf("exhausted delivery acknowledged before durable completion: messages=%d", info.State.Msgs)
		}
	}
	if accepted, err := queue.Ack(ctx, job.RunID, job.Generation); err != nil || !accepted {
		t.Fatalf("durable completion Ack=%v %v", accepted, err)
	}
	if actual, err := queue.LookupInflight(ctx, job.RunID); err != nil || actual != nil {
		t.Fatalf("inflight retained after Ack: %+v %v", actual, err)
	}
	if _, dead, err := queue.ReclaimStale(ctx, 0, int(job.Generation)); err != nil || len(dead) != 0 {
		t.Fatalf("acknowledged poison returned again: %+v %v", dead, err)
	}
}
