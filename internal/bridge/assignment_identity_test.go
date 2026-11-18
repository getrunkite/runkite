package bridge

import (
	"context"
	"testing"
	"time"

	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAssignmentIdentityMissingRPCProofCannotBorrowLease(t *testing.T) {
	srv, queue, broker := generationZeroFixture(t, 1)
	for _, proof := range []string{"", "invalid proof", "other-record"} {
		t.Run(proof, func(t *testing.T) {
			request := &pb.HeartbeatRequest{RunId: "generation-zero-run", Generation: 1, RunIncarnationId: proof}
			response, err := srv.Heartbeat(t.Context(), request)
			if proof == "other-record" {
				if err != nil || !response.Superseded {
					t.Fatal("foreign record was not superseded", response, err)
				}
			} else if status.Code(err) != codes.FailedPrecondition {
				t.Fatal("missing or malformed proof accepted", response, err)
			}
			_, err = srv.ReportStatus(t.Context(), &pb.ReportStatusRequest{RunId: request.RunId, Generation: 1, RunIncarnationId: proof, Status: "success"})
			if proof != "other-record" && status.Code(err) != codes.FailedPrecondition {
				t.Fatal("completion accepted missing or malformed proof", err)
			}
			_ = srv.StreamEvents(&fakeStreamEventsServer{msgs: []*pb.RunEventProto{{RunId: request.RunId, Generation: 1, RunIncarnationId: proof, EventJson: `{"event_id":"bad","seq":1,"method":"end","data":{"status":"success"}}`}}})
			assertGenerationZeroLeaseRetained(t, queue, 1)
		})
	}
	events, err := broker.Replay(t.Context(), "generation-zero-run", "fixture-record", 0)
	if err != nil || len(events) != 0 {
		t.Fatal("unproven event entered original record", events, err)
	}
}

func TestAssignmentIdentityRequiredLifecycleAndLegacyJobFailClosed(t *testing.T) {
	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	cancels := inprocess.NewCancelBus()
	if err := queue.Enqueue(t.Context(), &transport.RunAssignment{RunID: "legacy", RunnerKind: "kind", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(queue, broker, cancels, nil)
	request := &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1, AssignmentIdentityVersion: 1}
	if _, err := server.GetJob(t.Context(), request); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("bridge without durable fencing accepted work", err)
	}
	if depth, err := queue.Len(t.Context()); err != nil || depth != 1 {
		t.Fatal("missing lifecycle consumed work", depth, err)
	}
	server = newIdentityServer(queue, broker, cancels, nil)
	if _, err := server.GetJob(t.Context(), request); status.Code(err) != codes.FailedPrecondition {
		t.Fatal("legacy assignment reached an agent", err)
	}
	if current, err := queue.LookupInflight(t.Context(), "legacy"); err != nil || current == nil {
		t.Fatal("legacy assignment was acknowledged", current, err)
	}
}

func TestAssignmentIdentityCleanupAndCancellationStayOnOriginalRecord(t *testing.T) {
	queue := inprocess.NewQueue()
	cancels := inprocess.NewCancelBus()
	server := newIdentityServer(queue, inprocess.NewBroker(), cancels, nil)
	watcher := make(chan *pb.CancelSignal, 4)
	server.addWatcher("kind", watcher)
	defer server.removeWatcher("kind", watcher)
	request := &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1, AssignmentIdentityVersion: 1}
	for _, identity := range []string{"old-record", "new-record"} {
		if err := queue.Enqueue(t.Context(), &transport.RunAssignment{RunID: "reused", RunIncarnationID: identity, RunnerKind: "kind", Generation: 1}); err != nil {
			t.Fatal(err)
		}
		if response, err := server.GetJob(t.Context(), request); err != nil || !response.HasJob {
			t.Fatal("dispatch", response, err)
		}
		if identity == "old-record" {
			if accepted, err := queue.Ack(t.Context(), "reused", identity, 1); err != nil || !accepted {
				t.Fatal("old lease release", accepted, err)
			}
		}
	}
	defer server.cleanupRun("reused", "new-record")
	server.cleanupRun("reused", "old-record")
	server.mu.Lock()
	_, oldWatch := server.runDone[recordKey{"reused", "old-record"}]
	_, newWatch := server.runDone[recordKey{"reused", "new-record"}]
	server.mu.Unlock()
	if oldWatch || !newWatch {
		t.Fatal("cleanup did not select only the original record", oldWatch, newWatch)
	}
	if err := cancels.PublishCancel(context.Background(), "reused", "old-record"); err != nil {
		t.Fatal(err)
	}
	// The runner-kind watch multiplexes all records. An old subscription may
	// deliver its correctly labelled cancellation while cleanup is taking
	// effect; SDKs select the matching physical record before stopping work.
	settled := time.NewTimer(10 * time.Millisecond)
observeOld:
	for {
		select {
		case signal := <-watcher:
			if signal.RunId != "reused" || signal.RunIncarnationId != "old-record" {
				t.Fatal("old cancellation was labelled as another record", signal)
			}
		case <-settled.C:
			break observeOld
		}
	}
	if err := cancels.PublishCancel(context.Background(), "reused", "new-record"); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		select {
		case signal := <-watcher:
			if signal.RunId != "reused" {
				t.Fatal("cancel lost run ID", signal)
			}
			switch signal.RunIncarnationId {
			case "new-record":
				return
			case "old-record":
				// A delayed kind-wide notification retains its original proof.
			default:
				t.Fatal("cancel lost original record", signal)
			}
		case <-deadline.C:
			t.Fatal("old cleanup stopped replacement cancellation watch")
		}
	}
}
