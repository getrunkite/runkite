package natstransport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/transport"
	natstransport "github.com/getrunkite/runkite/internal/transport/nats"
	"github.com/nats-io/nats.go/jetstream"
)

func TestNATSRunIdentityRejectsWildcardReplayAndSubscriptions(t *testing.T) {
	nc := getNatsConn(t)
	defer nc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := natstransport.FlushAll(ctx, nc); err != nil {
		t.Fatal(err)
	}
	broker, err := natstransport.NewBroker(ctx, nc)
	if err != nil {
		t.Fatal(err)
	}
	for _, victim := range []string{"another-tenant", "another-tenant.child"} {
		if err := broker.Publish(ctx, victim, &transport.RunEvent{EventID: victim, Seq: 1, Method: "values", Data: json.RawMessage(`{"private":"another run"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"*", ">", "another-tenant.*"} {
		if events, err := broker.Replay(ctx, id, 0); err == nil {
			t.Errorf("run ID %q became a subject filter and replayed %d unrelated events: %+v", id, len(events), events)
		}
		watchCtx, stop := context.WithCancel(ctx)
		if events, err := broker.Subscribe(watchCtx, id); err == nil {
			if err := broker.Publish(ctx, "another-tenant.child", &transport.RunEvent{EventID: "new-private", Seq: 2, Method: "values", Data: json.RawMessage(`{"private":"live"}`)}); err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-events:
				t.Errorf("run ID %q subscribed to another run: %+v", id, event)
			case <-time.After(100 * time.Millisecond):
				t.Errorf("run ID %q was accepted as a wildcard subscription", id)
			}
		}
		stop()
	}
	bus := natstransport.NewCancelBus(nc)
	for _, id := range []string{"*", ">"} {
		watchCtx, stop := context.WithCancel(ctx)
		if signals, err := bus.SubscribeCancel(watchCtx, id); err == nil {
			if err := nc.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := bus.PublishCancel(ctx, "another-tenant"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-signals:
				t.Errorf("run ID %q received cancellation for another run", id)
			case <-time.After(100 * time.Millisecond):
				t.Errorf("run ID %q was accepted as a wildcard cancellation subscription", id)
			}
		}
		stop()
	}
}

func TestNATSRunIdentityPreservesLiteralPartitions(t *testing.T) {
	nc := getNatsConn(t)
	defer nc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := natstransport.FlushAll(ctx, nc); err != nil {
		t.Fatal(err)
	}
	queue, err := natstransport.NewQueue(ctx, nc)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := natstransport.NewBroker(ctx, nc)
	if err != nil {
		t.Fatal(err)
	}
	bus := natstransport.NewCancelBus(nc)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// A rejected pre-upgrade assignment remains stored for explicit repair;
	// rejecting a dispatch is not permission to acknowledge or delete it.
	legacy, err := json.Marshal(&transport.RunAssignment{RunID: "*", RunnerKind: "legacy-invalid-test", GraphID: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	retained, err := js.Publish(ctx, "rk.jobs.legacy-invalid-test", legacy)
	if err != nil {
		t.Fatal(err)
	}
	if assignment, err := queue.Dequeue(ctx, "legacy-invalid-test", 2*time.Second); err == nil || assignment != nil {
		t.Fatalf("unsupported legacy assignment reached a worker: %+v %v", assignment, err)
	}
	stream, err := js.Stream(ctx, "RUNKITE_JOBS")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := stream.GetMsg(ctx, retained.Sequence)
	if err != nil || !bytes.Equal(stored.Data, legacy) {
		t.Fatalf("dispatch rejection deleted legacy work: %+v %v", stored, err)
	}
	for _, id := range []string{"Upper_case-ID", "tenant/run=1", "tenant.child"} {
		t.Run(id, func(t *testing.T) {
			job := &transport.RunAssignment{RunID: id, RunnerKind: "literal-test", GraphID: "echo"}
			if err := queue.Enqueue(ctx, job); err != nil {
				t.Fatal(err)
			}
			assignment, err := queue.Dequeue(ctx, "literal-test", 2*time.Second)
			if err != nil || assignment == nil || assignment.RunID != id {
				t.Fatalf("literal queue identity changed: %+v %v", assignment, err)
			}
			if accepted, err := queue.Ack(ctx, id, assignment.Generation); err != nil || !accepted {
				t.Fatalf("literal ack: %v %v", accepted, err)
			}
			other := "neighbor-" + id
			for _, runID := range []string{id, other} {
				if err := broker.Publish(ctx, runID, &transport.RunEvent{EventID: runID, Seq: 1, Method: "values", Data: json.RawMessage(`{}`)}); err != nil {
					t.Fatal(err)
				}
			}
			past, err := broker.Replay(ctx, id, 0)
			if err != nil || len(past) != 1 || past[0].EventID != id {
				t.Fatalf("literal replay crossed partitions: %+v %v", past, err)
			}
			watchCtx, stop := context.WithCancel(ctx)
			defer stop()
			live, err := broker.Subscribe(watchCtx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.Publish(ctx, other, &transport.RunEvent{EventID: "wrong", Seq: 2, Method: "values", Data: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if err := broker.Publish(ctx, id, &transport.RunEvent{EventID: "right", Seq: 2, Method: "end", Data: json.RawMessage(`{"status":"success"}`)}); err != nil {
				t.Fatal(err)
			}
			select {
			case event := <-live:
				if event == nil || event.EventID != "right" {
					t.Fatalf("literal live subscription crossed partitions: %+v", event)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			cancelled, err := bus.SubscribeCancel(watchCtx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := bus.PublishCancel(ctx, other); err != nil {
				t.Fatal(err)
			}
			if err := bus.PublishCancel(ctx, id); err != nil {
				t.Fatal(err)
			}
			select {
			case <-cancelled:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
