package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestTimeoutOverdueRuns_MarksOldActiveRuns(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := env.store.CreateThread(ctx, &models.Thread{
		ThreadID: "t-timeout", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	old := now.Add(-2 * time.Hour)
	if err := env.store.CreateRun(ctx, &models.Run{
		RunID: "r-old", ThreadID: "t-timeout", AgentID: "echo",
		Status: models.RunStatusRunning, CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateRun(ctx, &models.Run{
		RunID: "r-fresh", ThreadID: "t-timeout", AgentID: "echo",
		Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := env.apiServer.TimeoutOverdueRuns(ctx, time.Hour, 100)
	if err != nil {
		t.Fatalf("TimeoutOverdueRuns: %v", err)
	}
	if n != 1 {
		t.Fatalf("timed out count = %d, want 1", n)
	}

	oldRun, err := env.store.GetRun(ctx, "r-old")
	if err != nil {
		t.Fatal(err)
	}
	if oldRun.Status != models.RunStatusTimeout {
		t.Errorf("r-old status = %q, want timeout", oldRun.Status)
	}
	if oldRun.Error != "run exceeded max_duration" {
		t.Errorf("r-old error = %q", oldRun.Error)
	}

	fresh, err := env.store.GetRun(ctx, "r-fresh")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != models.RunStatusPending {
		t.Errorf("r-fresh status = %q, want pending (still within max_duration)", fresh.Status)
	}

	// Fresh run still active on the thread -- must NOT idle it.
	th, err := env.store.GetThread(ctx, "t-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if th.Status != models.ThreadStatusBusy {
		t.Errorf("thread status = %q, want busy (other active run remains)", th.Status)
	}
}

func TestTimeoutOverdueRuns_IdlesThreadWhenNoOtherActive(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := env.store.CreateThread(ctx, &models.Thread{
		ThreadID: "t-solo", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	if err := env.store.CreateRun(ctx, &models.Run{
		RunID: "r-solo", ThreadID: "t-solo", AgentID: "echo",
		Status: models.RunStatusPending, CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := env.apiServer.TimeoutOverdueRuns(ctx, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("timed out count = %d, want 1", n)
	}
	th, err := env.store.GetThread(ctx, "t-solo")
	if err != nil {
		t.Fatal(err)
	}
	if th.Status != models.ThreadStatusIdle {
		t.Errorf("thread status = %q, want idle", th.Status)
	}
}

func TestTimeoutOverdueRuns_WinsOnceAcrossReplicas(t *testing.T) {
	env := newTestEnv(t)
	ctx := tenant.SystemContext(context.Background())
	now := time.Now().UTC()

	if err := env.store.CreateThread(ctx, &models.Thread{
		ThreadID: "t-race", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	if err := env.store.CreateRun(ctx, &models.Run{
		RunID: "r-race", ThreadID: "t-race", AgentID: "echo",
		Status: models.RunStatusRunning, CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	n1, err := env.apiServer.TimeoutOverdueRuns(ctx, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	n2, err := env.apiServer.TimeoutOverdueRuns(ctx, time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 1 || n2 != 0 {
		t.Fatalf("expected first tick to win (1) and second to no-op (0), got %d then %d", n1, n2)
	}
}
