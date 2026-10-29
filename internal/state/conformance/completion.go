package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func runCompletionTests(t *testing.T, factory StoreFactory) {
	seed := func(t *testing.T) (state.Store, context.Context) {
		s := factory(t)
		ctx := tenant.WithContext(context.Background(), "completion-tenant")
		now := time.Now().UTC()
		if err := s.CreateThread(ctx, &models.Thread{ThreadID: "completion-thread", Status: models.ThreadStatusBusy, Values: map[string]interface{}{"previous": "kept"}, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateRun(ctx, &models.Run{RunID: "completion-run", ThreadID: "completion-thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		return s, ctx
	}
	t.Run("fenced_atomic_idempotent_result", func(t *testing.T) {
		s, ctx := seed(t)
		if ok, err := s.BeginRunAttempt(ctx, "completion-run", 2); err != nil || !ok {
			t.Fatal(ok, err)
		}
		if ok, err := s.BeginRunAttempt(ctx, "completion-run", 1); err != nil || ok {
			t.Fatal("attempt moved backward", ok, err)
		}
		completion := &state.RunCompletion{RunID: "completion-run", Generation: 1, Status: models.RunStatusSuccess, Values: map[string]interface{}{"answer": "current"}}
		for _, stale := range []int64{0, 1} {
			completion.Generation = stale
			if result, err := s.CompleteRun(ctx, completion); err != nil || result.Accepted {
				t.Fatal("stale completion accepted", stale, result, err)
			}
		}
		before, err := s.GetRun(ctx, "completion-run")
		if err != nil || before.Status != models.RunStatusPending || len(before.Output) != 0 {
			t.Fatalf("rejected completion changed durable result: run=%+v err=%v", before, err)
		}
		completion.Generation = 2
		if result, err := s.CompleteRun(ctx, completion); err != nil || !result.Applied {
			t.Fatal(result, err)
		}
		completion.Values = map[string]interface{}{"answer": "duplicate must not replace"}
		if result, err := s.CompleteRun(ctx, completion); err != nil || !result.Accepted || result.Applied {
			t.Fatal("duplicate rewrote output", result, err)
		}
		run, err := s.GetRun(ctx, "completion-run")
		if err != nil {
			t.Fatal(err)
		}
		thread, err := s.GetThread(ctx, "completion-thread")
		if err != nil {
			t.Fatal(err)
		}
		history, err := s.ListCheckpoints(ctx, "completion-thread", 10, "")
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != models.RunStatusSuccess || thread.Status != models.ThreadStatusIdle || thread.Values["answer"] != "current" || thread.Values["previous"] != "kept" || len(history) != 1 {
			t.Fatalf("result not atomic/idempotent: run=%+v thread=%+v checkpoints=%d", run, thread, len(history))
		}
		if _, err := s.BeginRunAttempt(tenant.WithContext(ctx, "other-tenant"), "completion-run", 3); err == nil {
			t.Fatal("foreign tenant advanced attempt")
		}
		if ok, err := s.BeginRunAttempt(ctx, "completion-run", 3); err != nil || ok {
			t.Fatal("terminal run redispatched", ok, err)
		}
	})
	t.Run("legacy_attempt_matches_only_zero", func(t *testing.T) {
		s, ctx := seed(t)
		completion := &state.RunCompletion{RunID: "completion-run", Generation: 1, Status: models.RunStatusSuccess}
		if result, err := s.CompleteRun(ctx, completion); err != nil || result.Accepted {
			t.Fatal("versioned completion claimed unversioned attempt", result, err)
		}
		completion.Generation = 0
		if result, err := s.CompleteRun(ctx, completion); err != nil || !result.Applied {
			t.Fatal("actual legacy completion rejected", result, err)
		}
	})
	t.Run("trusted_cancel_targets_current_attempt_without_stale_values", func(t *testing.T) {
		s, ctx := seed(t)
		if ok, err := s.BeginRunAttempt(ctx, "completion-run", 2); err != nil || !ok {
			t.Fatal(ok, err)
		}
		result, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: "completion-run", Generation: 1, Status: models.RunStatusInterrupted, CancelCurrentAttempt: true, Values: map[string]interface{}{"answer": "stale"}})
		if err != nil || !result.Applied {
			t.Fatal(result, err)
		}
		run, err := s.GetRun(ctx, "completion-run")
		if err != nil {
			t.Fatal(err)
		}
		thread, err := s.GetThread(ctx, "completion-thread")
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != models.RunStatusInterrupted || state.MetadataGeneration(run.Metadata, state.CompletionGenerationKey) != 2 || thread.Values["answer"] != nil || thread.Status != models.ThreadStatusInterrupted {
			t.Fatalf("bad current-attempt cancellation: run=%+v thread=%+v", run, thread)
		}
	})
}
