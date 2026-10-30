package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/bridge"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type completionFaultStore struct {
	state.Store
	fail bool
}

func (s *completionFaultStore) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	if s.fail {
		return state.CompletionResult{}, errors.New("injected SQL outage")
	}
	return s.Store.CompleteRun(ctx, c)
}

type completionStream struct {
	grpc.ServerStream
	msgs     []*pb.RunEventProto
	i        int
	response *pb.StreamEventsResponse
	closeErr error
}

func (s *completionStream) Context() context.Context { return context.Background() }
func (s *completionStream) Recv() (*pb.RunEventProto, error) {
	if s.i >= len(s.msgs) {
		return nil, io.EOF
	}
	m := s.msgs[s.i]
	s.i++
	return m, nil
}
func (s *completionStream) SendAndClose(r *pb.StreamEventsResponse) error {
	s.response = r
	return s.closeErr
}
func completionEvent(gen, seq int64, method, data string) *pb.RunEventProto {
	raw, _ := json.Marshal(&transport.RunEvent{EventID: method, Seq: seq, Method: method, Data: json.RawMessage(data)})
	return &pb.RunEventProto{RunId: "run", Generation: gen, EventJson: string(raw)}
}
func completionFixture(t *testing.T) (*sqlite.SQLiteStore, *inprocess.Queue, *inprocess.Broker, *inprocess.CancelBus) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	q := inprocess.NewQueue()
	if err := q.Enqueue(ctx, &transport.RunAssignment{RunID: "run", ThreadID: "thread", RunnerKind: "kind", GraphID: "agent", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	return store, q, inprocess.NewBroker(), inprocess.NewCancelBus()
}
func TestDurableCompletionFailureRetainsLeaseAndRetryIsIdempotent(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &completionFaultStore{Store: store, fail: true}
	plane := api.NewServer(fault, q, b, c)
	rpc := bridge.NewServer(q, b, c, nil)
	rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
	ctx := context.Background()
	if r, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1}); err != nil || !r.HasJob {
		t.Fatal(r, err)
	}
	started, err := store.GetRun(ctx, "run")
	if err != nil || started.Status != models.RunStatusRunning || state.MetadataGeneration(started.Metadata, state.AttemptGenerationKey) != 1 {
		t.Fatalf("GetJob did not persist running attempt: run=%+v err=%v", started, err)
	}
	stream := &completionStream{msgs: []*pb.RunEventProto{completionEvent(1, 1, "values", `{"answer":"kept"}`), completionEvent(1, 2, "end", `{"status":"success"}`)}}
	if err := rpc.StreamEvents(stream); err != nil {
		t.Fatal(err)
	}
	req := &pb.ReportStatusRequest{RunId: "run", Generation: 1, Status: "success"}
	if _, err := rpc.ReportStatus(ctx, req); status.Code(err) != codes.Unavailable {
		t.Fatalf("expected retryable error, got %v", err)
	}
	inflight, _ := q.LookupInflight(ctx, "run")
	saved, _ := store.GetRun(ctx, "run")
	thread, _ := store.GetThread(ctx, "thread")
	if inflight == nil || saved.Status != models.RunStatusRunning || thread.Status != models.ThreadStatusBusy {
		t.Fatal("failed persistence acknowledged", inflight, saved, thread)
	}
	fault.fail = false
	for i := 0; i < 2; i++ {
		resp, err := rpc.ReportStatus(ctx, req)
		if err != nil || !resp.Ok || resp.Superseded {
			t.Fatal("completion/retry", resp, err)
		}
	}
	saved, _ = store.GetRun(ctx, "run")
	thread, _ = store.GetThread(ctx, "thread")
	history, _ := store.ListCheckpoints(ctx, "thread", 10, "")
	if saved.Status != models.RunStatusSuccess || thread.Values["answer"] != "kept" || len(history) != 1 {
		t.Fatalf("bad durable result: run=%+v thread=%+v checkpoints=%d", saved, thread, len(history))
	}
}

