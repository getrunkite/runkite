package mysql_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// Mirrors the shared conformance suite's CP-00x cases (internal/state/
// conformance's runCheckpointTests) -- hand-written for now, same reason
// as agents_test.go/threads_test.go/runs_test.go: *mysql.Store doesn't
// satisfy the full state.Store interface yet.

func TestCheckpoint_SaveGetRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-cp1")

	created := now.Format(time.RFC3339)
	ts := &models.ThreadState{
		Values:     map[string]interface{}{"messages": []interface{}{"hello"}},
		Next:       []string{},
		Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp1", CheckpointNS: "", CheckpointID: "cp-001"},
		Metadata:   map[string]interface{}{"source": "test"},
		CreatedAt:  &created,
		Tasks:      []interface{}{},
		Interrupts: []interface{}{},
	}
	if err := s.SaveCheckpoint(ctx, "t-cp1", ts); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	got, err := s.GetLatestCheckpoint(ctx, "t-cp1")
	if err != nil {
		t.Fatalf("GetLatestCheckpoint: %v", err)
	}
	if got.Checkpoint.CheckpointID != "cp-001" {
		t.Errorf("CheckpointID = %q, want cp-001", got.Checkpoint.CheckpointID)
	}
	if got.Checkpoint.ThreadID != "t-cp1" {
		t.Errorf("ThreadID = %q, want t-cp1", got.Checkpoint.ThreadID)
	}
	if got.Values["messages"] == nil {
		t.Error("Values.messages should not be nil")
	}
	if got.Metadata["source"] != "test" {
		t.Errorf("Metadata[source] = %+v, want test", got.Metadata["source"])
	}
	if got.ParentCheckpoint != nil {
		t.Errorf("expected nil ParentCheckpoint for a root checkpoint, got %+v", got.ParentCheckpoint)
	}
}

func TestCheckpoint_GetLatestReturnsNewest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-cp2")

	for i, id := range []string{"cp-a", "cp-b", "cp-c"} {
		ts := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		if err := s.SaveCheckpoint(ctx, "t-cp2", &models.ThreadState{
			Values: map[string]interface{}{"step": id}, Next: []string{},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp2", CheckpointNS: "", CheckpointID: id},
			Metadata:   map[string]interface{}{}, CreatedAt: &ts,
			Tasks: []interface{}{}, Interrupts: []interface{}{},
		}); err != nil {
			t.Fatalf("SaveCheckpoint(%s): %v", id, err)
		}
	}

	got, err := s.GetLatestCheckpoint(ctx, "t-cp2")
	if err != nil {
		t.Fatalf("GetLatestCheckpoint: %v", err)
	}
	if got.Checkpoint.CheckpointID != "cp-c" {
		t.Errorf("latest = %q, want cp-c", got.Checkpoint.CheckpointID)
	}
}

func TestCheckpoint_ListOrderingAndLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-cp3")

	for i := 0; i < 5; i++ {
		ts := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		if err := s.SaveCheckpoint(ctx, "t-cp3", &models.ThreadState{
			Values: map[string]interface{}{"i": i}, Next: []string{},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp3", CheckpointNS: "", CheckpointID: fmt.Sprintf("cp-%d", i)},
			Metadata:   map[string]interface{}{}, CreatedAt: &ts,
			Tasks: []interface{}{}, Interrupts: []interface{}{},
		}); err != nil {
			t.Fatalf("SaveCheckpoint(%d): %v", i, err)
		}
	}

	all, err := s.ListCheckpoints(ctx, "t-cp3", 100, "")
	if err != nil {
		t.Fatalf("ListCheckpoints: %v", err)
	}
	if len(all) != 5 {
		t.Fatalf("got %d checkpoints, want 5", len(all))
	}
	if all[0].Checkpoint.CheckpointID != "cp-4" {
		t.Errorf("first (newest) = %q, want cp-4", all[0].Checkpoint.CheckpointID)
	}

	limited, err := s.ListCheckpoints(ctx, "t-cp3", 2, "")
	if err != nil {
		t.Fatalf("ListCheckpoints(limit=2): %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("got %d, want 2", len(limited))
	}
}

