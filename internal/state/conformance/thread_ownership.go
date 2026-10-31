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

// Thread children must bind the current parent's tenant at the mutation, not
// merely rely on a preceding API lookup or an ID-only foreign key.
func runThreadChildOwnershipTests(t *testing.T, factory StoreFactory) {
	for _, operation := range []string{"run", "admitted_run", "checkpoint", "opaque_checkpoint"} {
		t.Run(operation, func(t *testing.T) {
			s := factory(t)
			a := tenant.WithContext(context.Background(), "parent-owner-a")
			b := tenant.WithContext(context.Background(), "parent-owner-b")
			now := time.Now().UTC()
			threadID := "reused-thread"
			thread := &models.Thread{ThreadID: threadID, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}
			if err := s.CreateThread(a, thread); err != nil {
				t.Fatal(err)
			}
			if claimed, err := s.TryClaimThread(a, threadID); err != nil || !claimed {
				t.Fatal(claimed, err)
			}
			// Model a delayed child mutation after the authenticated parent was
			// deleted and a different tenant reused its custom public ID.
			if err := s.DeleteThread(a, threadID); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateThread(b, thread); err != nil {
				t.Fatal(err)
			}
			run := &models.Run{RunID: "stale-run", ThreadID: threadID, AgentID: "agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}
			var err error
			switch operation {
			case "run":
				err = s.CreateRun(a, run)
			case "admitted_run":
				err = s.CreateRunAdmitted(a, run, &state.RunAdmissionCaps{TenantConcurrent: 3})
			case "checkpoint":
				err = s.SaveCheckpoint(a, threadID, &models.ThreadState{Checkpoint: models.ThreadCheckpoint{ThreadID: threadID, CheckpointID: "stale-checkpoint"}, Values: map[string]interface{}{"private": "a"}})
			case "opaque_checkpoint":
				_, err = s.PutOpaqueCheckpoint(a, threadID, "stale-opaque", []byte("private-a"), "test", nil)
			}
			var missing *state.ErrNotFound
			if !errors.As(err, &missing) || missing.Resource != "thread" {
				t.Fatalf("stale child mutation must reject replacement tenant's parent: %T %v", err, err)
			}
			if _, err := s.GetRun(a, run.RunID); err == nil {
				t.Fatal("rejected child run was persisted")
			}
			if _, err := s.GetLatestCheckpoint(a, threadID); err == nil {
				t.Fatal("rejected checkpoint was persisted")
			}
			if _, err := s.GetOpaqueCheckpoint(a, threadID, "stale-opaque"); err == nil {
				t.Fatal("rejected opaque checkpoint was persisted")
			}
			if _, err := s.GetThread(b, threadID); err != nil {
				t.Fatal("replacement parent was changed", err)
			}
		})
	}
}
