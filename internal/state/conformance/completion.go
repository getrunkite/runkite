package conformance

import (
	"context"
	"encoding/json"
	"fmt"
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
		if err != nil || before.Status != models.RunStatusRunning || len(before.Output) != 0 {
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
		if history[0].Values["previous"] != "kept" || history[0].Values["answer"] != "current" {
			t.Fatalf("completion checkpoint is a partial patch instead of a state snapshot: %#v", history[0].Values)
		}
		if _, err := s.BeginRunAttempt(tenant.WithContext(ctx, "other-tenant"), "completion-run", 3); err == nil {
			t.Fatal("foreign tenant advanced attempt")
		}
		if ok, err := s.BeginRunAttempt(ctx, "completion-run", 3); err != nil || ok {
			t.Fatal("terminal run redispatched", ok, err)
		}
	})
	t.Run("partial_completion_snapshots_accumulated_thread_values", func(t *testing.T) {
		s, ctx := seed(t)
		for i, values := range []map[string]interface{}{{"first": "one"}, {"second": "two"}} {
			runID := "completion-run"
			if i > 0 {
				runID += "-next"
				if err := s.SetThreadStatus(ctx, "completion-thread", models.ThreadStatusBusy); err != nil {
					t.Fatal(err)
				}
				if err := s.CreateRun(ctx, &models.Run{RunID: runID, ThreadID: "completion-thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			}
			if ok, err := s.BeginRunAttempt(ctx, runID, 1); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if result, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: runID, Generation: 1, Status: models.RunStatusSuccess, Values: values}); err != nil || !result.Applied {
				t.Fatal(result, err)
			}
		}
		thread, err := s.GetThread(ctx, "completion-thread")
		if err != nil {
			t.Fatal(err)
		}
		latest, err := s.GetLatestCheckpoint(ctx, "completion-thread")
		if err != nil || latest.Values["previous"] != "kept" || latest.Values["first"] != "one" || latest.Values["second"] != "two" {
			t.Fatalf("latest state lost previous keys: state=%+v err=%v", latest, err)
		}
		if len(latest.Values) != len(thread.Values) {
			t.Fatalf("checkpoint differs from durable thread: checkpoint=%#v thread=%#v", latest.Values, thread.Values)
		}
		history, err := s.ListCheckpoints(ctx, "completion-thread", 10, "")
		if err != nil || len(history) != 2 || history[1].Values["previous"] != "kept" || history[1].Values["first"] != "one" || history[1].Values["second"] != nil {
			t.Fatalf("history is not immutable state snapshots: states=%+v err=%v", history, err)
		}
		run, err := s.GetRun(ctx, "completion-run-next")
		if err != nil {
			t.Fatal(err)
		}
		var output map[string]interface{}
		if err := json.Unmarshal(run.Output, &output); err != nil || len(output) != 1 || output["second"] != "two" {
			t.Fatalf("run output must remain per-run result: output=%s err=%v", run.Output, err)
		}
	})
	t.Run("dispatch_marks_running_and_fences_reclaim", func(t *testing.T) {
		for _, firstGeneration := range []int64{0, 1} {
			t.Run(fmt.Sprintf("generation_%d", firstGeneration), func(t *testing.T) {
				s := factory(t)
				ctx := tenant.WithContext(context.Background(), "dispatch-tenant")
				old := time.Now().UTC().Add(-time.Hour)
				if err := s.CreateThread(ctx, &models.Thread{ThreadID: "dispatch-thread", Status: models.ThreadStatusBusy, CreatedAt: old, UpdatedAt: old}); err != nil {
					t.Fatal(err)
				}
				if err := s.CreateRun(ctx, &models.Run{RunID: "dispatch-run", ThreadID: "dispatch-thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{"preserved": "metadata"}, CreatedAt: old, UpdatedAt: old}); err != nil {
					t.Fatal(err)
				}
				if ok, err := s.BeginRunAttempt(ctx, "dispatch-run", firstGeneration); err != nil || !ok {
					t.Fatal("first dispatch", ok, err)
				}
				run, err := s.GetRun(ctx, "dispatch-run")
				if err != nil || run.Status != models.RunStatusRunning || !run.UpdatedAt.After(old) || state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey) != firstGeneration || run.Metadata["preserved"] != "metadata" {
					t.Fatalf("dispatch did not atomically start run: run=%+v err=%v", run, err)
				}
				if ok, err := s.BeginRunAttempt(ctx, "dispatch-run", 2); err != nil || !ok {
					t.Fatal("reclaimed dispatch", ok, err)
				}
				reclaimed, err := s.GetRun(ctx, "dispatch-run")
				if err != nil || reclaimed.Status != models.RunStatusRunning || reclaimed.UpdatedAt.Before(run.UpdatedAt) || state.MetadataGeneration(reclaimed.Metadata, state.AttemptGenerationKey) != 2 {
					t.Fatalf("reclaim did not remain running: run=%+v err=%v", reclaimed, err)
				}
				if ok, err := s.BeginRunAttempt(ctx, "dispatch-run", 1); err != nil || ok {
					t.Fatal("stale dispatch accepted", ok, err)
				}
				if _, err := s.BeginRunAttempt(tenant.WithContext(ctx, "other-tenant"), "dispatch-run", 3); err == nil {
					t.Fatal("foreign tenant dispatched run")
				}
				afterRejected, err := s.GetRun(ctx, "dispatch-run")
				if err != nil || afterRejected.Status != models.RunStatusRunning || !afterRejected.UpdatedAt.Equal(reclaimed.UpdatedAt) || state.MetadataGeneration(afterRejected.Metadata, state.AttemptGenerationKey) != 2 {
					t.Fatalf("rejected dispatch changed active run: run=%+v err=%v", afterRejected, err)
				}
				if result, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: "dispatch-run", Generation: 2, Status: models.RunStatusSuccess}); err != nil || !result.Applied {
					t.Fatal("complete dispatched run", result, err)
				}
				terminal, err := s.GetRun(ctx, "dispatch-run")
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := s.BeginRunAttempt(ctx, "dispatch-run", 3); err != nil || ok {
					t.Fatal("terminal run redispatched", ok, err)
				}
				afterTerminal, err := s.GetRun(ctx, "dispatch-run")
				if err != nil || afterTerminal.Status != models.RunStatusSuccess || !afterTerminal.UpdatedAt.Equal(terminal.UpdatedAt) || state.MetadataGeneration(afterTerminal.Metadata, state.AttemptGenerationKey) != 2 {
					t.Fatalf("rejected dispatch reopened terminal run: run=%+v err=%v", afterTerminal, err)
				}
			})
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
		result, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: "completion-run", Generation: 1, Status: models.RunStatusInterrupted, CancelCurrentAttempt: true, Values: map[string]interface{}{"answer": "stale"}, CancelUsage: map[string]interface{}{"prompt_tokens": 99}})
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
		if run.Status != models.RunStatusInterrupted || state.MetadataGeneration(run.Metadata, state.CompletionGenerationKey) != 2 || thread.Values["answer"] != nil || thread.Status != models.ThreadStatusInterrupted || len(run.Output) != 0 {
			t.Fatalf("bad current-attempt cancellation: run=%+v thread=%+v", run, thread)
		}
	})
	t.Run("trusted_cancel_retains_only_current_usage_once", func(t *testing.T) {
		s, ctx := seed(t)
		if ok, err := s.BeginRunAttempt(ctx, "completion-run", 2); err != nil || !ok {
			t.Fatal(ok, err)
		}
		completion := &state.RunCompletion{
			RunID: "completion-run", Generation: 2, Status: models.RunStatusInterrupted,
			CancelCurrentAttempt: true, Values: map[string]interface{}{"answer": "unfinished"},
			CancelUsage: map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 4},
		}
		if result, err := s.CompleteRun(ctx, completion); err != nil || !result.Applied {
			t.Fatal(result, err)
		}
		completion.CancelUsage = map[string]interface{}{"prompt_tokens": 999}
		if result, err := s.CompleteRun(ctx, completion); err != nil || !result.Accepted || result.Applied {
			t.Fatal("duplicate cancellation changed usage", result, err)
		}
		run, err := s.GetRun(ctx, "completion-run")
		if err != nil {
			t.Fatal(err)
		}
		var output map[string]interface{}
		if err := json.Unmarshal(run.Output, &output); err != nil {
			t.Fatal(err)
		}
		usage, ok := output["usage"].(map[string]interface{})
		if !ok || usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(4) || len(output) != 1 {
			t.Fatalf("cancellation output=%s", run.Output)
		}
		thread, err := s.GetThread(ctx, "completion-thread")
		if err != nil || thread.Values["previous"] != "kept" || thread.Values["answer"] != nil || thread.Values["usage"] != nil {
			t.Fatalf("cancellation replaced history: thread=%+v err=%v", thread, err)
		}
		history, err := s.ListCheckpoints(ctx, "completion-thread", 10, "")
		if err != nil || len(history) != 0 {
			t.Fatalf("cancellation created a conversation checkpoint: count=%d err=%v", len(history), err)
		}
	})
}
