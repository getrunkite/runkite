package bridge

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

// successfulLeaseChanges records effects, not just method invocations. A
// rejected request may still call Renew to check its generation atomically.
type successfulLeaseChanges struct {
	*inprocess.Queue
	renewals         int
	acknowledgements int
}

func (q *successfulLeaseChanges) Renew(ctx context.Context, id string, generation int64) (bool, error) {
	current, err := q.Queue.Renew(ctx, id, generation)
	if err == nil && current {
		q.renewals++
	}
	return current, err
}

func (q *successfulLeaseChanges) Ack(ctx context.Context, id string, generation int64) (bool, error) {
	accepted, err := q.Queue.Ack(ctx, id, generation)
	if err == nil && accepted {
		q.acknowledgements++
	}
	return accepted, err
}

func generationZeroFixture(t *testing.T, generation int64) (*Server, *successfulLeaseChanges, *inprocess.Broker) {
	t.Helper()
	queue := &successfulLeaseChanges{Queue: inprocess.NewQueue()}
	broker := inprocess.NewBroker()
	assignment := &transport.RunAssignment{RunID: "generation-zero-run", ThreadID: "thread", RunnerKind: "kind", GraphID: "graph", Generation: generation, Input: json.RawMessage(`{}`)}
	if err := queue.Enqueue(context.Background(), assignment); err != nil {
		t.Fatal(err)
	}
	job, err := queue.Dequeue(context.Background(), "kind", time.Second)
	if err != nil || job == nil {
		t.Fatalf("dequeue: job=%v err=%v", job, err)
	}
	return NewServer(queue, broker, inprocess.NewCancelBus(), nil), queue, broker
}

func assertGenerationZeroLeaseRetained(t *testing.T, queue *successfulLeaseChanges, generation int64) {
	t.Helper()
	assignment, err := queue.LookupInflight(context.Background(), "generation-zero-run")
	if err != nil || assignment == nil || assignment.Generation != generation {
		t.Fatalf("rejected zero-generation request changed the live assignment: assignment=%+v err=%v", assignment, err)
	}
	if queue.renewals != 0 || queue.acknowledgements != 0 {
		t.Fatalf("rejected zero-generation request changed the lease: renewals=%d acknowledgements=%d", queue.renewals, queue.acknowledgements)
	}
}

func TestStreamEvents_ZeroGenerationCannotBorrowLivePositiveAttempt(t *testing.T) {
	for _, generation := range []int64{1, 2} {
		t.Run(strconv.FormatInt(generation, 10), func(t *testing.T) {
			srv, queue, broker := generationZeroFixture(t, generation)
			stale := &fakeStreamEventsServer{msgs: []*pb.RunEventProto{{RunId: "generation-zero-run", Generation: 0, EventJson: `{"event_id":"stale","seq":1,"method":"values","generation":2,"data":{"answer":"stale"}}`}}}
			// Dropping the event or rejecting the RPC are both safe. A claimed generation
			// inside EventJson must never replace the missing outer fencing token.
			_ = srv.StreamEvents(stale)
			events, err := broker.Replay(context.Background(), "generation-zero-run", 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Fatalf("zero-generation event was published as current: %+v", events)
			}
			assertGenerationZeroLeaseRetained(t, queue, generation)
			current := &fakeStreamEventsServer{msgs: []*pb.RunEventProto{{RunId: "generation-zero-run", Generation: generation, EventJson: `{"event_id":"current","seq":1,"method":"values","generation":999,"data":{"answer":"current"}}`}}}
			if err := srv.StreamEvents(current); err != nil {
				t.Fatal(err)
			}
			events, err = broker.Replay(context.Background(), "generation-zero-run", 0)
			if err != nil || len(events) != 1 {
				t.Fatalf("exact-generation event missing: events=%+v err=%v", events, err)
			}
			if events[0].Generation != generation || queue.renewals == 0 {
				t.Fatalf("exact-generation event not fenced correctly: event=%+v renewals=%d", events[0], queue.renewals)
			}
		})
	}
}

