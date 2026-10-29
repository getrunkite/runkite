package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type poisonOrderingBroker struct {
	transport.EventBroker
	failReplay bool
	failClose  bool
	closeCalls int
}

func (b *poisonOrderingBroker) Replay(ctx context.Context, id string, since int64) ([]*transport.RunEvent, error) {
	if b.failReplay {
		return nil, errors.New("injected poison recovery replay outage")
	}
	return b.EventBroker.Replay(ctx, id, since)
}

func (b *poisonOrderingBroker) Close(id string) error {
	b.closeCalls++
	if b.failClose {
		return errors.New("injected postcommit poison finalization outage")
	}
	return b.EventBroker.Close(id)
}

func beginPoisonOrderingAttempt(t *testing.T, store *sqlite.SQLiteStore, q *inprocess.Queue) {
	t.Helper()
	ctx := context.Background()
	assignment, err := q.Dequeue(ctx, "kind", time.Second)
	if err != nil || assignment == nil || assignment.Generation != 1 {
		t.Fatal("dequeue", assignment, err)
	}
	if accepted, err := store.BeginRunAttempt(ctx, "run", 1); err != nil || !accepted {
		t.Fatal("begin attempt", accepted, err)
	}
}

func retainedPoisonOrderingPill(t *testing.T, q *inprocess.Queue) transport.PoisonPill {
	t.Helper()
	ctx := context.Background()
	n, dead, err := q.ReclaimStale(ctx, -time.Second, 1)
	if err != nil || n != 0 || len(dead) != 1 || dead[0].Generation != 2 {
		t.Fatal("poison selection", n, dead, err)
	}
	lease, err := q.LookupInflight(ctx, "run")
	if err != nil || lease == nil || lease.Generation != 1 {
		t.Fatal("poison selection must retain current lease until durable completion and Ack", lease, err)
	}
	return dead[0]
}

func publishPoisonOrderingSuccess(t *testing.T, b transport.EventBroker) {
	t.Helper()
	for _, event := range []*transport.RunEvent{
		{EventID: "final-values", Generation: 1, Seq: 1, Method: "values", Data: json.RawMessage(`{"answer":"original success"}`)},
		{EventID: "final-end", Generation: 1, Seq: 2, Method: "end", Data: json.RawMessage(`{"status":"success"}`)},
	} {
		if err := b.Publish(context.Background(), "run", event); err != nil {
			t.Fatal(err)
		}
	}
}

