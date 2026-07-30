package bridge

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/sharanharsoor/runkite/internal/bridge/runnerpb"
	"github.com/sharanharsoor/runkite/internal/transport"
	"github.com/sharanharsoor/runkite/internal/transport/inprocess"
)

// fakeStreamEventsServer is a minimal fake for the generated
// grpc.ClientStreamingServer[RunEventProto, StreamEventsResponse] alias --
// just enough to drive StreamEvents in a unit test without a real gRPC
// connection: Recv() replays a fixed slice of messages then io.EOF,
// SendAndClose captures the final response.
type fakeStreamEventsServer struct {
	grpc.ServerStream
	msgs     []*pb.RunEventProto
	i        int
	response *pb.StreamEventsResponse
}

func (f *fakeStreamEventsServer) Recv() (*pb.RunEventProto, error) {
	if f.i >= len(f.msgs) {
		return nil, io.EOF
	}
	m := f.msgs[f.i]
	f.i++
	return m, nil
}

func (f *fakeStreamEventsServer) SendAndClose(resp *pb.StreamEventsResponse) error {
	f.response = resp
	return nil
}

func (f *fakeStreamEventsServer) Context() context.Context { return context.Background() }

// TestStreamEvents_FirstEventRenewsNotAcks proves the fix for a
// crash-recovery gap: a job's first StreamEvents
// message must NOT fully Ack (remove) it from in-flight tracking --
// otherwise nothing watches the run for the rest of its execution. If
// this regresses back to a full Ack, Nack below would be a no-op and the
// job would never come back on Dequeue.
func TestStreamEvents_FirstEventRenewsNotAcks(t *testing.T) {
	queue := inprocess.NewQueue()
	srv := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	ctx := context.Background()

	runID := "run-heartbeat-1"
	assignment := &transport.RunAssignment{RunID: runID, ThreadID: "t", RunnerKind: "k", GraphID: "g", Input: json.RawMessage(`{}`)}
	if err := queue.Enqueue(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1})
	if err != nil || !resp.HasJob {
		t.Fatalf("GetJob failed: %v %v", resp, err)
	}

	fake := &fakeStreamEventsServer{msgs: []*pb.RunEventProto{
		{RunId: runID, EventJson: `{"event_id":"e1","seq":1,"method":"lifecycle","namespace":[],"data":{"event":"running"},"ts":0}`},
	}}
	if err := srv.StreamEvents(fake); err != nil {
		t.Fatalf("StreamEvents failed: %v", err)
	}
	if fake.response == nil || !fake.response.Ok {
		t.Fatalf("expected ok response, got %v", fake.response)
	}

	// The job must still be in-flight after its first event -- proven by
	// Nack succeeding in re-enqueueing it. A full Ack on first event
	// (the pre-fix behavior) would make this a silent no-op and the job
	// would never reappear on Dequeue.
	if err := queue.Nack(ctx, runID); err != nil {
		t.Fatalf("Nack failed: %v", err)
	}
	again, err := queue.Dequeue(ctx, "k", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Dequeue after Nack failed: %v", err)
	}
	if again == nil || again.RunID != runID {
		t.Fatalf("expected job %q to be re-enqueued by Nack after its first StreamEvents message (proves it was still in-flight, not fully Ack'd), got %v", runID, again)
	}
}

// TestHeartbeat_RenewsInFlightLease proves the Heartbeat RPC keeps a job
// in-flight the same way StreamEvents' first-event Renew does.
func TestHeartbeat_RenewsInFlightLease(t *testing.T) {
	queue := inprocess.NewQueue()
	srv := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	ctx := context.Background()

	runID := "run-heartbeat-2"
	assignment := &transport.RunAssignment{RunID: runID, ThreadID: "t", RunnerKind: "k", GraphID: "g", Input: json.RawMessage(`{}`)}
	if err := queue.Enqueue(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1})
	if err != nil || !resp.HasJob {
		t.Fatalf("GetJob failed: %v %v", resp, err)
	}

	hbResp, err := srv.Heartbeat(ctx, &pb.HeartbeatRequest{RunId: runID})
	if err != nil {
		t.Fatalf("Heartbeat failed: %v", err)
	}
	if !hbResp.Ok {
		t.Fatalf("expected Heartbeat ok=true, got %v", hbResp)
	}

	if err := queue.Nack(ctx, runID); err != nil {
		t.Fatalf("Nack failed: %v", err)
	}
	again, err := queue.Dequeue(ctx, "k", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Dequeue after Nack failed: %v", err)
	}
	if again == nil || again.RunID != runID {
		t.Fatalf("expected job %q still in-flight after Heartbeat (proven by Nack re-enqueueing it), got %v", runID, again)
	}
}

// TestHeartbeat_UnknownRunIDIsNotAnError proves a heartbeat for a run_id
// that's no longer in-flight (already completed, reclaimed, or never
// existed -- e.g. a late heartbeat racing ReportStatus) is not treated as
// an error, since that's an expected, harmless race, not a bug.
func TestHeartbeat_UnknownRunIDIsNotAnError(t *testing.T) {
	queue := inprocess.NewQueue()
	srv := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)

	resp, err := srv.Heartbeat(context.Background(), &pb.HeartbeatRequest{RunId: "never-existed"})
	if err != nil {
		t.Fatalf("Heartbeat for unknown run_id should not error, got: %v", err)
	}
	if !resp.Ok {
		t.Fatalf("expected ok=true even for unknown run_id, got %v", resp)
	}
}

// TestReportStatus_StillFullyAcksOnCompletion proves ReportStatus (not
// StreamEvents' first event any more) is what permanently removes a job
// from in-flight tracking -- Nack after ReportStatus must be a no-op.
func TestReportStatus_StillFullyAcksOnCompletion(t *testing.T) {
	queue := inprocess.NewQueue()
	srv := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	ctx := context.Background()

	runID := "run-heartbeat-3"
	assignment := &transport.RunAssignment{RunID: runID, ThreadID: "t", RunnerKind: "k", GraphID: "g", Input: json.RawMessage(`{}`)}
	if err := queue.Enqueue(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1})
	if err != nil || !resp.HasJob {
		t.Fatalf("GetJob failed: %v %v", resp, err)
	}

	if _, err := srv.ReportStatus(ctx, &pb.ReportStatusRequest{RunId: runID, Status: "success"}); err != nil {
		t.Fatalf("ReportStatus failed: %v", err)
	}

	if err := queue.Nack(ctx, runID); err != nil {
		t.Fatalf("Nack failed: %v", err)
	}
	again, err := queue.Dequeue(ctx, "k", 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Dequeue failed: %v", err)
	}
	if again != nil {
		t.Fatalf("job %q resurrected by Nack after ReportStatus -- should have been fully Ack'd and gone for good, got %v", runID, again)
	}
}
