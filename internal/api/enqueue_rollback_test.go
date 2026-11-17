package api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	dto "github.com/prometheus/client_model/go"
)

// A broker can accept publication before its acknowledgement is lost. Keep
// that boundary deterministic instead of relying on a network timeout race.
type enqueueReceiptErrorQueue struct {
	*inprocess.Queue
	afterAccept func(context.Context, *transport.RunAssignment)
	receiptErr  error
}

func (q *enqueueReceiptErrorQueue) Enqueue(ctx context.Context, assignment *transport.RunAssignment) error {
	if err := q.Queue.Enqueue(ctx, assignment); err != nil {
		return err
	}
	q.afterAccept(ctx, assignment)
	return q.receiptErr
}

func rollbackActiveRuns(t *testing.T) float64 {
	t.Helper()
	value := new(dto.Metric)
	if err := metrics.ActiveRuns.Write(value); err != nil {
		t.Fatal(err)
	}
	return value.GetGauge().GetValue()
}

func rollbackAcceptAttempt(t *testing.T, plane *Server, queue *inprocess.Queue, assignment *transport.RunAssignment) {
	t.Helper()
	ctx := context.Background()
	job, err := queue.Dequeue(ctx, assignment.RunnerKind, time.Second)
	if err != nil || job == nil || job.RunID != assignment.RunID || job.RunIncarnationID != assignment.RunIncarnationID || job.Generation != assignment.Generation {
		t.Fatalf("accepted assignment was not available: job=%+v err=%v", job, err)
	}
	if accepted, err := plane.BeginRunAttempt(ctx, assignment.RunID, assignment.RunIncarnationID, assignment.Generation); err != nil || !accepted {
		t.Fatalf("begin accepted assignment: accepted=%v err=%v", accepted, err)
	}
}

func rollbackCompleteAttempt(t *testing.T, plane *Server, assignment *transport.RunAssignment) {
	t.Helper()
	ctx := context.Background()
	if err := plane.broker.Publish(ctx, assignment.RunID, assignment.RunIncarnationID, withOriginalRunEvent(assignment.RunIncarnationID, &transport.RunEvent{
		Generation: assignment.Generation, Method: "values", Seq: 1,
		Data: json.RawMessage(`{"answer":"committed","usage":{"prompt_tokens":13,"completion_tokens":5,"model":"synthetic"}}`),
	})); err != nil {
		t.Fatal(err)
	}
	if accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{
		TrustedSystem: true, ExpectedIncarnationID: assignment.RunIncarnationID, RunID: assignment.RunID, Generation: assignment.Generation, Status: models.RunStatusSuccess,
	}); err != nil || !accepted {
		t.Fatalf("complete accepted assignment: accepted=%v err=%v", accepted, err)
	}
}

func TestEnqueueReceiptErrorPreservesCommittedResultAndFinalization(t *testing.T) {
	ctx := context.Background()
	receiptErr := errors.New("synthetic publication acknowledgement lost")
	queue := &enqueueReceiptErrorQueue{Queue: inprocess.NewQueue(), receiptErr: receiptErr}
	plane, store := newLifecycleServer(t, queue)
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "receipt-agent", Name: "receipt-agent"}); err != nil {
		t.Fatal(err)
	}
	sink := new(cacheHookSink)
	dispatcher := hooks.NewDispatcher()
	dispatcher.Register(sink, hooks.RunComplete, hooks.Error)
	plane.SetHookDispatcher(dispatcher)
	t.Cleanup(func() { _ = dispatcher.Close(ctx) })
	before := rollbackActiveRuns(t)
	var committed *models.Run
	queue.afterAccept = func(ctx context.Context, assignment *transport.RunAssignment) {
		rollbackAcceptAttempt(t, plane, queue.Queue, assignment)
		rollbackCompleteAttempt(t, plane, assignment)
		var err error
		committed, err = store.GetRun(ctx, assignment.RunID)
		if err != nil || len(committed.Output) == 0 {
			t.Fatalf("fixture did not commit output before the lost acknowledgement: %+v %v", committed, err)
		}
	}
	if _, err := plane.DispatchScheduledRun(ctx, "receipt", "receipt-agent", nil, nil); !errors.Is(err, receiptErr) {
		t.Fatalf("caller lost the enqueue error: %v", err)
	}
	current, err := store.GetRun(ctx, committed.RunID)
	if err != nil || current.Status != models.RunStatusSuccess || string(current.Output) != string(committed.Output) || current.Error != "" || !reflect.DeepEqual(current.Metadata, committed.Metadata) {
		t.Fatalf("enqueue error replaced a committed result: run=%+v err=%v", current, err)
	}
	if after := rollbackActiveRuns(t); after != before {
		t.Fatalf("terminal rollback decremented occupancy twice: before=%v after=%v", before, after)
	}
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	in, out, _, err := store.SumUsage(ctx, tenant.DefaultTenant, "receipt-agent", start, end)
	if err != nil || in != 13 || out != 5 {
		t.Fatalf("committed usage changed: in=%d out=%d err=%v", in, out, err)
	}
	// Closing drains the synchronous completion's queued hook before inspecting.
	if err := dispatcher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dispatcher = nil
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 || sink.events[0].Type != hooks.RunComplete {
		t.Fatalf("completion hook changed or repeated: %+v", sink.events)
	}
}

