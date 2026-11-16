package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

func TestSearchRunsRejectsPartialIterationFailure(t *testing.T) {
	ctx := context.Background()
	store, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "history", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"broken", "first"} {
		created := now.Add(time.Duration(i) * time.Second)
		if err := store.CreateRun(ctx, &models.Run{RunID: id, ThreadID: "history", AgentID: "agent", Status: models.RunStatusSuccess, CreatedAt: created, UpdatedAt: created}); err != nil {
			t.Fatal(err)
		}
	}
	// A real SQLite expression failure occurs only on the second ordered row.
	// The covering order avoids evaluating every projection in a temporary sort.
	_, err = store.db.ExecContext(ctx, `ALTER TABLE runs RENAME TO retained_runs;
 CREATE INDEX history_iteration_order ON retained_runs(tenant_id,thread_id,created_at DESC,run_id DESC);
 CREATE VIEW runs AS SELECT tenant_id,run_id,thread_id,agent_id,status,
 CASE WHEN run_id='broken' THEN json_extract('invalid-json','$') ELSE metadata END AS metadata,
 input,config,output,error_msg,created_at,updated_at,parent_run_id,root_run_id,depth,incarnation_id FROM retained_runs;`)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := store.db.QueryContext(ctx, `SELECT metadata FROM runs WHERE tenant_id='default' AND thread_id='history' ORDER BY created_at DESC,run_id DESC`)
	if err != nil {
		t.Fatal("fixture failed before row iteration", err)
	}
	if !probe.Next() {
		t.Fatal("fixture did not produce a valid first row", probe.Err())
	}
	if probe.Next() || probe.Err() == nil {
		t.Fatal("fixture did not produce a second-row driver error")
	}
	_ = probe.Close()
	rows, err := store.SearchRuns(ctx, &models.RunSearchRequest{ThreadID: "history", Limit: 10})
	if err == nil || len(rows) != 0 {
		t.Fatalf("partial history was reported as success: rows=%d error=%v", len(rows), err)
	}
}
