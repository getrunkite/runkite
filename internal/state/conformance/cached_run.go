package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func runCachedRunTests(t *testing.T, factory StoreFactory) {
	t.Run("merged_state_result_and_exact_parent", func(t *testing.T) {
		store := factory(t)
		creator, ok := store.(state.CachedRunCreator)
		if !ok {
			t.Skip("custom store does not support atomic cache hits")
		}
		ctx := tenant.WithContext(context.Background(), "cache-owner")
		now := time.Now().UTC()
		if err := store.CreateThread(ctx, &models.Thread{ThreadID: "cache-parent", Status: models.ThreadStatusIdle, Values: map[string]interface{}{"prior": json.Number("9007199254740993")}, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		run := &models.Run{RunID: "cache-result", ThreadID: "cache-parent", AgentID: "cache-agent", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{"cache_hit": true}, Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`), Output: json.RawMessage(`{"answer":9007199254740995}`), CreatedAt: now, UpdatedAt: now}
		if err := creator.CreateCachedRun(ctx, run, nil); err != nil {
			t.Fatal(err)
		}
		stored, err := store.GetRun(ctx, run.RunID)
		if err != nil || stored.Status != models.RunStatusSuccess || len(stored.Output) == 0 {
			t.Fatalf("durable cached result: %+v %v", stored, err)
		}
		var output map[string]interface{}
		if err := json.Unmarshal(stored.Output, &output); err != nil || len(output) != 1 {
			t.Fatalf("result output gained prior fields: %s %v", stored.Output, err)
		}
		thread, err := store.GetThread(ctx, run.ThreadID)
		if err != nil || thread.Status != models.ThreadStatusIdle || len(thread.Values) != 2 {
			t.Fatalf("merged thread: %+v %v", thread, err)
		}
		checkpoint, err := store.GetLatestCheckpoint(ctx, run.ThreadID)
		if err != nil || checkpoint.Values["prior"] != json.Number("9007199254740993") || checkpoint.Values["answer"] != json.Number("9007199254740995") {
			t.Fatalf("merged checkpoint: %+v %v", checkpoint, err)
		}
		for _, other := range []struct{ tenant, status string }{{"other-owner", ""}, {"cache-owner", "busy"}} {
			if other.status == "busy" {
				if _, err := store.TryClaimThread(ctx, run.ThreadID); err != nil {
					t.Fatal(err)
				}
			}
			rejected := *run
			rejected.RunID = "reject-" + other.tenant
			err := creator.CreateCachedRun(tenant.WithContext(ctx, other.tenant), &rejected, nil)
			if err == nil {
				t.Fatalf("cached result accepted for forbidden parent %q", other.tenant)
			}
			_, err = store.GetRun(tenant.SystemContext(ctx), rejected.RunID)
			var missing *state.ErrNotFound
			if !errors.As(err, &missing) {
				t.Fatalf("rejected cached result left row: %v", err)
			}
		}
	})

	t.Run("admission_and_concurrent_merge", func(t *testing.T) {
		store := factory(t)
		creator, ok := store.(state.CachedRunCreator)
		if !ok {
			t.Skip("custom store does not support atomic cache hits")
		}
		ctx := context.Background()
		now := time.Now().UTC()
		if err := store.CreateThread(ctx, &models.Thread{ThreadID: "cache-merge", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				run := &models.Run{RunID: fmt.Sprintf("merge-%d", i), ThreadID: "cache-merge", AgentID: "cache-agent", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{"cache_hit": true}, Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`), Output: json.RawMessage(fmt.Sprintf(`{"key_%d":%d}`, i, i)), CreatedAt: now, UpdatedAt: now}
				if err := creator.CreateCachedRun(ctx, run, &state.RunAdmissionCaps{TenantDaily: 8, Now: now}); err != nil {
					t.Errorf("concurrent cached result %d: %v", i, err)
				}
			}(i)
		}
		wg.Wait()
		thread, err := store.GetThread(ctx, "cache-merge")
		if err != nil || len(thread.Values) != 8 || thread.Status != models.ThreadStatusIdle {
			t.Fatalf("cache merge lost writes: %+v %v", thread, err)
		}
		checkpoint, err := store.GetLatestCheckpoint(ctx, "cache-merge")
		if err != nil || len(checkpoint.Values) != 8 {
			t.Fatalf("cache checkpoint lost writes: %+v %v", checkpoint, err)
		}
		rejected := &models.Run{RunID: "cache-over-quota", ThreadID: "cache-merge", AgentID: "cache-agent", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`), Output: json.RawMessage(`{"over":true}`), CreatedAt: now, UpdatedAt: now}
		err = creator.CreateCachedRun(ctx, rejected, &state.RunAdmissionCaps{TenantDaily: 8, Now: now})
		var exceeded *state.ErrAdmissionLimitExceeded
		if !errors.As(err, &exceeded) {
			t.Fatalf("cached result bypassed daily admission: %v", err)
		}
		if _, err := store.GetRun(ctx, rejected.RunID); err == nil {
			t.Fatal("over-quota cached result was persisted")
		}
	})
}