func TestEnqueueReceiptErrorDoesNotOverwriteRecreatedRun(t *testing.T) {
	ctx := context.Background()
	queue := &enqueueReceiptErrorQueue{Queue: inprocess.NewQueue(), receiptErr: errors.New("synthetic lost receipt")}
	plane, store := newLifecycleServer(t, queue)
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "receipt-agent", Name: "receipt-agent"}); err != nil {
		t.Fatal(err)
	}
	var replacement *models.Run
	queue.afterAccept = func(ctx context.Context, assignment *transport.RunAssignment) {
		rollbackAcceptAttempt(t, plane, queue.Queue, assignment)
		rollbackCompleteAttempt(t, plane, assignment)
		original, err := store.GetRun(ctx, assignment.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteRun(ctx, assignment.RunID); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if err := store.CreateThread(ctx, &models.Thread{ThreadID: "replacement-thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		replacement = &models.Run{RunID: assignment.RunID, ThreadID: "replacement-thread", AgentID: "receipt-agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{"replacement": true}, CreatedAt: original.CreatedAt, UpdatedAt: original.UpdatedAt}
		if err := store.CreateRun(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		if replacement.IncarnationID == "" || replacement.IncarnationID == original.IncarnationID {
			t.Fatal("fixture did not replace the original run incarnation")
		}
	}
	if _, err := plane.DispatchScheduledRun(ctx, "recreated", "receipt-agent", nil, nil); err == nil {
		t.Fatal("caller lost the enqueue error")
	}
	current, err := store.GetRun(ctx, replacement.RunID)
	if err != nil || current.Status != models.RunStatusPending || current.ThreadID != replacement.ThreadID || current.IncarnationID != replacement.IncarnationID || current.Metadata["replacement"] != true || current.Error != "" {
		t.Fatalf("old enqueue rollback mutated the replacement: run=%+v err=%v", current, err)
	}
	thread, err := store.GetThread(ctx, replacement.ThreadID)
	if err != nil || thread.Status != models.ThreadStatusBusy {
		t.Fatalf("replacement thread was released: thread=%+v err=%v", thread, err)
	}
}

func TestEnqueueReceiptErrorRetainsStartedAttemptUntilItsOwnCompletion(t *testing.T) {
	ctx := context.Background()
	queue := &enqueueReceiptErrorQueue{Queue: inprocess.NewQueue(), receiptErr: errors.New("synthetic lost receipt")}
	plane, store := newLifecycleServer(t, queue)
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "receipt-agent", Name: "receipt-agent"}); err != nil {
		t.Fatal(err)
	}
	var accepted *transport.RunAssignment
	queue.afterAccept = func(_ context.Context, assignment *transport.RunAssignment) {
		accepted = assignment
		rollbackAcceptAttempt(t, plane, queue.Queue, assignment)
	}
	if _, err := plane.DispatchScheduledRun(ctx, "running", "receipt-agent", nil, nil); err == nil {
		t.Fatal("caller lost the enqueue error")
	}
	current, err := store.GetRun(ctx, accepted.RunID)
	if err != nil || current.Status != models.RunStatusRunning || state.MetadataGeneration(current.Metadata, state.AttemptGenerationKey) != accepted.Generation {
		t.Fatalf("accepted worker was overwritten: run=%+v err=%v", current, err)
	}
	thread, err := store.GetThread(ctx, accepted.ThreadID)
	if err != nil || thread.Status != models.ThreadStatusBusy {
		t.Fatalf("accepted worker lost its thread: thread=%+v err=%v", thread, err)
	}
	if _, _, err := plane.createRunCtx(ctx, accepted.ThreadID, &models.RunCreate{AgentID: accepted.GraphID}); err == nil {
		t.Fatal("a second run could enter the accepted worker's thread")
	}
	rollbackCompleteAttempt(t, plane, accepted)
	current, err = store.GetRun(ctx, accepted.RunID)
	if err != nil || current.Status != models.RunStatusSuccess || len(current.Output) == 0 {
		t.Fatalf("accepted attempt could not finish normally: run=%+v err=%v", current, err)
	}
}

