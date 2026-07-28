package mysql_test

import (
	"context"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// Tenant-isolation cases genuinely NOT covered by the shared
// conformance suite's runTenantIsolationTests -- each doc comment below
// explains the specific gap. Cases the shared suite already covers
// (agents/threads/runs/store_items/cron_schedules/run_cache basic
// isolation) are NOT duplicated here.

// TestAgent_TenantIsolation adds version-isolation on top of what
// conformance's agents_same_id_different_tenants_dont_collide already
// checks (that two tenants don't collide on the same agent_id): tenant
// A upserting its own agent must never bump tenant B's independent
// version counter for the same agent_id.
func TestAgent_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")

	s.UpsertAgent(ctxA, &models.Agent{AgentID: "shared", Name: "a-name", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})
	s.UpsertAgent(ctxB, &models.Agent{AgentID: "shared", Name: "b-name", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	gotA, err := s.GetAgent(ctxA, "shared")
	if err != nil || gotA.Name != "a-name" {
		t.Fatalf("expected tenant-a's own agent, got %+v err=%v", gotA, err)
	}
	gotB, err := s.GetAgent(ctxB, "shared")
	if err != nil || gotB.Name != "b-name" {
		t.Fatalf("expected tenant-b's own agent, got %+v err=%v", gotB, err)
	}

	agentA := &models.Agent{AgentID: "shared", Name: "a-name-v2", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}
	s.UpsertAgent(ctxA, agentA)
	gotBAfter, _ := s.GetAgent(ctxB, "shared")
	if gotBAfter.Version != 1 {
		t.Fatalf("expected tenant-b's version unaffected by tenant-a's upsert, got %d", gotBAfter.Version)
	}
}

// TestThread_TenantIsolation matches this schema's actual isolation
// model: unlike Agents/Registry (composite tenant-scoped primary keys,
// so two tenants CAN independently own the same natural key),
// threads.thread_id is globally unique -- PRIMARY KEY (thread_id) alone.
// Conformance's threads_get_returns_not_found_across_tenants already
// checks the ErrNotFound-on-lookup half; this adds the half it doesn't:
// a second tenant attempting to CREATE a thread reusing an existing
// thread_id must see a real ErrConflict, not silently succeed with its
// own independent row.
func TestThread_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	now := time.Now().UTC()

	if err := s.CreateThread(ctxA, &models.Thread{ThreadID: "shared-uuid-thread", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{"who": "a"}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateThread (tenant A): %v", err)
	}

	createErr := s.CreateThread(ctxB, &models.Thread{ThreadID: "shared-uuid-thread", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{"who": "b"}, CreatedAt: now, UpdatedAt: now})
	if _, ok := createErr.(*state.ErrConflict); !ok {
		t.Fatalf("expected tenant B's create with a colliding thread_id to report ErrConflict, got %v", createErr)
	}
}

// TestRun_TenantIsolation adds two checks conformance's
// runs_isolated_by_tenant doesn't make: DeleteRun from the wrong tenant
// must report ErrNotFound (0 rows affected) rather than succeeding, and
// a system context must be able to read a run regardless of which
// tenant created it (conformance only exercises system-context bypass
// for Threads, not Runs).
func TestRun_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	now := time.Now().UTC()
	seedThread(t, ctxA, s, "t-tenant-a")

	if err := s.CreateRun(ctxA, &models.Run{RunID: "shared-uuid-run", ThreadID: "t-tenant-a", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateRun (tenant A): %v", err)
	}

	if err := s.DeleteRun(ctxB, "shared-uuid-run"); err == nil {
		t.Fatal("tenant B deleting tenant A's run should report ErrNotFound (0 rows affected), not succeed")
	}
	if _, err := s.GetRun(ctxA, "shared-uuid-run"); err != nil {
		t.Fatalf("tenant A's run must survive tenant B's delete attempt: %v", err)
	}

	if _, err := s.GetRun(tenant.SystemContext(context.Background()), "shared-uuid-run"); err != nil {
		t.Fatalf("system context should see the run regardless of tenant: %v", err)
	}
}

// TestCheckpoint_TenantIsolation isn't covered by the shared
// conformance suite at all (checkpoints are normally reached via an
// already-tenant-filtered thread_id, but thread_checkpoints carries its
// own tenant_id and every query filters by it): tenant B must not see
// tenant A's checkpoints for a thread_id it doesn't own, and a system
// context must see them regardless.
func TestCheckpoint_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	now := time.Now().UTC()
	seedThread(t, ctxA, s, "t-cp-tenant")

	created := now.Format(time.RFC3339)
	if err := s.SaveCheckpoint(ctxA, "t-cp-tenant", &models.ThreadState{
		Values: map[string]interface{}{"v": 1}, Next: []string{},
		Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp-tenant", CheckpointNS: "", CheckpointID: "cp-tenant-a"},
		Metadata:   map[string]interface{}{}, CreatedAt: &created,
		Tasks: []interface{}{}, Interrupts: []interface{}{},
	}); err != nil {
		t.Fatalf("SaveCheckpoint (tenant A): %v", err)
	}

	if _, err := s.GetLatestCheckpoint(ctxB, "t-cp-tenant"); err == nil {
		t.Fatal("tenant B must not see tenant A's checkpoint")
	} else if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound, got %T: %v", err, err)
	}
	if list, err := s.ListCheckpoints(ctxB, "t-cp-tenant", 10, ""); err != nil {
		t.Fatalf("ListCheckpoints (tenant B): %v", err)
	} else if len(list) != 0 {
		t.Fatalf("tenant B's ListCheckpoints should be empty, got %d", len(list))
	}

	if _, err := s.GetLatestCheckpoint(tenant.SystemContext(context.Background()), "t-cp-tenant"); err != nil {
		t.Fatalf("system context should see the checkpoint regardless of tenant: %v", err)
	}
}
