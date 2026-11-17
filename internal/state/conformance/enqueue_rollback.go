package conformance

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// These store tests run against every built-in backend, including Mongo's
// internal ObjectID proof. They do not use SQL-only public run references.
func runEnqueueRollbackTests(t *testing.T, factory StoreFactory) {
	seed := func(t *testing.T) (state.Store, context.Context, *models.Run, *state.RunCompletion) {
		t.Helper()
		store := factory(t)
		ctx := tenant.ScopedContext(t.Context(), "enqueue-owner")
		now := time.Now().UTC()
		if err := store.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		run := &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}
		if err := store.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		identity := state.RunIdentity(run)
		if identity == "" {
			t.Fatal("created run has no immutable insertion identity")
		}
		return store, ctx, run, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: identity, FailUnstarted: true, RunID: run.RunID, Status: models.RunStatusError, Error: "enqueue failed"}
	}
	t.Run("never_started_completes_once_and_late_admission_is_rejected", func(t *testing.T) {
		store, ctx, run, completion := seed(t)
		result, err := store.CompleteRun(ctx, completion)
		if err != nil || !result.Accepted || !result.Applied {
			t.Fatalf("rejection did not commit: %+v %v", result, err)
		}
		again, err := store.CompleteRun(ctx, completion)
		if err != nil || !again.Accepted || again.Applied {
			t.Fatalf("rejection replay was not idempotent: %+v %v", again, err)
		}
		if accepted, err := store.BeginRunAttempt(ctx, run.RunID, 1); err != nil || accepted {
			t.Fatalf("late queued delivery started a failed run: accepted=%v err=%v", accepted, err)
		}
		current, err := store.GetRun(ctx, run.RunID)
		if err != nil || current.Status != models.RunStatusError || current.Error != "enqueue failed" {
			t.Fatalf("wrong retained rejection: %+v %v", current, err)
		}
		thread, err := store.GetThread(ctx, run.ThreadID)
		if err != nil || thread.Status != models.ThreadStatusIdle {
			t.Fatalf("rejection did not release its claim: %+v %v", thread, err)
		}
	})
	for _, generation := range []int64{0, 1} {
		name := "admitted_current"
		if generation == 0 {
			name = "admitted_legacy_zero"
		}
		t.Run(name, func(t *testing.T) {
			store, ctx, run, completion := seed(t)
			if accepted, err := store.BeginRunAttempt(ctx, run.RunID, generation); err != nil || !accepted {
				t.Fatal(accepted, err)
			}
			result, err := store.CompleteRun(ctx, completion)
			if err != nil || result.Accepted || result.Applied {
				t.Fatalf("rollback accepted an admitted worker: %+v %v", result, err)
			}
			current, err := store.GetRun(ctx, run.RunID)
			thread, threadErr := store.GetThread(ctx, run.ThreadID)
			if err != nil || current.Status != models.RunStatusRunning || threadErr != nil || thread.Status != models.ThreadStatusBusy {
				t.Fatal("admitted worker lost its run or claim")
			}
		})
	}
	t.Run("terminal_winner_preserves_output_metadata_and_checkpoints", func(t *testing.T) {
		store, ctx, run, completion := seed(t)
		if accepted, err := store.BeginRunAttempt(ctx, run.RunID, 1); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		winner := &state.RunCompletion{RunID: run.RunID, Generation: 1, Status: models.RunStatusSuccess, Values: map[string]interface{}{"answer": "committed"}}
		if result, err := store.CompleteRun(ctx, winner); err != nil || !result.Applied {
			t.Fatal(result, err)
		}
		before, err := store.GetRun(ctx, run.RunID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.CompleteRun(ctx, completion)
		if err != nil || result.Accepted || result.Applied {
			t.Fatalf("rollback accepted a terminal winner: %+v %v", result, err)
		}
		after, err := store.GetRun(ctx, run.RunID)
		if err != nil || after.Status != before.Status || string(after.Output) != string(before.Output) || !reflect.DeepEqual(after.Metadata, before.Metadata) {
			t.Fatal("rollback changed committed output or metadata")
		}
		checkpoints, err := store.ListCheckpoints(ctx, run.ThreadID, 10, "")
		if err != nil || len(checkpoints) != 1 || checkpoints[0].Values["answer"] != "committed" {
			t.Fatalf("rollback changed completion checkpoint: %+v %v", checkpoints, err)
		}
	})
	t.Run("replacement_at_same_timestamp_is_fenced", func(t *testing.T) {
		store, ctx, run, completion := seed(t)
		if err := store.DeleteRun(ctx, run.RunID); err != nil {
			t.Fatal(err)
		}
		copy := *run
		copy.IncarnationID, copy.StorageIdentity = "", ""
		if err := store.CreateRun(ctx, &copy); err != nil {
			t.Fatal(err)
		}
		if state.RunIdentity(&copy) == completion.ExpectedIncarnationID {
			t.Fatal("replacement reused original immutable identity")
		}
		result, err := store.CompleteRun(ctx, completion)
		var conflict *state.ErrConflict
		if !errors.As(err, &conflict) || result.Accepted || result.Applied {
			t.Fatalf("old rollback was not fenced: %+v %v", result, err)
		}
		current, err := store.GetRun(ctx, run.RunID)
		thread, threadErr := store.GetThread(ctx, run.ThreadID)
		if err != nil || current.Status != models.RunStatusPending || state.RunIdentity(current) != state.RunIdentity(&copy) || threadErr != nil || thread.Status != models.ThreadStatusBusy {
			t.Fatal("old rollback changed replacement or released its claim")
		}
	})
	t.Run("admission_and_rollback_have_one_transaction_winner", func(t *testing.T) {
		store, ctx, run, completion := seed(t)
		start := make(chan struct{})
		var ready sync.WaitGroup
		ready.Add(2)
		var admitted bool
		var admissionErr, rollbackErr error
		var rolledBack state.CompletionResult
		go func() {
			defer ready.Done()
			<-start
			admitted, admissionErr = store.BeginRunAttempt(ctx, run.RunID, 1)
		}()
		go func() {
			defer ready.Done()
			<-start
			rolledBack, rollbackErr = store.CompleteRun(ctx, completion)
		}()
		close(start)
		ready.Wait()
		if admissionErr != nil || rollbackErr != nil || admitted == rolledBack.Applied {
			t.Fatalf("expected exactly one winner: admitted=%v/%v rollback=%+v/%v", admitted, admissionErr, rolledBack, rollbackErr)
		}
		current, err := store.GetRun(ctx, run.RunID)
		thread, threadErr := store.GetThread(ctx, run.ThreadID)
		if err != nil || threadErr != nil {
			t.Fatal(err, threadErr)
		}
		if admitted && (current.Status != models.RunStatusRunning || thread.Status != models.ThreadStatusBusy) {
			t.Fatal("rollback released the winning worker")
		}
		if rolledBack.Applied && (current.Status != models.RunStatusError || thread.Status != models.ThreadStatusIdle) {
			t.Fatal("rejected late delivery retained an executable run")
		}
	})
}