func TestLostStreamAcknowledgementRecoversAgentOutcomeBeforeRedispatch(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	rpc := bridge.NewServer(q, b, c, nil)
	rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
	ctx := context.Background()
	if response, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1}); err != nil || !response.HasJob {
		t.Fatalf("first dispatch: response=%v err=%v", response, err)
	}
	ackLost := errors.New("stream close acknowledgement lost")
	stream := &completionStream{
		msgs: []*pb.RunEventProto{
			completionEvent(1, 1, "values", `{"answer":"agent succeeded"}`),
			completionEvent(1, 2, "end", `{"status":"success"}`),
		},
		closeErr: ackLost,
	}
	if err := rpc.StreamEvents(stream); !errors.Is(err, ackLost) {
		t.Fatalf("lost acknowledgement not exposed: %v", err)
	}
	// A runner cannot confirm delivery, so it sends no replacement status.
	// The next dispatch must recover the persisted terminal instead of work.
	if reclaimed, _, err := q.ReclaimStale(ctx, -time.Second, 3); err != nil || reclaimed != 1 {
		t.Fatalf("reclaim: count=%d err=%v", reclaimed, err)
	}
	if response, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1}); err != nil || response.HasJob {
		t.Fatalf("completed agent was redispatched: response=%v err=%v", response, err)
	}
	saved, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil {
		t.Fatal(err)
	}
	inflight, err := q.LookupInflight(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != models.RunStatusSuccess || saved.Error != "" || thread.Values["answer"] != "agent succeeded" || thread.Status != models.ThreadStatusIdle || inflight != nil {
		t.Fatalf("outcome not recovered: run=%+v thread=%+v inflight=%+v", saved, thread, inflight)
	}
}
func TestDurableCompletionIgnoresSupersededValues(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	rpc := bridge.NewServer(q, b, c, nil)
	rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
	ctx := context.Background()
	rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
	q.ReclaimStale(ctx, -time.Second, 3)
	rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
	stream := &completionStream{msgs: []*pb.RunEventProto{completionEvent(2, 1, "values", `{"answer":"current"}`), completionEvent(1, 50, "values", `{"answer":"stale"}`), completionEvent(2, 2, "end", `{"status":"success"}`)}}
	if err := rpc.StreamEvents(stream); err != nil {
		t.Fatal(err)
	}
	if resp, err := rpc.ReportStatus(ctx, &pb.ReportStatusRequest{RunId: "run", Generation: 2, Status: "success"}); err != nil || resp.Superseded {
		t.Fatal(resp, err)
	}
	thread, _ := store.GetThread(ctx, "thread")
	if thread.Values["answer"] != "current" {
		t.Fatal(thread.Values)
	}
	events, _ := b.Replay(ctx, "run", 0)
	for _, e := range events {
		if e.Generation != 2 {
			t.Fatalf("stale event persisted: %+v", e)
		}
	}
}

func TestDurableCompletionDoesNotUseUnversionedValuesForCurrentAttempt(t *testing.T) {
	for _, currentValues := range []bool{false, true} {
		t.Run(fmt.Sprintf("current_values_%t", currentValues), func(t *testing.T) {
			store, q, b, c := completionFixture(t)
			plane := api.NewServer(store, q, b, c)
			ctx := context.Background()
			if _, err := plane.BeginRunAttempt(ctx, "run", 2); err != nil {
				t.Fatal(err)
			}
			if currentValues {
				if err := b.Publish(ctx, "run", &transport.RunEvent{Generation: 2, Seq: 1, Method: "values", Data: json.RawMessage(`{"answer":"current"}`)}); err != nil {
					t.Fatal(err)
				}
			}
			// Defend durable output even against legacy events already in replay.
			if err := b.Publish(ctx, "run", &transport.RunEvent{Generation: 0, Seq: 50, Method: "values", Data: json.RawMessage(`{"answer":"stale"}`)}); err != nil {
				t.Fatal(err)
			}
			if ok, err := plane.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Generation: 2, Status: models.RunStatusSuccess}); err != nil || !ok {
				t.Fatalf("completion accepted=%v err=%v", ok, err)
			}
			saved, err := store.GetRun(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			if currentValues {
				var output map[string]interface{}
				if err := json.Unmarshal(saved.Output, &output); err != nil || output["answer"] != "current" {
					t.Fatalf("output=%s err=%v", saved.Output, err)
				}
			} else if len(saved.Output) != 0 {
				t.Fatalf("unversioned output persisted: %s", saved.Output)
			}
		})
	}
}

type failedCompletionBroker struct{ transport.EventBroker }

func (b failedCompletionBroker) Publish(context.Context, string, *transport.RunEvent) error {
	return errors.New("event persistence outage")
}
func TestDurableCompletionEventFailureIsNotAcknowledged(t *testing.T) {
	store, q, b, c := completionFixture(t)
	broken := failedCompletionBroker{b}
	plane := api.NewServer(store, q, broken, c)
	rpc := bridge.NewServer(q, broken, c, nil)
	rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
	ctx := context.Background()
	rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
	stream := &completionStream{msgs: []*pb.RunEventProto{completionEvent(1, 1, "values", `{"answer":"lost"}`)}}
	if err := rpc.StreamEvents(stream); status.Code(err) != codes.Unavailable || stream.response != nil {
		t.Fatalf("publish failure acknowledged: response=%v err=%v", stream.response, err)
	}
	resp, err := rpc.ReportStatus(ctx, &pb.ReportStatusRequest{RunId: "run", Generation: 1, Status: "error", ErrorMessage: "event delivery failed"})
	if err != nil || resp.Superseded {
		t.Fatal(resp, err)
	}
	saved, _ := store.GetRun(ctx, "run")
	if saved.Status != models.RunStatusError || saved.Error != "event delivery failed" {
		t.Fatal(saved)
	}
}
