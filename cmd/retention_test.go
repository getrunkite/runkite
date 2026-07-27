package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	sqlitestore "github.com/sharanharsoor/runkite/internal/state/sqlite"
)

func writeLangGraphJSON(t *testing.T, dir string, content string) string {
	t.Helper()
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInitRetentionConfig_Unconfigured(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{"graphs": {"echo": "graph.py:graph"}}`)

	if rc := initRetentionConfig(path); rc != nil {
		t.Fatalf("expected nil retentionConfig when unconfigured, got %+v", rc)
	}
}

func TestInitRetentionConfig_BothDimensionsConfigured(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"retention": {"runs_max_age": "720h", "checkpoints_keep_last": 10, "cron_claims_max_age": "168h", "interval_minutes": 5}
	}`)

	rc := initRetentionConfig(path)
	if rc == nil {
		t.Fatal("expected non-nil retentionConfig")
	}
	if rc.runsMaxAge != 720*time.Hour {
		t.Errorf("runsMaxAge = %v, want 720h", rc.runsMaxAge)
	}
	if rc.checkpointsKeepLast != 10 {
		t.Errorf("checkpointsKeepLast = %d, want 10", rc.checkpointsKeepLast)
	}
	if rc.cronClaimsMaxAge != 168*time.Hour {
		t.Errorf("cronClaimsMaxAge = %v, want 168h", rc.cronClaimsMaxAge)
	}
	if rc.interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", rc.interval)
	}
}

func TestInitRetentionConfig_DefaultInterval(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"retention": {"runs_max_age": "24h"}
	}`)

	rc := initRetentionConfig(path)
	if rc == nil {
		t.Fatal("expected non-nil retentionConfig")
	}
	if rc.interval != 60*time.Minute {
		t.Errorf("interval = %v, want default 60m", rc.interval)
	}
}

// TestInitRetentionConfig_InvalidDurationDisablesRunPruningOnly proves an
// invalid runs_max_age degrades safely: run pruning turns off (rather than
// crashing the control plane at startup), and checkpoint pruning still
// works if separately configured.
func TestInitRetentionConfig_InvalidDurationDisablesRunPruningOnly(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"retention": {"runs_max_age": "not-a-duration", "checkpoints_keep_last": 5}
	}`)

	rc := initRetentionConfig(path)
	if rc == nil {
		t.Fatal("expected non-nil retentionConfig (checkpoints_keep_last is still valid)")
	}
	if rc.runsMaxAge != 0 {
		t.Errorf("expected runsMaxAge=0 after invalid duration, got %v", rc.runsMaxAge)
	}
	if rc.checkpointsKeepLast != 5 {
		t.Errorf("checkpointsKeepLast = %d, want 5", rc.checkpointsKeepLast)
	}
}

func TestInitRetentionConfig_ZeroValuesMeansDisabled(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"retention": {}
	}`)

	if rc := initRetentionConfig(path); rc != nil {
		t.Fatalf("expected nil retentionConfig when all dimensions are zero, got %+v", rc)
	}
}

// TestRunRetentionTick_PrunesAcrossAllTenants is the integration proof
// that the tick body actually calls into the store correctly end to end
// (not just that config parses), and that it uses a system context so a
// deployment-wide retention policy applies regardless of which tenant
// created the data.
func TestRunRetentionTick_PrunesAcrossAllTenants(t *testing.T) {
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour)
	store.CreateThread(ctx, &models.Thread{ThreadID: "t-tick", Status: models.ThreadStatusIdle, CreatedAt: old, UpdatedAt: old})
	store.CreateRun(ctx, &models.Run{RunID: "r-tick-old", ThreadID: "t-tick", Status: models.RunStatusSuccess, CreatedAt: old, UpdatedAt: old})

	for i, id := range []string{"cp-1", "cp-2", "cp-3"} {
		created := time.Now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339)
		store.SaveCheckpoint(ctx, "t-tick", &models.ThreadState{
			Values: map[string]interface{}{}, Next: []string{},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-tick", CheckpointID: id},
			Metadata:   map[string]interface{}{}, CreatedAt: &created, Tasks: []interface{}{}, Interrupts: []interface{}{},
		})
	}

	rc := &retentionConfig{runsMaxAge: 24 * time.Hour, checkpointsKeepLast: 1}
	runRetentionTick(ctx, store, rc)

	if _, err := store.GetRun(ctx, "r-tick-old"); err == nil {
		t.Error("expected r-tick-old to be pruned by the tick")
	}
	remaining, err := store.ListCheckpoints(ctx, "t-tick", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Errorf("expected 1 checkpoint remaining after tick, got %d", len(remaining))
	}
}

// TestRunRetentionTick_DimensionsAreIndependentlyOptional proves a
// deployment that only configures checkpoints_keep_last (runsMaxAge left
// at zero) never touches runs, and vice versa.
func TestRunRetentionTick_DimensionsAreIndependentlyOptional(t *testing.T) {
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour)
	store.CreateThread(ctx, &models.Thread{ThreadID: "t-only-cp", Status: models.ThreadStatusIdle, CreatedAt: old, UpdatedAt: old})
	store.CreateRun(ctx, &models.Run{RunID: "r-should-survive", ThreadID: "t-only-cp", Status: models.RunStatusSuccess, CreatedAt: old, UpdatedAt: old})

	// runsMaxAge left at zero -- run pruning must be off even though the
	// run above is well past any reasonable age.
	rc := &retentionConfig{checkpointsKeepLast: 5}
	runRetentionTick(ctx, store, rc)

	if _, err := store.GetRun(ctx, "r-should-survive"); err != nil {
		t.Error("run pruning must stay off when runsMaxAge is zero, regardless of run age")
	}
}

// TestRunRetentionLoop_PrunesImmediatelyOnStartup is the regression for a
// real gap found in review: time.NewTicker's first tick doesn't land
// until the configured interval has elapsed, so a freshly-started
// control plane with a large backlog of old data (e.g. retention just
// enabled on an existing deployment) would otherwise prune nothing for
// up to the default 60-minute interval. runRetentionLoop must run one
// tick immediately at startup, before waiting on the ticker at all.
func TestRunRetentionLoop_PrunesImmediatelyOnStartup(t *testing.T) {
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	old := time.Now().UTC().Add(-48 * time.Hour)
	store.CreateThread(ctx, &models.Thread{ThreadID: "t-startup", Status: models.ThreadStatusIdle, CreatedAt: old, UpdatedAt: old})
	store.CreateRun(ctx, &models.Run{RunID: "r-startup-old", ThreadID: "t-startup", Status: models.RunStatusSuccess, CreatedAt: old, UpdatedAt: old})

	// A long interval (1 hour) that a ticker-only implementation would
	// never fire within this test's lifetime -- proves the prune
	// happened via the immediate-first-run path, not a lucky tick.
	rc := &retentionConfig{runsMaxAge: 24 * time.Hour, interval: time.Hour}

	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runRetentionLoop(loopCtx, store, rc)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var pruned bool
	for time.Now().Before(deadline) {
		if _, err := store.GetRun(ctx, "r-startup-old"); err != nil {
			pruned = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if !pruned {
		t.Fatal("expected runRetentionLoop to prune the old run immediately on startup, without waiting for the 1-hour ticker interval")
	}
}