func assertPoisonOrderingSuccess(t *testing.T, store *sqlite.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	run, err := store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusSuccess || run.Error != "" {
		t.Fatal("original agent outcome not preserved", run, err)
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil || thread.Values["answer"] != "original success" || thread.Status != models.ThreadStatusIdle {
		t.Fatal("final output/thread not persisted", thread, err)
	}
	checkpoints, err := store.ListCheckpoints(ctx, "thread", 10, "")
	if err != nil || len(checkpoints) != 1 {
		t.Fatal("completion must be idempotent", len(checkpoints), err)
	}
}

func TestPoisonCompletionDatabaseOutageRetainsLeaseForNextTick(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &completionFaultStore{Store: store, fail: true}
	plane := api.NewServer(fault, q, b, c)
	beginPoisonOrderingAttempt(t, store, q)
	pill := retainedPoisonOrderingPill(t, q)
	ctx := context.Background()
	if _, err := plane.CompleteReclaimedRun(ctx, pill, "retry ceiling reached"); err == nil {
		t.Fatal("expected retryable database error")
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusPending {
		t.Fatal("failed durable completion changed the outcome", run, err)
	}
	if _, completed := run.Metadata[state.CompletionGenerationKey]; completed {
		t.Fatal("failed durable completion recorded a completion marker")
	}
	fault.fail = false
	repeated := retainedPoisonOrderingPill(t, q)
	if accepted, err := plane.CompleteReclaimedRun(ctx, repeated, "retry ceiling reached"); err != nil || !accepted {
		t.Fatal("next tick must retry durable poison completion", accepted, err)
	}
	if lease, err := q.LookupInflight(ctx, "run"); err != nil || lease == nil {
		t.Fatal("API must leave Ack to the reaper", lease, err)
	}
	if acknowledged, err := q.Ack(ctx, "run", repeated.Generation-1); err != nil || !acknowledged {
		t.Fatal("Ack after durable completion", acknowledged, err)
	}
	run, err = store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusError || run.Error != "retry ceiling reached" {
		t.Fatal("poison fallback not persisted", run, err)
	}
}

func TestPoisonCompletionBrokerOutageCannotInventFailedAgentOutcome(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &poisonOrderingBroker{EventBroker: b, failReplay: true}
	plane := api.NewServer(store, q, fault, c)
	beginPoisonOrderingAttempt(t, store, q)
	publishPoisonOrderingSuccess(t, b)
	pill := retainedPoisonOrderingPill(t, q)
	ctx := context.Background()
	if _, err := plane.CompleteReclaimedRun(ctx, pill, "retry ceiling reached"); err == nil {
		t.Fatal("replay outage must be retried, not treated as missing terminal history")
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusPending {
		t.Fatal("broker outage replaced actual success with fallback error", run, err)
	}
	fault.failReplay = false
	repeated := retainedPoisonOrderingPill(t, q)
	if accepted, err := plane.CompleteReclaimedRun(ctx, repeated, "retry ceiling reached"); err != nil || !accepted {
		t.Fatal("retry after broker recovery", accepted, err)
	}
	assertPoisonOrderingSuccess(t, store)
}

func TestPoisonCompletionPostcommitFailureRetriesFinalizationBeforeAck(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &poisonOrderingBroker{EventBroker: b, failClose: true}
	plane := api.NewServer(store, q, fault, c)
	beginPoisonOrderingAttempt(t, store, q)
	publishPoisonOrderingSuccess(t, b)
	pill := retainedPoisonOrderingPill(t, q)
	ctx := context.Background()
	if _, err := plane.CompleteReclaimedRun(ctx, pill, "retry ceiling reached"); err == nil {
		t.Fatal("expected finalization failure after durable completion")
	}
	assertPoisonOrderingSuccess(t, store)
	// Once the durable result exists, a history outage must not prevent
	// retrying finalization or cause the fallback poison status to replace it.
	fault.failClose = false
	fault.failReplay = true
	repeated := retainedPoisonOrderingPill(t, q)
	if accepted, err := plane.CompleteReclaimedRun(ctx, repeated, "retry ceiling reached"); err != nil || !accepted {
		t.Fatal("committed result must retry finalization without replay", accepted, err)
	}
	if fault.closeCalls != 2 {
		t.Fatalf("finalization calls=%d, want failed call plus successful retry", fault.closeCalls)
	}
	assertPoisonOrderingSuccess(t, store)
	if acknowledged, err := q.Ack(ctx, "run", repeated.Generation-1); err != nil || !acknowledged {
		t.Fatal("finalized result must be acknowledged", acknowledged, err)
	}
}

func TestPoisonCompletionLostAckRepeatsSamePillWithoutDuplicatingResult(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	beginPoisonOrderingAttempt(t, store, q)
	publishPoisonOrderingSuccess(t, b)
	ctx := context.Background()
	for attempt := 0; attempt < 2; attempt++ {
		pill := retainedPoisonOrderingPill(t, q)
		if accepted, err := plane.CompleteReclaimedRun(ctx, pill, "retry ceiling reached"); err != nil || !accepted {
			t.Fatal("repeat poison completion without runner ReportStatus or queue Ack", attempt, accepted, err)
		}
		assertPoisonOrderingSuccess(t, store)
	}
	if acknowledged, err := q.Ack(ctx, "run", 0); err != nil || acknowledged {
		t.Fatal("stale generation must not acknowledge retained lease", acknowledged, err)
	}
	if acknowledged, err := q.Ack(ctx, "run", 1); err != nil || !acknowledged {
		t.Fatal("current generation must acknowledge finalized lease", acknowledged, err)
	}
	if lease, err := q.LookupInflight(ctx, "run"); err != nil || lease != nil {
		t.Fatal("Ack must remove the finalized lease", lease, err)
	}
}

func TestPoisonCompletionBindsOrphanedClaimBeforeFallback(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	ctx := context.Background()
	assignment, err := q.Dequeue(ctx, "kind", time.Second)
	if err != nil || assignment == nil || assignment.Generation != 1 {
		t.Fatal("dequeue before crashed GetJob", assignment, err)
	}
	before, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if _, attempted := before.Metadata[state.AttemptGenerationKey]; attempted {
		t.Fatal("fixture unexpectedly recorded an attempt before GetJob crashed")
	}
	pill := retainedPoisonOrderingPill(t, q)
	if accepted, err := plane.CompleteReclaimedRun(ctx, pill, "orphaned claim reached retry ceiling"); err != nil || !accepted {
		t.Fatal("trusted reaper must bind retained orphaned dispatch before finishing", accepted, err)
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusError || run.Error != "orphaned claim reached retry ceiling" {
		t.Fatal("orphaned claim not durably failed", run, err)
	}
	if state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey) != 1 || state.MetadataGeneration(run.Metadata, state.CompletionGenerationKey) != 1 {
		t.Fatal("completion did not bind the actual retained generation", run.Metadata)
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil || thread.Status != models.ThreadStatusIdle {
		t.Fatal("orphaned completion did not release the thread", thread, err)
	}
	if lease, err := q.LookupInflight(ctx, "run"); err != nil || lease == nil || lease.Generation != 1 {
		t.Fatal("completion removed orphaned claim before Ack", lease, err)
	}
	if acknowledged, err := q.Ack(ctx, "run", pill.Generation-1); err != nil || !acknowledged {
		t.Fatal("Ack orphaned claim after durable completion", acknowledged, err)
	}
}

func TestPoisonCompletionRejectsPillForSupersededQueueGeneration(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	beginPoisonOrderingAttempt(t, store, q)
	stalePill := retainedPoisonOrderingPill(t, q)
	ctx := context.Background()
	// An operator may raise the retry ceiling while a reaper still holds a
	// previously selected pill. That pill must not complete the new attempt.
	if n, dead, err := q.ReclaimStale(ctx, -time.Second, 0); err != nil || n != 1 || len(dead) != 0 {
		t.Fatal("reclaim after retry ceiling changes", n, dead, err)
	}
	assignment, err := q.Dequeue(ctx, "kind", time.Second)
	if err != nil || assignment == nil || assignment.Generation != 2 {
		t.Fatal("new dispatch", assignment, err)
	}
	if accepted, err := store.BeginRunAttempt(ctx, "run", 2); err != nil || !accepted {
		t.Fatal("begin newer attempt", accepted, err)
	}
	if accepted, err := plane.CompleteReclaimedRun(ctx, stalePill, "stale poison error"); err != nil || accepted {
		t.Fatal("old pill must reject a different current queue generation", accepted, err)
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusPending || run.Error != "" || state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey) != 2 {
		t.Fatal("stale pill mutated the newer attempt", run, err)
	}
	if _, completed := run.Metadata[state.CompletionGenerationKey]; completed {
		t.Fatal("stale pill recorded a completion marker")
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil || thread.Status != models.ThreadStatusBusy {
		t.Fatal("stale pill released the active thread", thread, err)
	}
	if lease, err := q.LookupInflight(ctx, "run"); err != nil || lease == nil || lease.Generation != 2 {
		t.Fatal("stale pill changed the newer queue lease", lease, err)
	}
}
