package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/bridge"
	pb "github.com/getrunkite/runkite/internal/bridge/runnerpb"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type closeFailureReviewStream struct{ *completionStream }

func (s *closeFailureReviewStream) SendAndClose(*pb.StreamEventsResponse) error {
	return errors.New("injected lost stream-close acknowledgement")
}

func recoveryReviewFixture(t *testing.T) (*api.Server, *bridge.Server, func(int) (int, []transport.PoisonPill, error), func() *models.Run) {
	t.Helper()
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	rpc := bridge.NewServer(q, b, c, nil)
	rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
	ctx := context.Background()
	response, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
	if err != nil || !response.HasJob {
		t.Fatal(response, err)
	}
	stream := &closeFailureReviewStream{&completionStream{msgs: []*pb.RunEventProto{completionEvent(1, 1, "values", `{"answer":"succeeded"}`), completionEvent(1, 2, "end", `{"status":"success"}`)}}}
	if err := rpc.StreamEvents(stream); err == nil {
		t.Fatal("expected lost stream-close acknowledgment")
	}
	inflight, err := q.LookupInflight(ctx, "run")
	if err != nil || inflight == nil {
		t.Fatal("stream-close failure lost recoverable assignment", inflight, err)
	}
	return plane, rpc, func(max int) (int, []transport.PoisonPill, error) { return q.ReclaimStale(ctx, -time.Second, max) }, func() *models.Run {
		run, err := store.GetRun(ctx, "run")
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
}

func TestPoisonPillRecoversPersistedSuccessAfterLostStreamAcknowledgement(t *testing.T) {
	plane, _, reclaim, read := recoveryReviewFixture(t)
	n, dead, err := reclaim(1)
	if err != nil || n != 0 || len(dead) != 1 {
		t.Fatal(n, dead, err)
	}
	// This is the completion callback used by cmd/reclaim.go before Ack.
	if accepted, err := plane.CompleteReclaimedRun(context.Background(), dead[0], "max retries exceeded"); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	run := read()
	if run.Status != models.RunStatusSuccess {
		t.Fatalf("durable success became %s at retry ceiling: output=%s error=%s", run.Status, run.Output, run.Error)
	}
}

func TestReclaimRecoversActualLegacyGenerationZeroTerminal(t *testing.T) {
	store, _, b, c := completionFixture(t)
	q := inprocess.NewQueue()
	ctx := context.Background()
	if err := q.Enqueue(ctx, &transport.RunAssignment{RunID: "run", ThreadID: "thread", RunnerKind: "kind", GraphID: "agent", Generation: 0}); err != nil {
		t.Fatal(err)
	}
	plane := api.NewServer(store, q, b, c)
	rpc := bridge.NewServer(q, b, c, nil)
	rpc.SetRunLifecycle(plane.BeginRunAttempt, plane.CompleteRun)
	first, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
	if err != nil || !first.HasJob {
		t.Fatal(first, err)
	}
	stream := &closeFailureReviewStream{&completionStream{msgs: []*pb.RunEventProto{completionEvent(0, 1, "values", `{"answer":"legacy succeeded"}`), completionEvent(0, 2, "end", `{"status":"success"}`)}}}
	if err := rpc.StreamEvents(stream); err == nil {
		t.Fatal("expected stream-close failure")
	}
	if n, dead, err := q.ReclaimStale(ctx, -time.Second, 1); err != nil || n != 1 || len(dead) != 0 {
		t.Fatal(n, dead, err)
	}
	second, err := rpc.GetJob(ctx, &pb.GetJobRequest{RunnerKind: "kind", TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if second.HasJob || run.Status != models.RunStatusSuccess {
		t.Fatalf("legacy success was redispatched: assignment=%+v status=%s", second, run.Status)
	}
	var output map[string]interface{}
	if err := json.Unmarshal(run.Output, &output); err != nil || output["answer"] != "legacy succeeded" {
		t.Fatalf("legacy output=%s err=%v", run.Output, err)
	}
}

func TestStrayUnversionedTerminalCannotCompleteAnUndispatchedRun(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	ctx := context.Background()
	if err := b.Publish(ctx, "run", &transport.RunEvent{Generation: 0, Seq: 1, Method: "end", Data: json.RawMessage(`{"status":"success"}`)}); err != nil {
		t.Fatal(err)
	}
	accepted, err := plane.BeginRunAttempt(ctx, "run", 1)
	if err != nil || !accepted {
		t.Fatalf("unversioned stray history blocked first dispatch: accepted=%t err=%v", accepted, err)
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.RunStatusPending {
		t.Fatalf("stray history completed undispatched run: %+v", run)
	}
}

func TestPoisonPillDoesNotRecoverUnconfirmedOrWrongAttemptOutcome(t *testing.T) {
	for _, test := range []struct {
		name     string
		current  int64
		terminal *transport.RunEvent
	}{
		{name: "no_terminal", current: 1},
		{name: "stale_terminal", current: 2, terminal: &transport.RunEvent{Generation: 1, Seq: 2, Method: "end", Data: json.RawMessage(`{"status":"success"}`)}},
		{name: "zero_terminal_cannot_match_positive_attempt", current: 1, terminal: &transport.RunEvent{Generation: 0, Seq: 2, Method: "end", Data: json.RawMessage(`{"status":"success"}`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, q, b, c := completionFixture(t)
			plane := api.NewServer(store, q, b, c)
			ctx := context.Background()
			if test.current == 2 {
				if _, err := q.Dequeue(ctx, "kind", time.Second); err != nil {
					t.Fatal(err)
				}
				if n, dead, err := q.ReclaimStale(ctx, -time.Second, 0); err != nil || n != 1 || len(dead) != 0 {
					t.Fatal(n, dead, err)
				}
			}
			assignment, err := q.Dequeue(ctx, "kind", time.Second)
			if err != nil || assignment == nil || assignment.Generation != test.current {
				t.Fatal(assignment, err)
			}
			if ok, err := store.BeginRunAttempt(ctx, "run", test.current); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if err := b.Publish(ctx, "run", &transport.RunEvent{Generation: test.current, Seq: 1, Method: "values", Data: json.RawMessage(`{"answer":"partial"}`)}); err != nil {
				t.Fatal(err)
			}
			if test.terminal != nil {
				if err := b.Publish(ctx, "run", test.terminal); err != nil {
					t.Fatal(err)
				}
			}
			n, dead, err := q.ReclaimStale(ctx, -time.Second, int(test.current))
			if err != nil || n != 0 || len(dead) != 1 {
				t.Fatal(n, dead, err)
			}
			if accepted, err := plane.CompleteReclaimedRun(ctx, dead[0], "max retries exceeded"); err != nil || !accepted {
				t.Fatal(accepted, err)
			}
			run, err := store.GetRun(ctx, "run")
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != models.RunStatusError || run.Error != "max retries exceeded" {
				t.Fatalf("wrong attempt recovered: run=%+v", run)
			}
		})
	}
}