func TestHeartbeat_ZeroGenerationCannotExtendLivePositiveAttempt(t *testing.T) {
	for _, generation := range []int64{1, 2} {
		t.Run(strconv.FormatInt(generation, 10), func(t *testing.T) {
			srv, queue, _ := generationZeroFixture(t, generation)
			response, err := srv.Heartbeat(context.Background(), &pb.HeartbeatRequest{RunId: "generation-zero-run", Generation: 0})
			if err == nil && (response == nil || !response.Superseded) {
				t.Fatalf("zero-generation heartbeat was accepted: response=%+v", response)
			}
			assertGenerationZeroLeaseRetained(t, queue, generation)
			response, err = srv.Heartbeat(context.Background(), &pb.HeartbeatRequest{RunId: "generation-zero-run", Generation: generation})
			if err != nil || response == nil || !response.Ok || response.Superseded || queue.renewals == 0 {
				t.Fatalf("exact-generation heartbeat rejected: response=%+v renewals=%d err=%v", response, queue.renewals, err)
			}
		})
	}
}

func TestReportStatus_ZeroGenerationCannotPersistOrAckLivePositiveAttempt(t *testing.T) {
	for _, generation := range []int64{1, 2} {
		t.Run(strconv.FormatInt(generation, 10), func(t *testing.T) {
			srv, queue, _ := generationZeroFixture(t, generation)
			var persisted []*state.RunCompletion
			srv.SetRunLifecycle(nil, func(_ context.Context, c *state.RunCompletion) (bool, error) {
				copy := *c
				persisted = append(persisted, &copy)
				return true, nil
			})
			response, err := srv.ReportStatus(context.Background(), &pb.ReportStatusRequest{RunId: "generation-zero-run", Generation: 0, Status: "success"})
			if err == nil && (response == nil || !response.Superseded) {
				t.Fatalf("zero-generation completion was accepted: response=%+v", response)
			}
			if len(persisted) != 0 {
				t.Fatalf("zero-generation completion reached durable persistence: %+v", persisted)
			}
			assertGenerationZeroLeaseRetained(t, queue, generation)
			response, err = srv.ReportStatus(context.Background(), &pb.ReportStatusRequest{RunId: "generation-zero-run", Generation: generation, Status: "success"})
			if err != nil || response == nil || !response.Ok || response.Superseded {
				t.Fatalf("exact-generation completion rejected: response=%+v err=%v", response, err)
			}
			if len(persisted) != 1 || persisted[0].Generation != generation || queue.acknowledgements != 1 {
				t.Fatalf("bad exact-generation persistence: persisted=%+v acknowledgements=%d", persisted, queue.acknowledgements)
			}
			assignment, err := queue.LookupInflight(context.Background(), "generation-zero-run")
			if err != nil || assignment != nil {
				t.Fatalf("exact-generation completion did not release lease: assignment=%+v err=%v", assignment, err)
			}
		})
	}
}

func TestGenerationZero_ActualLegacyAssignmentRemainsCompatible(t *testing.T) {
	srv, queue, broker := generationZeroFixture(t, 0)
	legacy := &fakeStreamEventsServer{msgs: []*pb.RunEventProto{{RunId: "generation-zero-run", Generation: 0, EventJson: `{"event_id":"legacy","seq":1,"method":"values","data":{"answer":"legacy"}}`}}}
	if err := srv.StreamEvents(legacy); err != nil {
		t.Fatalf("legacy stream rejected: %v", err)
	}
	events, err := broker.Replay(context.Background(), "generation-zero-run", 0)
	if err != nil || len(events) != 1 || events[0].Generation != 0 {
		t.Fatalf("legacy event missing: events=%+v err=%v", events, err)
	}
	response, err := srv.Heartbeat(context.Background(), &pb.HeartbeatRequest{RunId: "generation-zero-run", Generation: 0})
	if err != nil || response == nil || !response.Ok || response.Superseded {
		t.Fatalf("legacy heartbeat rejected: response=%+v err=%v", response, err)
	}
	completions := 0
	srv.SetRunLifecycle(nil, func(_ context.Context, c *state.RunCompletion) (bool, error) {
		completions++
		if c.Generation != 0 {
			t.Fatalf("legacy completion relabeled as generation %d", c.Generation)
		}
		return true, nil
	})
	completed, err := srv.ReportStatus(context.Background(), &pb.ReportStatusRequest{RunId: "generation-zero-run", Generation: 0, Status: "success"})
	if err != nil || completed == nil || !completed.Ok || completed.Superseded || completions != 1 || queue.acknowledgements != 1 {
		t.Fatalf("legacy completion rejected: response=%+v completions=%d acknowledgements=%d err=%v", completed, completions, queue.acknowledgements, err)
	}
}
