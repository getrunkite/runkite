package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

func TestCorruptApplicationJSONIsRejected(t *testing.T) {
	for _, column := range []string{"thread", "checkpoint_values", "checkpoint_tasks", "checkpoint_interrupts", "item", "cache"} {
		t.Run(column, func(t *testing.T) {
			s, err := New("")
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if err := s.Init(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			now := time.Now().UTC()
			if err := s.CreateThread(ctx, &models.Thread{ThreadID: "corrupt", Status: models.ThreadStatusIdle, Values: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			invalid := `{"id":9007199254740993} {"extra":1}`
			assertError := func(err error) {
				t.Helper()
				if err == nil {
					t.Fatal("corrupt application JSON was silently accepted")
				}
			}
			var query string
			switch column {
			case "thread":
				query = `UPDATE threads SET values_json = ? WHERE thread_id = 'corrupt'`
			case "checkpoint_values", "checkpoint_tasks", "checkpoint_interrupts":
				if err := s.SaveCheckpoint(ctx, "corrupt", &models.ThreadState{Checkpoint: models.ThreadCheckpoint{CheckpointID: "cp"}, Values: map[string]interface{}{}, Tasks: []interface{}{}, Interrupts: []interface{}{}}); err != nil {
					t.Fatal(err)
				}
				field := map[string]string{"checkpoint_values": "values_json", "checkpoint_tasks": "tasks", "checkpoint_interrupts": "interrupts"}[column]
				query = `UPDATE thread_checkpoints SET ` + field + ` = ? WHERE checkpoint_id = 'cp'`
			case "item":
				if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"corrupt"}, Key: "item", Value: map[string]interface{}{}}); err != nil {
					t.Fatal(err)
				}
				query = `UPDATE store_items SET value = ? WHERE key = 'item'`
			case "cache":
				if err := s.SaveCachedRunResult(ctx, &models.CachedRunResult{CacheKey: "corrupt", AgentID: "agent", Output: map[string]interface{}{}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				query = `UPDATE run_cache SET output = ? WHERE cache_key = 'corrupt'`
			}
			if _, err := s.db.ExecContext(ctx, query, invalid); err != nil {
				t.Fatal(err)
			}
			switch column {
			case "thread":
				_, err := s.GetThread(ctx, "corrupt")
				assertError(err)
				_, err = s.SearchThreads(ctx, &models.ThreadSearchRequest{})
				assertError(err)
				_, err = s.UpdateThread(ctx, "corrupt", &models.ThreadPatch{Metadata: map[string]interface{}{"changed": true}})
				assertError(err)
			case "checkpoint_values", "checkpoint_tasks", "checkpoint_interrupts":
				_, err := s.GetLatestCheckpoint(ctx, "corrupt")
				assertError(err)
				_, err = s.ListCheckpoints(ctx, "corrupt", 10, "")
				assertError(err)
			case "item":
				_, err := s.GetItem(ctx, []string{"corrupt"}, "item", false)
				assertError(err)
				_, err = s.SearchItems(ctx, &models.StoreSearchRequest{})
				assertError(err)
			case "cache":
				_, err := s.GetCachedRunResult(ctx, "corrupt")
				assertError(err)
			}
		})
	}
}
