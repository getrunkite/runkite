package sqlite

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func seedCompletion(t *testing.T) (*SQLiteStore, context.Context) {
	t.Helper()
	ctx := context.Background()
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, Values: map[string]interface{}{"old": "kept"}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return s, ctx
}

func TestCompletionTransactionRollsBackAllResultWrites(t *testing.T) {
	s, ctx := seedCompletion(t)
	if ok, err := s.BeginRunAttempt(ctx, "run", 1); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_, err := s.db.Exec(`CREATE TRIGGER fail_completion BEFORE UPDATE OF status ON runs WHEN NEW.status = 'success' BEGIN SELECT RAISE(ABORT, 'injected completion failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	c := &state.RunCompletion{RunID: "run", Generation: 1, Status: models.RunStatusSuccess, Values: map[string]interface{}{"answer": "winner"}}
	if _, err := s.CompleteRun(ctx, c); err == nil {
		t.Fatal("expected injected write failure")
	}
	run, _ := s.GetRun(ctx, "run")
	thread, _ := s.GetThread(ctx, "thread")
	history, err := s.ListCheckpoints(ctx, "thread", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.RunStatusPending || thread.Status != models.ThreadStatusBusy || thread.Values["answer"] != nil || len(history) != 0 {
		t.Fatalf("partial completion: run=%+v thread=%+v history=%d", run, thread, len(history))
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_completion`); err != nil {
		t.Fatal(err)
	}
	result, err := s.CompleteRun(ctx, c)
	if err != nil || !result.Applied {
		t.Fatal(result, err)
	}
	result, err = s.CompleteRun(ctx, c)
	if err != nil || !result.Accepted || result.Applied {
		t.Fatal("retry must be idempotent", result, err)
	}
	history, _ = s.ListCheckpoints(ctx, "thread", 10, "")
	thread, _ = s.GetThread(ctx, "thread")
	run, _ = s.GetRun(ctx, "run")
	if len(history) != 1 || thread.Values["old"] != "kept" || thread.Values["answer"] != "winner" || thread.Status != models.ThreadStatusIdle || run.Status != models.RunStatusSuccess {
		t.Fatalf("completion mismatch: run=%+v thread=%+v history=%d", run, thread, len(history))
	}
}

func TestCompletionGenerationIsMonotonicAndDoesNotReopenTerminal(t *testing.T) {
	s, ctx := seedCompletion(t)
	var wg sync.WaitGroup
	for gen := int64(1); gen <= 10; gen++ {
		wg.Add(1)
		go func(g int64) {
			defer wg.Done()
			if _, err := s.BeginRunAttempt(ctx, "run", g); err != nil {
				t.Error(err)
			}
		}(gen)
	}
	wg.Wait()
	run, _ := s.GetRun(ctx, "run")
	if got := state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey); got != 10 {
		t.Fatalf("generation=%d", got)
	}
	stale, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Generation: 9, Status: models.RunStatusSuccess, Values: map[string]interface{}{"answer": "stale"}})
	if err != nil || stale.Accepted {
		t.Fatal(stale, err)
	}
	if err := s.UpdateRunStatus(ctx, "run", models.RunStatusTimeout, nil, "timed out"); err != nil {
		t.Fatal(err)
	}
	result, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Generation: 10, Status: models.RunStatusSuccess})
	if err != nil || result.Accepted {
		t.Fatal(result, err)
	}
	if ok, err := s.BeginRunAttempt(ctx, "run", 11); err != nil || ok {
		t.Fatal("terminal dispatch reopened", ok, err)
	}
	run, _ = s.GetRun(ctx, "run")
	if run.Status != models.RunStatusTimeout {
		t.Fatal(run.Status)
	}
}
