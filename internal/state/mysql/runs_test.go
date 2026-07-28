package mysql_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// assertJSONEqual compares two JSON documents structurally rather than
// byte-for-byte -- MySQL's JSON column type re-serializes to its own
// canonical form on read (spacing differs from whatever compact form
// was written), so a byte-exact comparison would fail on a formatting
// difference that carries no actual data loss.
func assertJSONEqual(t *testing.T, got json.RawMessage, wantRaw string) {
	t.Helper()
	var gotVal, wantVal interface{}
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("assertJSONEqual: got is not valid JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(wantRaw), &wantVal); err != nil {
		t.Fatalf("assertJSONEqual: want is not valid JSON: %v (%s)", err, wantRaw)
	}
	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Errorf("JSON mismatch: got %s, want (structurally) %s", got, wantRaw)
	}
}

// seedThread is a small helper -- every run in this package has a FK to
// threads(thread_id) ON DELETE CASCADE, same as Postgres/SQLite, so
// every test needs a thread row to exist before it can create a run.
func seedThread(t *testing.T, ctx context.Context, s interface {
	CreateThread(context.Context, *models.Thread) error
}, threadID string) {
	t.Helper()
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: threadID, Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seedThread(%s): %v", threadID, err)
	}
}

func TestRun_CreateGetRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-run")

	run := &models.Run{
		RunID:     "run-001",
		ThreadID:  "t-run",
		AgentID:   "agent-x",
		Status:    models.RunStatusPending,
		Metadata:  map[string]interface{}{"tag": "test"},
		Input:     json.RawMessage(`{"messages":[{"role":"user","content":"hi"}]}`),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	got, err := s.GetRun(ctx, "run-001")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Status != models.RunStatusPending {
		t.Errorf("Status = %q, want %q", got.Status, models.RunStatusPending)
	}
	if got.ThreadID != "t-run" || got.AgentID != "agent-x" {
		t.Errorf("ThreadID/AgentID = %q/%q, want t-run/agent-x", got.ThreadID, got.AgentID)
	}
	if got.AssistantID != "agent-x" {
		t.Errorf("AssistantID (SDK compat alias) = %q, want agent-x", got.AssistantID)
	}
	if got.Metadata["tag"] != "test" {
		t.Errorf("Metadata[tag] = %+v, want test", got.Metadata["tag"])
	}
	// Structural comparison, not byte-exact: MySQL's JSON column type
	// re-serializes on read (canonical spacing, e.g. `"content": "hi"`
	// instead of the compact form written), same as jsonb's own
	// normalization on Postgres -- a formatting difference, not a data
	// loss bug.
	assertJSONEqual(t, got.Input, `{"messages":[{"role":"user","content":"hi"}]}`)
	if got.ParentRunID != nil || got.RootRunID != nil || got.Depth != 0 {
		t.Errorf("top-level run should have nil parent/root and depth 0, got parent=%+v root=%+v depth=%d", got.ParentRunID, got.RootRunID, got.Depth)
	}
}

func TestRun_GetNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.GetRun(ctx, "does-not-exist"); err == nil {
		t.Fatal("expected ErrNotFound for a run that was never created")
	} else if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected *state.ErrNotFound, got %T: %v", err, err)
	}
}

