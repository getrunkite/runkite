package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
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

// TestStreamEvents_TerminalEventFromSupersededGenerationIsDropped proves
// a real, confirmed gap: a runner that gets reclaimed while genuinely
// still executing, then finishes its own now-pointless work anyway and
// streams its "end" event, must not have that terminal event published
// -- a caller long-polling for this run's result (or any SSE
// subscriber) would otherwise see a stale, wrong outcome BEFORE the
// real one arrives, since a client-streaming RPC's individual events
// have no per-message rejection path the way Heartbeat/ReportStatus do.
func TestStreamEvents_TerminalEventFromSupersededGenerationIsDropped(t *testing.T) {
	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	srv := NewServer(queue, broker, inprocess.NewCancelBus(), nil)
	ctx := context.Background()

	runID := "run-stale-terminal-event"
	assignment := &transport.RunAssignment{RunID: runID, ThreadID: "t", RunnerKind: "k", GraphID: "g", Input: json.RawMessage(`{}`), Generation: 1}
	if err := queue.Enqueue(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1}); err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	// A second runner reclaims the job (generation bumps to 2) and is
	// dispatched the redelivered work -- mirrors a genuine transient
	// blip that made the first runner miss the reaper's window.
	if n, _, err := queue.ReclaimStale(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("ReclaimStale: n=%d err=%v", n, err)
	}
	if _, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1}); err != nil {
		t.Fatalf("GetJob for replacement: %v", err)
	}

	// Subscribe to the run's events BEFORE either runner streams its
	// terminal event, the way waitForRunResult (internal/api/runs.go)
	// and any SSE client actually do.
	eventCh, err := broker.Subscribe(ctx, runID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// The FIRST (stale, generation 1) runner's blip was transient -- it
	// finishes its own now-superseded execution anyway and streams its
	// "end" event.
	staleStream := &fakeStreamEventsServer{msgs: []*pb.RunEventProto{
		{RunId: runID, Generation: 1, EventJson: `{"event_id":"e1","seq":1,"method":"end","namespace":[],"data":{"status":"interrupted"},"ts":0}`},
	}}
	if err := srv.StreamEvents(staleStream); err != nil {
		t.Fatalf("StreamEvents (stale) failed: %v", err)
	}

	// The SECOND (current, generation 2) runner then genuinely
	// completes and streams its own, real "end" event.
	currentStream := &fakeStreamEventsServer{msgs: []*pb.RunEventProto{
		{RunId: runID, Generation: 2, EventJson: `{"event_id":"e2","seq":1,"method":"end","namespace":[],"data":{"status":"success"},"ts":0}`},
	}}
	if err := srv.StreamEvents(currentStream); err != nil {
		t.Fatalf("StreamEvents (current) failed: %v", err)
	}

	// A subscriber watching this run's event stream must see exactly
	// ONE terminal event -- the real one -- not the stale one first.
	event, ok := <-eventCh
	if !ok {
		t.Fatal("expected exactly one event on the channel, got none (channel closed early)")
	}
	if event.Method != "end" {
		t.Fatalf("expected the terminal event, got method=%q", event.Method)
	}
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatalf("unmarshal event data: %v", err)
	}
	if data["status"] != "success" {
		t.Fatalf("expected the CURRENT generation's real status \"success\", got %v -- the stale generation's \"interrupted\" event was not dropped", data["status"])
	}
	if _, stillOpen := <-eventCh; stillOpen {
		t.Fatal("expected the channel to be closed after the one real terminal event, got a second event")
	}
}

// TestHeartbeat_SupersededGenerationSignalsRunnerToStop proves fencing
// end to end through the actual gRPC handlers, not just the JobQueue
// layer: a runner reclaimed while genuinely still executing (its blip
// was transient, it finishes anyway) gets Superseded=true on its NEXT
// heartbeat, not an error and not a silent "ok" that lets it keep
// burning resources on a run someone else now owns.
func TestHeartbeat_SupersededGenerationSignalsRunnerToStop(t *testing.T) {
	queue := inprocess.NewQueue()
	srv := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), nil)
	ctx := context.Background()

	runID := "run-fence-bridge-1"
	assignment := &transport.RunAssignment{RunID: runID, ThreadID: "t", RunnerKind: "k", GraphID: "g", Input: json.RawMessage(`{}`), Generation: 1}
	if err := queue.Enqueue(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1})
	if err != nil || !resp.HasJob {
		t.Fatalf("GetJob failed: %v %v", resp, err)
	}
	var original transport.RunAssignment
	if err := json.Unmarshal([]byte(resp.AssignmentJson), &original); err != nil {
		t.Fatalf("unmarshal assignment: %v", err)
	}
	if original.Generation != 1 {
		t.Fatalf("expected generation 1, got %d", original.Generation)
	}

	// This runner's connection blips -- another replica's reaper
	// reclaims and redispatches the job (generation bumps to 2) to a
	// second runner, before this original runner's own Heartbeat call
	// arrives.
	if n, _, err := queue.ReclaimStale(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("ReclaimStale: n=%d err=%v", n, err)
	}
	if _, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1}); err != nil {
		t.Fatalf("GetJob for the redispatched replacement failed: %v", err)
	}

	// The original runner's blip turns out to be transient -- it's
	// still executing, unaware it's been reclaimed, and calls Heartbeat
	// presenting the generation it was actually handed (1, now stale).
	hbResp, err := srv.Heartbeat(ctx, &pb.HeartbeatRequest{RunId: runID, Generation: original.Generation})
	if err != nil {
		t.Fatalf("Heartbeat should not itself error just because it's stale: %v", err)
	}
	if !hbResp.Ok {
		t.Fatalf("expected Ok=true (the RPC itself succeeded) even when superseded, got %v", hbResp)
	}
	if !hbResp.Superseded {
		t.Fatalf("expected Superseded=true for a stale generation, got %v", hbResp)
	}

	// A heartbeat from the CURRENT (replacement) generation must NOT be
	// superseded.
	hbResp2, err := srv.Heartbeat(ctx, &pb.HeartbeatRequest{RunId: runID, Generation: 2})
	if err != nil {
		t.Fatalf("Heartbeat for the current generation failed: %v", err)
	}
	if hbResp2.Superseded {
		t.Fatalf("expected Superseded=false for the current generation, got %v", hbResp2)
	}
}

