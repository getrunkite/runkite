package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func runBoundCheckpointTests(t *testing.T, factory StoreFactory) {
	seed := func(t *testing.T, generation int64) (state.Store, context.Context, state.RunCheckpointBinding) {
		s := factory(t)
		ctx := tenant.WithContext(context.Background(), "checkpoint-owner")
		now := time.Now().UTC()
		binding := state.RunCheckpointBinding{RunID: "checkpoint-run", ThreadID: "checkpoint-thread", Generation: generation}
		if err := s.CreateThread(ctx, &models.Thread{ThreadID: binding.ThreadID, Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.CreateRun(ctx, &models.Run{RunID: binding.RunID, ThreadID: binding.ThreadID, AgentID: "checkpoint-agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.BeginRunAttempt(ctx, binding.RunID, originalRunIdentity(t, s, binding.RunID), generation); err != nil || !ok {
			t.Fatalf("begin attempt: accepted=%t err=%v", ok, err)
		}
		run, err := s.GetRun(ctx, binding.RunID)
		if err != nil {
			t.Fatal(err)
		}
		binding.RecordIdentity = state.RunIdentity(run)
		if binding.RecordIdentity == "" {
			t.Fatal("run has no immutable record identity")
		}
		return s, ctx, binding
	}
	rejected := func(t *testing.T, err error, reason string) {
		t.Helper()
		var fence *state.ErrRunCheckpointFence
		if !errors.As(err, &fence) || fence.ReasonCode != reason {
			t.Fatalf("expected fence %s, got %v", reason, err)
		}
	}
	t.Run("reclaim_preserves_current_history", func(t *testing.T) {
		s, ctx, old := seed(t, 1)
		if _, err := s.PutBoundOpaqueCheckpoint(ctx, old, "adapter-state", []byte("old"), "test", nil); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.BeginRunAttempt(ctx, old.RunID, originalRunIdentity(t, s, old.RunID), 2); err != nil || !ok {
			t.Fatal(ok, err)
		}
		current := old
		current.Generation = 2
		version, err := s.PutBoundOpaqueCheckpoint(ctx, current, "adapter-state", []byte("current"), "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, stale := range []int64{0, 1} {
			binding := old
			binding.Generation = stale
			// Even a current blob ETag cannot grant a superseded attempt access.
			for _, match := range []*int64{nil, &version} {
				_, err := s.PutBoundOpaqueCheckpoint(ctx, binding, "adapter-state", []byte("stale"), "test", match)
				rejected(t, err, "run_generation_mismatch")
			}
			rejected(t, s.DeleteBoundOpaqueCheckpoint(ctx, binding, "adapter-state"), "run_generation_mismatch")
		}
		blob, err := s.GetOpaqueCheckpoint(ctx, old.ThreadID, "adapter-state")
		if err != nil || string(blob.Data) != "current" || blob.Version != version {
			t.Fatalf("stale mutation changed state: blob=%+v err=%v", blob, err)
		}
	})
	t.Run("terminal_and_missing_runs_cannot_mutate", func(t *testing.T) {
		s, ctx, binding := seed(t, 1)
		if _, err := s.PutBoundOpaqueCheckpoint(ctx, binding, "adapter-state", []byte("saved"), "test", nil); err != nil {
			t.Fatal(err)
		}
		if result, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: binding.RunID, Generation: 1, Status: models.RunStatusInterrupted}); err != nil || !result.Applied {
			t.Fatal(result, err)
		}
		_, err := s.PutBoundOpaqueCheckpoint(ctx, binding, "adapter-state", []byte("late"), "test", nil)
		rejected(t, err, "run_not_inflight")
		rejected(t, s.DeleteBoundOpaqueCheckpoint(ctx, binding, "adapter-state"), "run_not_inflight")
		binding.RunID = "missing-run"
		_, err = s.PutBoundOpaqueCheckpoint(ctx, binding, "adapter-state", []byte("missing"), "test", nil)
		rejected(t, err, "run_not_inflight")
		blob, err := s.GetOpaqueCheckpoint(ctx, binding.ThreadID, "adapter-state")
		if err != nil || string(blob.Data) != "saved" {
			t.Fatal(blob, err)
		}
	})
	t.Run("tenant_thread_legacy_and_cas", func(t *testing.T) {
		s, ctx, binding := seed(t, 0)
		create := state.OpaqueCreateOnly
		version, err := s.PutBoundOpaqueCheckpoint(ctx, binding, "adapter-state", []byte("first"), "test", &create)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.PutBoundOpaqueCheckpoint(ctx, binding, "adapter-state", nil, "test", &create)
		var conflict *state.ErrConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("create-only conflict: %v", err)
		}
		next, err := s.PutBoundOpaqueCheckpoint(ctx, binding, "adapter-state", []byte("second"), "test", &version)
		if err != nil || next != version+1 {
			t.Fatal(next, err)
		}
		_, err = s.PutBoundOpaqueCheckpoint(ctx, binding, "adapter-state", nil, "test", &version)
		if !errors.As(err, &conflict) {
			t.Fatalf("stale ETag: %v", err)
		}
		other := binding
		other.ThreadID = "other-thread"
		_, err = s.PutBoundOpaqueCheckpoint(ctx, other, "adapter-state", nil, "test", nil)
		rejected(t, err, "run_thread_mismatch")
		foreign := tenant.WithContext(ctx, "foreign-tenant")
		_, err = s.PutBoundOpaqueCheckpoint(foreign, binding, "adapter-state", nil, "test", nil)
		rejected(t, err, "run_not_inflight")
		if err := s.DeleteBoundOpaqueCheckpoint(ctx, binding, "adapter-state"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetOpaqueCheckpoint(ctx, binding.ThreadID, "adapter-state"); !errors.As(err, new(*state.ErrNotFound)) {
			t.Fatalf("delete did not remove checkpoint: %v", err)
		}
	})
	t.Run("recreated_record_rejects_old_and_missing_identity", func(t *testing.T) {
		s, ctx, old := seed(t, 1)
		original, err := s.GetRun(ctx, old.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := s.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, RunID: old.RunID, Generation: 1, Status: models.RunStatusSuccess}); err != nil || !result.Applied {
			t.Fatal(result, err)
		}
		if err := s.DeleteRun(ctx, old.RunID); err != nil {
			t.Fatal(err)
		}
		// Deliberately reuse the timestamps too. Time is not a row identity.
		replacement := &models.Run{RunID: old.RunID, ThreadID: old.ThreadID, AgentID: original.AgentID, Status: models.RunStatusPending, CreatedAt: original.CreatedAt, UpdatedAt: original.UpdatedAt, Metadata: map[string]interface{}{}}
		if err := s.CreateRun(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		if accepted, err := s.BeginRunAttempt(ctx, old.RunID, originalRunIdentity(t, s, old.RunID), old.Generation); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		replacement, err = s.GetRun(ctx, old.RunID)
		if err != nil {
			t.Fatal(err)
		}
		current := old
		current.RecordIdentity = state.RunIdentity(replacement)
		if current.RecordIdentity == "" || current.RecordIdentity == old.RecordIdentity {
			t.Fatal("replacement reused record identity")
		}
		version, err := s.PutBoundOpaqueCheckpoint(ctx, current, "adapter-state", []byte("current"), "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range []*int64{nil, &version} {
			_, err := s.PutBoundOpaqueCheckpoint(ctx, old, "adapter-state", []byte("stale"), "test", match)
			rejected(t, err, "run_incarnation_mismatch")
		}
		create := state.OpaqueCreateOnly
		_, err = s.PutBoundOpaqueCheckpoint(ctx, old, "new-state", []byte("stale"), "test", &create)
		rejected(t, err, "run_incarnation_mismatch")
		rejected(t, s.DeleteBoundOpaqueCheckpoint(ctx, old, "adapter-state"), "run_incarnation_mismatch")
		missing := current
		missing.RecordIdentity = ""
		_, err = s.PutBoundOpaqueCheckpoint(ctx, missing, "adapter-state", nil, "test", nil)
		rejected(t, err, "run_binding_required")
		rejected(t, s.DeleteBoundOpaqueCheckpoint(ctx, missing, "adapter-state"), "run_binding_required")
		if blob, err := s.GetOpaqueCheckpoint(ctx, old.ThreadID, "adapter-state"); err != nil || string(blob.Data) != "current" || blob.Version != version {
			t.Fatalf("replacement state changed: blob=%+v err=%v", blob, err)
		}
		// Trusted completion uses the same immutable identity, including Mongo.
		if _, err := s.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: old.RecordIdentity, RunID: old.RunID, Generation: 1, Status: models.RunStatusSuccess}); err == nil {
			t.Fatal("old record identity completed the replacement")
		}
		if result, err := s.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: current.RecordIdentity, RunID: old.RunID, Generation: 1, Status: models.RunStatusSuccess}); err != nil || !result.Applied {
			t.Fatal(result, err)
		}
	})
}