type rollbackReplayFailureBroker struct {
	transport.EventBroker
	replays int
}

func (b *rollbackReplayFailureBroker) Replay(context.Context, string, string, int64) ([]*transport.RunEvent, error) {
	b.replays++
	return nil, errors.New("synthetic broker replay outage")
}

type rollbackCompletionStore struct {
	state.Store
	before func(context.Context, *state.RunCompletion)
	fail   bool
}

func (s *rollbackCompletionStore) CompleteRun(ctx context.Context, completion *state.RunCompletion) (state.CompletionResult, error) {
	if s.fail {
		return state.CompletionResult{}, errors.New("synthetic completion write outage")
	}
	if s.before != nil {
		before := s.before
		s.before = nil
		before(ctx, completion)
	}
	return s.Store.CompleteRun(ctx, completion)
}

func TestEnqueueRollbackDefiniteRejectionRecoversWithoutBrokerReplay(t *testing.T) {
	ctx := tenant.ScopedContext(context.Background(), "rollback-tenant")
	plane, store := newLifecycleServer(t, failQueue{})
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "receipt-agent", Name: "receipt-agent"}); err != nil {
		t.Fatal(err)
	}
	broker := &rollbackReplayFailureBroker{EventBroker: plane.broker}
	plane.broker = broker
	sink := new(cacheHookSink)
	dispatcher := hooks.NewDispatcher()
	dispatcher.Register(sink, hooks.RunComplete, hooks.Error)
	plane.SetHookDispatcher(dispatcher)
	t.Cleanup(func() { _ = dispatcher.Close(context.Background()) })
	before := rollbackActiveRuns(t)
	run, _, err := plane.createRunCtx(ctx, "rejected-thread", &models.RunCreate{AgentID: "receipt-agent"})
	if err != nil {
		t.Fatal(err)
	}
	// A disconnected caller must not prevent the owned cleanup transaction.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if !plane.rollbackCreatedRun(cancelled, run) {
		t.Fatal("applied never-started rollback did not provide positive retry proof")
	}
	if plane.rollbackCreatedRun(cancelled, run) {
		t.Fatal("rollback replay supplied new retry proof without an applied transition")
	}
	current, err := store.GetRun(ctx, run.RunID)
	if err != nil || current.Status != models.RunStatusError || current.Error != "enqueue failed" {
		t.Fatalf("definite rejection was not settled: run=%+v err=%v", current, err)
	}
	if _, done := current.Metadata[state.CompletionGenerationKey]; !done || broker.replays != 0 {
		t.Fatal("enqueue rollback did not use conditional completion without replay")
	}
	thread, err := store.GetThread(ctx, run.ThreadID)
	if err != nil || thread.Status != models.ThreadStatusIdle {
		t.Fatalf("rejected thread was not released: thread=%+v err=%v", thread, err)
	}
	if after := rollbackActiveRuns(t); after != before {
		t.Fatalf("repeated cleanup changed occupancy: before=%v after=%v", before, after)
	}
	if err := dispatcher.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	dispatcher = nil
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 || sink.events[0].Type != hooks.Error {
		t.Fatalf("rejection hook was repeated or misclassified: %+v", sink.events)
	}
}