// TestRun_A2AParentRootDepthPersist mirrors the shared conformance
// suite's SS-006a: a delegated (A2A) run's parent_run_id/root_run_id/
// depth must round-trip through both GetRun and SearchRuns, since a
// past regression left SearchRuns's SELECT with a narrower column list
// than GetRun's on another backend.
func TestRun_A2AParentRootDepthPersist(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-a2a")

	if err := s.CreateRun(ctx, &models.Run{RunID: "a2a-root", ThreadID: "t-a2a", Status: models.RunStatusRunning, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateRun (root): %v", err)
	}

	parentID, rootID := "a2a-root", "a2a-root"
	child := &models.Run{
		RunID: "a2a-child", ThreadID: "t-a2a", Status: models.RunStatusPending,
		CreatedAt: now, UpdatedAt: now,
		ParentRunID: &parentID, RootRunID: &rootID, Depth: 1,
	}
	if err := s.CreateRun(ctx, child); err != nil {
		t.Fatalf("CreateRun (delegated): %v", err)
	}

	got, err := s.GetRun(ctx, "a2a-child")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ParentRunID == nil || *got.ParentRunID != "a2a-root" {
		t.Errorf("GetRun: ParentRunID = %+v, want a2a-root", got.ParentRunID)
	}
	if got.RootRunID == nil || *got.RootRunID != "a2a-root" {
		t.Errorf("GetRun: RootRunID = %+v, want a2a-root", got.RootRunID)
	}
	if got.Depth != 1 {
		t.Errorf("GetRun: Depth = %d, want 1", got.Depth)
	}

	results, err := s.SearchRuns(ctx, &models.RunSearchRequest{ThreadID: "t-a2a", Limit: 10})
	if err != nil {
		t.Fatalf("SearchRuns: %v", err)
	}
	var foundChild, foundRoot bool
	for _, r := range results {
		if r.RunID == "a2a-child" {
			foundChild = true
			if r.ParentRunID == nil || *r.ParentRunID != "a2a-root" || r.Depth != 1 {
				t.Errorf("SearchRuns: parent/depth not persisted, got parent=%+v depth=%d", r.ParentRunID, r.Depth)
			}
		}
		if r.RunID == "a2a-root" {
			foundRoot = true
			if r.ParentRunID != nil || r.Depth != 0 {
				t.Errorf("SearchRuns: top-level run should have nil parent and depth 0, got parent=%+v depth=%d", r.ParentRunID, r.Depth)
			}
		}
	}
	if !foundChild || !foundRoot {
		t.Fatalf("SearchRuns: expected both root and child, foundRoot=%v foundChild=%v", foundRoot, foundChild)
	}
}

func TestRun_UpdateStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-st")

	if err := s.CreateRun(ctx, &models.Run{RunID: "run-st", ThreadID: "t-st", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	output := json.RawMessage(`{"result":"done"}`)
	if err := s.UpdateRunStatus(ctx, "run-st", models.RunStatusSuccess, output, ""); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	got, err := s.GetRun(ctx, "run-st")
	if err != nil {
		t.Fatalf("GetRun after update: %v", err)
	}
	if got.Status != models.RunStatusSuccess {
		t.Errorf("Status = %q, want success", got.Status)
	}
	assertJSONEqual(t, got.Output, `{"result":"done"}`)

	if err := s.UpdateRunStatus(ctx, "run-st", models.RunStatusError, nil, "boom"); err != nil {
		t.Fatalf("UpdateRunStatus (error): %v", err)
	}
	got, err = s.GetRun(ctx, "run-st")
	if err != nil {
		t.Fatalf("GetRun after second update: %v", err)
	}
	if got.Status != models.RunStatusError || got.Error != "boom" {
		t.Errorf("Status/Error = %q/%q, want error/boom", got.Status, got.Error)
	}
}

func TestRun_Delete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-del")

	if err := s.CreateRun(ctx, &models.Run{RunID: "run-del", ThreadID: "t-del", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.DeleteRun(ctx, "run-del"); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := s.GetRun(ctx, "run-del"); err == nil {
		t.Fatal("expected ErrNotFound after delete")
	}
}

func TestRun_DeleteNonexistent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.DeleteRun(ctx, "never-existed")
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected *state.ErrNotFound deleting a run that never existed, got %T: %v", err, err)
	}
}

func TestRun_SearchByStatusThreadAgentAndMetadata(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-search")
	seedThread(t, ctx, s, "t-search-2")

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateRun(ctx, &models.Run{RunID: "r-1", ThreadID: "t-search", AgentID: "agent-a", Status: models.RunStatusPending, Metadata: map[string]interface{}{"env": "prod"}, CreatedAt: now, UpdatedAt: now}))
	must(s.CreateRun(ctx, &models.Run{RunID: "r-2", ThreadID: "t-search", AgentID: "agent-b", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{"env": "dev"}, CreatedAt: now, UpdatedAt: now}))
	must(s.CreateRun(ctx, &models.Run{RunID: "r-3", ThreadID: "t-search-2", AgentID: "agent-a", Status: models.RunStatusPending, Metadata: map[string]interface{}{"env": "prod"}, CreatedAt: now, UpdatedAt: now}))

	// By thread.
	byThread, err := s.SearchRuns(ctx, &models.RunSearchRequest{ThreadID: "t-search", Limit: 10})
	must(err)
	if len(byThread) != 2 {
		t.Errorf("SearchRuns(thread=t-search) len = %d, want 2", len(byThread))
	}

	// By status.
	pendingStatus := models.RunStatusPending
	byStatus, err := s.SearchRuns(ctx, &models.RunSearchRequest{Status: &pendingStatus, Limit: 10})
	must(err)
	if len(byStatus) != 2 {
		t.Errorf("SearchRuns(status=pending) len = %d, want 2", len(byStatus))
	}

	// By agent.
	byAgent, err := s.SearchRuns(ctx, &models.RunSearchRequest{AgentID: "agent-a", Limit: 10})
	must(err)
	if len(byAgent) != 2 {
		t.Errorf("SearchRuns(agent=agent-a) len = %d, want 2", len(byAgent))
	}

	// By metadata (JSON_EXTRACT scalar comparison -- the same MySQL
	// implicit-JSON-cast rule SearchThreads already relies on).
	byMeta, err := s.SearchRuns(ctx, &models.RunSearchRequest{Metadata: map[string]interface{}{"env": "prod"}, Limit: 10})
	must(err)
	if len(byMeta) != 2 {
		t.Errorf("SearchRuns(metadata.env=prod) len = %d, want 2", len(byMeta))
	}
	for _, r := range byMeta {
		if r.Metadata["env"] != "prod" {
			t.Errorf("SearchRuns(metadata.env=prod) returned a non-matching run: %+v", r.Metadata)
		}
	}
}