// TestReportStatus_SupersededGenerationIsIgnoredNotAppliedToRunState
// proves the same fencing at the OTHER end of a run's lifecycle: the
// stale runner from the test above eventually finishes its own
// (worthless) execution and calls ReportStatus. That report must be
// ignored -- not applied via the statusCallback -- so it can't clobber
// whatever the real, current-generation runner reports.
func TestReportStatus_SupersededGenerationIsIgnoredNotAppliedToRunState(t *testing.T) {
	queue := inprocess.NewQueue()
	var reportedStatuses []string
	srv := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), func(runID, status, errorMsg string) {
		reportedStatuses = append(reportedStatuses, status)
	})
	ctx := context.Background()

	runID := "run-fence-bridge-2"
	assignment := &transport.RunAssignment{RunID: runID, ThreadID: "t", RunnerKind: "k", GraphID: "g", Input: json.RawMessage(`{}`), Generation: 1}
	if err := queue.Enqueue(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1}); err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	if n, _, err := queue.ReclaimStale(ctx, 0, 0); err != nil || n != 1 {
		t.Fatalf("ReclaimStale: n=%d err=%v", n, err)
	}
	if _, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1}); err != nil {
		t.Fatalf("GetJob for replacement: %v", err)
	}

	// The stale (generation 1) runner reports success for its own,
	// no-longer-relevant execution -- must be ignored entirely.
	resp, err := srv.ReportStatus(ctx, &pb.ReportStatusRequest{RunId: runID, Status: "success", Generation: 1})
	if err != nil {
		t.Fatalf("ReportStatus should not error just because it's superseded: %v", err)
	}
	if !resp.Superseded {
		t.Fatalf("expected Superseded=true for the stale generation-1 report, got %v", resp)
	}
	if len(reportedStatuses) != 0 {
		t.Fatalf("statusCallback must NOT have been invoked for a superseded report, got %v", reportedStatuses)
	}

	// The genuinely current (generation 2) runner's real report must
	// still go through normally.
	resp2, err := srv.ReportStatus(ctx, &pb.ReportStatusRequest{RunId: runID, Status: "error", Generation: 2})
	if err != nil {
		t.Fatalf("ReportStatus for the current generation failed: %v", err)
	}
	if resp2.Superseded {
		t.Fatalf("expected Superseded=false for the current generation, got %v", resp2)
	}
	if len(reportedStatuses) != 1 || reportedStatuses[0] != "error" {
		t.Fatalf("expected exactly one applied status (\"error\", from the current generation), got %v", reportedStatuses)
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

// ackErrorQueue wraps a real queue but makes Ack always fail with a
// transport-level error, simulating a backend outage (e.g. Redis
// briefly unreachable) rather than a genuine fencing rejection.
type ackErrorQueue struct {
	*inprocess.Queue
}

func (q *ackErrorQueue) Ack(ctx context.Context, runID string, generation int64) (bool, error) {
	return false, errors.New("simulated backend error")
}

// TestReportStatus_QueueErrorStillAppliesStatusNotTreatedAsSuperseded
// proves a real, confirmed bug: a transport-level error from Ack (e.g.
// Redis briefly unreachable) is not evidence that this report is
// stale, and must not be treated the same as a genuine fencing
// rejection. Silently dropping a real run's real, successful result
// just because the queue backend hiccuped would be a worse outcome
// than the rare case a stale report slips through during that same
// outage (reclaim itself needs the same backend, so a second runner
// reporting concurrently during an outage is unlikely anyway).
func TestReportStatus_QueueErrorStillAppliesStatusNotTreatedAsSuperseded(t *testing.T) {
	queue := &ackErrorQueue{Queue: inprocess.NewQueue()}
	var reportedStatuses []string
	srv := NewServer(queue, inprocess.NewBroker(), inprocess.NewCancelBus(), func(runID, status, errorMsg string) {
		reportedStatuses = append(reportedStatuses, status)
	})
	ctx := context.Background()

	runID := "run-ack-error"
	assignment := &transport.RunAssignment{RunID: runID, ThreadID: "t", RunnerKind: "k", GraphID: "g", Input: json.RawMessage(`{}`), Generation: 1}
	if err := queue.Enqueue(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "k", TimeoutSeconds: 1}); err != nil {
		t.Fatalf("GetJob: %v", err)
	}

	resp, err := srv.ReportStatus(ctx, &pb.ReportStatusRequest{RunId: runID, Status: "success", Generation: 1})
	if err != nil {
		t.Fatalf("ReportStatus should not itself error just because Ack did: %v", err)
	}
	if resp.Superseded {
		t.Fatalf("a queue/transport error must not be reported as Superseded=true -- that's a fencing signal, not an infrastructure one, got %v", resp)
	}
	if len(reportedStatuses) != 1 || reportedStatuses[0] != "success" {
		t.Fatalf("expected the real status \"success\" to still be applied despite the Ack error, got %v", reportedStatuses)
	}
}