func TestEnqueueRollbackPersistenceFailureRetainsClaimUntilRetry(t *testing.T) {
	ctx := context.Background()
	plane, store := newLifecycleServer(t, failQueue{})
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "receipt-agent", Name: "receipt-agent"}); err != nil {
		t.Fatal(err)
	}
	run, _, err := plane.createRunCtx(ctx, "write-failure-thread", &models.RunCreate{AgentID: "receipt-agent"})
	if err != nil {
		t.Fatal(err)
	}
	fault := &rollbackCompletionStore{Store: store, fail: true}
	plane.store = fault
	before := rollbackActiveRuns(t)
	plane.rollbackCreatedRun(ctx, run)
	current, err := store.GetRun(ctx, run.RunID)
	if err != nil || current.Status != models.RunStatusPending {
		t.Fatalf("failed write changed the run: run=%+v err=%v", current, err)
	}
	thread, err := store.GetThread(ctx, run.ThreadID)
	if err != nil || thread.Status != models.ThreadStatusBusy || rollbackActiveRuns(t) != before {
		t.Fatal("failed rollback released its claim or occupancy")
	}
	fault.fail = false
	plane.rollbackCreatedRun(ctx, run)
	plane.rollbackCreatedRun(ctx, run)
	current, err = store.GetRun(ctx, run.RunID)
	if err != nil || current.Status != models.RunStatusError || rollbackActiveRuns(t) != before-1 {
		t.Fatalf("retry did not settle exactly once: run=%+v err=%v", current, err)
	}
}

func TestEnqueueRollbackRechecksStartedAttemptInsideTransaction(t *testing.T) {
	ctx := context.Background()
	plane, store := newLifecycleServer(t, failQueue{})
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "receipt-agent", Name: "receipt-agent"}); err != nil {
		t.Fatal(err)
	}
	run, _, err := plane.createRunCtx(ctx, "started-during-rollback", &models.RunCreate{AgentID: "receipt-agent"})
	if err != nil {
		t.Fatal(err)
	}
	plane.store = &rollbackCompletionStore{Store: store, before: func(ctx context.Context, completion *state.RunCompletion) {
		if accepted, err := store.BeginRunAttempt(ctx, completion.RunID, originalRunIdentity(t, store, completion.RunID), 1); err != nil || !accepted {
			t.Fatalf("worker admission race was not established: %v", err)
		}
	}}
	plane.rollbackCreatedRun(ctx, run)
	current, err := store.GetRun(ctx, run.RunID)
	if err != nil || current.Status != models.RunStatusRunning {
		t.Fatalf("transaction did not preserve the admitted worker: run=%+v err=%v", current, err)
	}
	thread, err := store.GetThread(ctx, run.ThreadID)
	if err != nil || thread.Status != models.ThreadStatusBusy {
		t.Fatal("transaction released the admitted worker's thread")
	}
}

func TestEnqueueRollbackRechecksReplacementInsideTransaction(t *testing.T) {
	ctx := context.Background()
	plane, store := newLifecycleServer(t, failQueue{})
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "receipt-agent", Name: "receipt-agent"}); err != nil {
		t.Fatal(err)
	}
	run, _, err := plane.createRunCtx(ctx, "replaced-during-rollback", &models.RunCreate{AgentID: "receipt-agent"})
	if err != nil {
		t.Fatal(err)
	}
	var replacement *models.Run
	plane.store = &rollbackCompletionStore{Store: store, before: func(ctx context.Context, completion *state.RunCompletion) {
		if err := store.DeleteRun(ctx, completion.RunID); err != nil {
			t.Fatal(err)
		}
		copy := *run
		copy.IncarnationID, copy.Metadata = "", map[string]interface{}{"replacement": true}
		replacement = &copy
		if err := store.CreateRun(ctx, replacement); err != nil {
			t.Fatal(err)
		}
	}}
	plane.rollbackCreatedRun(ctx, run)
	current, err := store.GetRun(ctx, run.RunID)
	if err != nil || current.Status != models.RunStatusPending || current.IncarnationID != replacement.IncarnationID || current.Metadata["replacement"] != true {
		t.Fatalf("transaction did not fence the replaced run: run=%+v err=%v", current, err)
	}
	thread, err := store.GetThread(ctx, run.ThreadID)
	if err != nil || thread.Status != models.ThreadStatusBusy {
		t.Fatal("transaction released the replacement's claim")
	}
}