func TestCheckpoint_ListBeforeFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-cp4")

	for i := 0; i < 5; i++ {
		ts := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		if err := s.SaveCheckpoint(ctx, "t-cp4", &models.ThreadState{
			Values: map[string]interface{}{"i": i}, Next: []string{},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp4", CheckpointNS: "", CheckpointID: fmt.Sprintf("cpb-%d", i)},
			Metadata:   map[string]interface{}{}, CreatedAt: &ts,
			Tasks: []interface{}{}, Interrupts: []interface{}{},
		}); err != nil {
			t.Fatalf("SaveCheckpoint(%d): %v", i, err)
		}
	}

	before, err := s.ListCheckpoints(ctx, "t-cp4", 100, "cpb-3")
	if err != nil {
		t.Fatalf("ListCheckpoints(before): %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("got %d checkpoints before cpb-3, want 3", len(before))
	}
	if before[0].Checkpoint.CheckpointID != "cpb-2" {
		t.Errorf("first before = %q, want cpb-2", before[0].Checkpoint.CheckpointID)
	}
}

func TestCheckpoint_ParentCheckpointTracking(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-cp5")

	ts1 := now.Format(time.RFC3339)
	if err := s.SaveCheckpoint(ctx, "t-cp5", &models.ThreadState{
		Values: map[string]interface{}{"v": 1}, Next: []string{},
		Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp5", CheckpointNS: "", CheckpointID: "parent-cp"},
		Metadata:   map[string]interface{}{}, CreatedAt: &ts1,
		Tasks: []interface{}{}, Interrupts: []interface{}{},
	}); err != nil {
		t.Fatalf("SaveCheckpoint (parent): %v", err)
	}

	ts2 := now.Add(time.Second).Format(time.RFC3339)
	if err := s.SaveCheckpoint(ctx, "t-cp5", &models.ThreadState{
		Values: map[string]interface{}{"v": 2}, Next: []string{"next_node"},
		Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp5", CheckpointNS: "", CheckpointID: "child-cp"},
		Metadata:   map[string]interface{}{}, CreatedAt: &ts2,
		ParentCheckpoint: &models.ThreadCheckpoint{ThreadID: "t-cp5", CheckpointNS: "", CheckpointID: "parent-cp"},
		Tasks:            []interface{}{}, Interrupts: []interface{}{},
	}); err != nil {
		t.Fatalf("SaveCheckpoint (child): %v", err)
	}

	got, err := s.GetLatestCheckpoint(ctx, "t-cp5")
	if err != nil {
		t.Fatalf("GetLatestCheckpoint: %v", err)
	}
	if got.ParentCheckpoint == nil {
		t.Fatal("ParentCheckpoint should not be nil")
	}
	if got.ParentCheckpoint.CheckpointID != "parent-cp" {
		t.Errorf("parent = %q, want parent-cp", got.ParentCheckpoint.CheckpointID)
	}
	if len(got.Next) != 1 || got.Next[0] != "next_node" {
		t.Errorf("Next = %v, want [next_node]", got.Next)
	}
}

func TestCheckpoint_GetNonexistentThread(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetLatestCheckpoint(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for a thread with no checkpoints")
	}
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Errorf("expected *state.ErrNotFound, got %T: %v", err, err)
	}
}

func TestCheckpoint_CascadeDeleteWithThread(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-cp7")

	ts := now.Format(time.RFC3339)
	if err := s.SaveCheckpoint(ctx, "t-cp7", &models.ThreadState{
		Values: map[string]interface{}{"v": 1}, Next: []string{},
		Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp7", CheckpointNS: "", CheckpointID: "cp-del"},
		Metadata:   map[string]interface{}{}, CreatedAt: &ts,
		Tasks: []interface{}{}, Interrupts: []interface{}{},
	}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	if err := s.DeleteThread(ctx, "t-cp7"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	if _, err := s.GetLatestCheckpoint(ctx, "t-cp7"); err == nil {
		t.Error("checkpoints should be cascade-deleted when their thread is deleted")
	}
}

// TestCheckpoint_TenantIsolation isn't in the shared conformance suite
// (checkpoints are accessed by thread_id, which is itself tenant-
// filtered at the Threads layer -- there's no dedicated conformance
// case for it), but thread_checkpoints does carry its own tenant_id
// column and every query above filters by it, so it's worth a direct
// check: tenant B must not see tenant A's checkpoints for a thread_id
// it doesn't own, and a system context must see them regardless.
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