// TestRun_TenantIsolation matches Threads' isolation model, not Agents':
// runs.run_id is a globally unique PRIMARY KEY (not tenant-scoped), so
// isolation is enforced via tenant-filtered WHERE clauses on access, and
// a system context (tenant.SystemContext) intentionally bypasses that
// filter -- exercised here too since PruneRuns depends on it.
func TestRun_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	now := time.Now().UTC()
	seedThread(t, ctxA, s, "t-tenant-a")

	if err := s.CreateRun(ctxA, &models.Run{RunID: "shared-uuid-run", ThreadID: "t-tenant-a", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateRun (tenant A): %v", err)
	}

	if _, err := s.GetRun(ctxB, "shared-uuid-run"); err == nil {
		t.Fatal("tenant B must not be able to read tenant A's run")
	} else if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound for cross-tenant get, got %T: %v", err, err)
	}

	// Cross-tenant update/delete must be silent no-ops, not data loss
	// for the actual owner.
	if err := s.UpdateRunStatus(ctxB, "shared-uuid-run", models.RunStatusError, nil, "hijacked"); err != nil {
		t.Fatalf("UpdateRunStatus (tenant B, wrong tenant) should be a no-op, not an error: %v", err)
	}
	if got, err := s.GetRun(ctxA, "shared-uuid-run"); err != nil || got.Status != models.RunStatusPending {
		t.Fatalf("tenant A's run should be untouched by tenant B's update, got status=%v err=%v", got, err)
	}
	if err := s.DeleteRun(ctxB, "shared-uuid-run"); err == nil {
		t.Fatal("tenant B deleting tenant A's run should report ErrNotFound (0 rows affected), not succeed")
	}
	if _, err := s.GetRun(ctxA, "shared-uuid-run"); err != nil {
		t.Fatalf("tenant A's run must survive tenant B's delete attempt: %v", err)
	}

	// System context bypasses tenant filtering entirely.
	if _, err := s.GetRun(tenant.SystemContext(context.Background()), "shared-uuid-run"); err != nil {
		t.Fatalf("system context should see the run regardless of tenant: %v", err)
	}
}

// TestRun_CascadeDeleteWithThread verifies the runs.thread_id FK's
// ON DELETE CASCADE: deleting a thread must delete its runs too, same
// as Postgres/SQLite (conformance's cascade_delete_thread_deletes_runs).
func TestRun_CascadeDeleteWithThread(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	seedThread(t, ctx, s, "t-cas")

	if err := s.CreateRun(ctx, &models.Run{RunID: "r-cas-1", ThreadID: "t-cas", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateRun 1: %v", err)
	}
	if err := s.CreateRun(ctx, &models.Run{RunID: "r-cas-2", ThreadID: "t-cas", Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}

	if err := s.DeleteThread(ctx, "t-cas"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}

	if _, err := s.GetRun(ctx, "r-cas-1"); err == nil {
		t.Error("r-cas-1 should have been cascade-deleted with its thread")
	}
	if _, err := s.GetRun(ctx, "r-cas-2"); err == nil {
		t.Error("r-cas-2 should have been cascade-deleted with its thread")
	}
}
