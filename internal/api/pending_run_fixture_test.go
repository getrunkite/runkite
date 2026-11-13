package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Managed approvals belong to an actual stored run attempt, just as they do
// behind production run-binding middleware. Keep these fixtures equally real.
func seedManagedPendingFixture(t *testing.T, store state.Store, binding *auth.RunBinding) {
	t.Helper()
	if binding.ThreadID == "" {
		binding.ThreadID = "pending-thread-" + uuid.NewString()
	}
	ctx := tenant.WithContext(context.Background(), binding.TenantID)
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: binding.ThreadID, Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.DeleteRun(ctx, binding.RunID)
		_ = store.DeleteThread(ctx, binding.ThreadID)
	})
	manifest := models.RunManifest{SchemaVersion: 1, TenantID: binding.TenantID, AgentID: binding.AgentID}
	if binding.User != nil {
		manifest.Principal = &models.RunManifestPrincipal{Identity: binding.User.Identity, Permissions: binding.User.Permissions}
	}
	if err := store.CreateRun(ctx, &models.Run{RunID: binding.RunID, ThreadID: binding.ThreadID, AgentID: binding.AgentID, Status: models.RunStatusPending, Metadata: map[string]interface{}{"run_manifest": manifest}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	completion, ok := store.(state.RunCompletionStore)
	if !ok {
		t.Fatal("managed approval fixture requires durable run attempts")
	}
	if accepted, err := completion.BeginRunAttempt(ctx, binding.RunID, binding.Generation); err != nil || !accepted {
		t.Fatalf("begin managed approval fixture: accepted=%v error=%v", accepted, err)
	}
}
