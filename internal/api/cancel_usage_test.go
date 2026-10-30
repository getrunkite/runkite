package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport"
)

func TestCancelRetainsDeliveredCurrentUsageWithoutUnfinishedHistory(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	ctx := context.Background()
	if err := store.SaveCheckpoint(ctx, "thread", &models.ThreadState{
		Values:     map[string]interface{}{"messages": []interface{}{map[string]interface{}{"role": "assistant", "content": "previous answer"}}},
		Checkpoint: models.ThreadCheckpoint{ThreadID: "thread", CheckpointID: "previous"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginRunAttempt(ctx, "run", 2); err != nil {
		t.Fatal(err)
	}
	publishReviewEvent(t, b, 1, 1, "values", `{"usage":{"prompt_tokens":999}}`)
	publishReviewEvent(t, b, 2, 1, "values", `{"messages":[{"role":"assistant","content":"unfinished"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`)
	// A later progress value without usage must not erase an observed charge.
	publishReviewEvent(t, b, 2, 2, "values", `{"progress":"waiting for provider"}`)
	publishReviewEvent(t, b, 1, 2, "values", `{"usage":{"prompt_tokens":1000}}`)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		plane.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run/cancel", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("cancel=%d %s", rec.Code, rec.Body.String())
		}
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]interface{}
	if err := json.Unmarshal(run.Output, &output); err != nil {
		t.Fatal(err)
	}
	usage, ok := output["usage"].(map[string]interface{})
	if !ok || usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(4) || len(output) != 1 {
		t.Fatalf("cancelled output=%s", run.Output)
	}
	tin, tout, _, err := store.SumUsage(ctx, "default", "agent", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || tin != 10 || tout != 4 {
		t.Fatalf("cancelled usage counted incorrectly: in=%d out=%d err=%v", tin, tout, err)
	}
	latest, err := store.GetLatestCheckpoint(ctx, "thread")
	if err != nil || latest.Checkpoint.CheckpointID != "previous" || latest.Values["usage"] != nil {
		t.Fatalf("cancel changed the latest conversation checkpoint: latest=%+v err=%v", latest, err)
	}
}

type unavailableCancelReplayBroker struct{ transport.EventBroker }

func (b unavailableCancelReplayBroker) Replay(context.Context, string, int64) ([]*transport.RunEvent, error) {
	return nil, errors.New("synthetic broker outage")
}

func TestCancelStillCommitsWhenUsageReplayIsUnavailable(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, unavailableCancelReplayBroker{b}, c)
	ctx := context.Background()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	plane.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/runs/run/cancel", nil))
	run, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || run.Status != models.RunStatusInterrupted {
		t.Fatalf("broker outage blocked cancel: response=%d %s run=%+v", rec.Code, rec.Body.String(), run)
	}
}

type stalledCancelReplayBroker struct{ transport.EventBroker }

func (b stalledCancelReplayBroker) Replay(ctx context.Context, _ string, _ int64) ([]*transport.RunEvent, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type uncooperativeCancelReplayBroker struct {
	transport.EventBroker
	release chan struct{}
	done    chan struct{}
}

func (b uncooperativeCancelReplayBroker) Replay(context.Context, string, int64) ([]*transport.RunEvent, error) {
	defer close(b.done)
	<-b.release
	return nil, errors.New("synthetic socket timeout")
}

func TestCancelCommitsBeforeAnUncooperativeBrokerReadReturns(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := uncooperativeCancelReplayBroker{EventBroker: b, release: make(chan struct{}), done: make(chan struct{})}
	defer func() {
		close(fault.release)
		select {
		case <-fault.done:
		case <-time.After(2 * time.Second):
			t.Error("test broker did not finish after release")
		}
	}()
	plane := api.NewServer(store, q, fault, c)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Status: models.RunStatusInterrupted, CancelCurrentAttempt: true})
	if err != nil || !accepted {
		t.Fatalf("optional socket read blocked cancellation: accepted=%v err=%v", accepted, err)
	}
	select {
	case <-fault.done:
		t.Fatal("test broker returned before cancellation committed")
	default:
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil || run.Status != models.RunStatusInterrupted {
		t.Fatalf("cancellation did not commit independently of replay: run=%+v err=%v", run, err)
	}
}

func TestCancelDoesNotSpendItsWholeDeadlineOnOptionalUsageReplay(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, stalledCancelReplayBroker{b}, c)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Status: models.RunStatusInterrupted, CancelCurrentAttempt: true})
	if err != nil || !accepted {
		t.Fatalf("optional replay blocked cancellation: accepted=%v err=%v", accepted, err)
	}
	if ctx.Err() != nil {
		t.Fatal("optional replay exhausted the cancellation deadline")
	}
}

type pausedCancelCommitStore struct {
	*sqlite.SQLiteStore
	paused atomic.Bool
	ready  chan struct{}
	resume chan struct{}
}

func (s *pausedCancelCommitStore) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	if c.CancelCurrentAttempt && s.paused.CompareAndSwap(false, true) {
		close(s.ready)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return state.CompletionResult{}, ctx.Err()
		}
	}
	return s.SQLiteStore.CompleteRun(ctx, c)
}

func TestCancelDropsUsageWhenReclaimRacesCapturedSnapshot(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &pausedCancelCommitStore{SQLiteStore: store, ready: make(chan struct{}), resume: make(chan struct{})}
	plane := api.NewServer(fault, q, b, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	publishReviewEvent(t, b, 1, 1, "values", `{"usage":{"prompt_tokens":999}}`)
	done := make(chan error, 1)
	go func() {
		_, err := plane.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Status: models.RunStatusInterrupted, CancelCurrentAttempt: true})
		done <- err
	}()
	select {
	case <-fault.ready:
	case <-ctx.Done():
		t.Fatal("cancellation did not capture usage")
	}
	if _, err := store.BeginRunAttempt(ctx, "run", 2); err != nil {
		close(fault.resume)
		t.Fatal(err)
	}
	close(fault.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.RunStatusInterrupted || state.MetadataGeneration(run.Metadata, state.CompletionGenerationKey) != 2 || len(run.Output) != 0 {
		t.Fatalf("stale usage borrowed the replacement attempt: run=%+v", run)
	}
}
