package main

import (
	"context"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/api"
	"github.com/sharanharsoor/runkite/internal/models"
	sqlitestore "github.com/sharanharsoor/runkite/internal/state/sqlite"
	"github.com/sharanharsoor/runkite/internal/transport/inprocess"
)

func TestInitRunTimeoutConfig_Unconfigured(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{"graphs": {"echo": "graph.py:graph"}}`)
	if rc := initRunTimeoutConfig(path); rc != nil {
		t.Fatalf("expected nil runTimeoutConfig when unconfigured, got %+v", rc)
	}
}

func TestInitRunTimeoutConfig_Configured(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"run_timeout": {"max_duration": "30m", "interval_seconds": 5}
	}`)

	rc := initRunTimeoutConfig(path)
	if rc == nil {
		t.Fatal("expected non-nil runTimeoutConfig")
	}
	if rc.maxDuration != 30*time.Minute {
		t.Errorf("maxDuration = %v, want 30m", rc.maxDuration)
	}
	if rc.interval != 5*time.Second {
		t.Errorf("interval = %v, want 5s", rc.interval)
	}
}

func TestInitRunTimeoutConfig_DefaultInterval(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"run_timeout": {"max_duration": "1h"}
	}`)

	rc := initRunTimeoutConfig(path)
	if rc == nil {
		t.Fatal("expected non-nil runTimeoutConfig")
	}
	if rc.interval != 15*time.Second {
		t.Errorf("interval = %v, want default 15s", rc.interval)
	}
}

func TestInitRunTimeoutConfig_InvalidDurationDisables(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"run_timeout": {"max_duration": "not-a-duration"}
	}`)

	if rc := initRunTimeoutConfig(path); rc != nil {
		t.Fatalf("expected nil after invalid max_duration, got %+v", rc)
	}
}

func TestInitRunTimeoutConfig_EmptySectionDisables(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"run_timeout": {}
	}`)

	if rc := initRunTimeoutConfig(path); rc != nil {
		t.Fatalf("expected nil when max_duration absent, got %+v", rc)
	}
}

func TestRunTimeoutTick_TimesOutOverdueRun(t *testing.T) {
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	ctx := context.Background()
	now := time.Now().UTC()

	if err := store.CreateThread(ctx, &models.Thread{
		ThreadID: "t-tick", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	if err := store.CreateRun(ctx, &models.Run{
		RunID: "r-tick", ThreadID: "t-tick", AgentID: "echo",
		Status: models.RunStatusRunning, CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	rc := &runTimeoutConfig{maxDuration: time.Hour, interval: time.Hour}
	runTimeoutTick(ctx, apiServer, rc)

	run, err := store.GetRun(ctx, "r-tick")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.RunStatusTimeout {
		t.Fatalf("status = %q, want timeout", run.Status)
	}
}

func TestRunTimeoutLoop_TicksImmediatelyOnStartup(t *testing.T) {
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	ctx := context.Background()
	now := time.Now().UTC()

	if err := store.CreateThread(ctx, &models.Thread{
		ThreadID: "t-immediate", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	if err := store.CreateRun(ctx, &models.Run{
		RunID: "r-immediate", ThreadID: "t-immediate", AgentID: "echo",
		Status: models.RunStatusPending, CreatedAt: old, UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	loopCtx, cancel := context.WithCancel(ctx)
	rc := &runTimeoutConfig{maxDuration: time.Hour, interval: time.Hour}
	done := make(chan struct{})
	go func() {
		runTimeoutLoop(loopCtx, apiServer, rc)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		run, err := store.GetRun(ctx, "r-immediate")
		if err == nil && run.Status == models.RunStatusTimeout {
			cancel()
			<-done
			return
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("expected runTimeoutLoop to timeout the overdue run immediately on startup")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
