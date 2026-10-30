package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

const jsonNumbers = `{"id":9007199254740993,"negative":-9007199254740993,"max_int64":9223372036854775807,"min_int64":-9223372036854775808,"max_uint64":18446744073709551615,"decimal":0.125,"exponent":1e20,"nested":[{"id":9007199254740993},null,false,0]}`

const exactJSONNumbers = `{"integer":1234567890123456789012345678901234567890,"decimal":0.12345678901234567890123456789,"exponent":1.2345678901234567890123456789e100,"tiny":1.234567890123456789e-100,"nested":[9007199254740993,false,null,0]}`

// RunExactJSONNumberTests covers the backends whose JSON storage preserves
// arbitrary decimal precision. Native MySQL JSON and BSON have their own
// representable ranges and must reject an unsupported value before writing.
func RunExactJSONNumberTests(t *testing.T, factory StoreFactory) {
	t.Helper()
	runJSONNumberTests(t, factory, exactJSONNumbers)
}

func numberValues(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var values map[string]interface{}
	if err := jsonvalue.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatal(err)
	}
	return values
}

// Compare numeric meaning rather than JSON spelling: Postgres JSONB can expand
// an exponent while still retaining every digit. Re-marshaling catches native
// values that would be emitted to an API as strings or rounded numbers.
func assertJSONValue(t *testing.T, actual, expected interface{}) {
	t.Helper()
	decode := func(value interface{}) interface{} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var parsed interface{}
		if err := jsonvalue.Unmarshal(raw, &parsed); err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	var compare func(string, interface{}, interface{})
	compare = func(path string, actual, expected interface{}) {
		switch expected := expected.(type) {
		case json.Number:
			got, ok := actual.(json.Number)
			if !ok {
				t.Fatalf("%s: number became %T (%v)", path, actual, actual)
			}
			wantNumber, wantOK := new(big.Rat).SetString(string(expected))
			gotNumber, gotOK := new(big.Rat).SetString(string(got))
			if !wantOK || !gotOK || wantNumber.Cmp(gotNumber) != 0 {
				t.Fatalf("%s: got %s, want %s", path, got, expected)
			}
		case map[string]interface{}:
			got, ok := actual.(map[string]interface{})
			if !ok || len(got) != len(expected) {
				t.Fatalf("%s: object mismatch: got %#v, want %#v", path, actual, expected)
			}
			for key, value := range expected {
				child, ok := got[key]
				if !ok {
					t.Fatalf("%s: missing key %q", path, key)
				}
				compare(path+"."+key, child, value)
			}
		case []interface{}:
			got, ok := actual.([]interface{})
			if !ok || len(got) != len(expected) {
				t.Fatalf("%s: array mismatch: got %#v, want %#v", path, actual, expected)
			}
			for index, value := range expected {
				compare(fmt.Sprintf("%s[%d]", path, index), got[index], value)
			}
		default:
			if actual != expected {
				t.Fatalf("%s: got %#v, want %#v", path, actual, expected)
			}
		}
	}
	compare("$", decode(actual), decode(expected))
}

func runJSONNumberTests(t *testing.T, factory StoreFactory, raw string) {
	t.Run("thread_read_search_and_update", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		values := numberValues(t, raw)
		if err := s.CreateThread(ctx, &models.Thread{ThreadID: "numbers-thread", Status: models.ThreadStatusIdle, Values: values, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		thread, err := s.GetThread(ctx, "numbers-thread")
		if err != nil {
			t.Fatal(err)
		}
		assertJSONValue(t, thread.Values, values)
		if _, err := s.UpdateThread(ctx, "numbers-thread", &models.ThreadPatch{Metadata: map[string]interface{}{"changed": true}}); err != nil {
			t.Fatal(err)
		}
		threads, err := s.SearchThreads(ctx, &models.ThreadSearchRequest{Limit: 10})
		if err != nil || len(threads) != 1 {
			t.Fatalf("search: count=%d err=%v", len(threads), err)
		}
		assertJSONValue(t, threads[0].Values, values)
	})
	t.Run("completion_preserves_unrelated_values_and_checkpoints", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		values := numberValues(t, raw)
		if err := s.CreateThread(ctx, &models.Thread{ThreadID: "numbers-thread", Status: models.ThreadStatusBusy, Values: map[string]interface{}{"previous": values}, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		for index := 1; index <= 2; index++ {
			runID := fmt.Sprintf("numbers-run-%d", index)
			if err := s.CreateRun(ctx, &models.Run{RunID: runID, ThreadID: "numbers-thread", AgentID: "agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			if accepted, err := s.BeginRunAttempt(ctx, runID, 1); err != nil || !accepted {
				t.Fatal(accepted, err)
			}
			completion := map[string]interface{}{fmt.Sprintf("result_%d", index): values}
			if result, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: runID, Generation: 1, Status: models.RunStatusSuccess, Values: completion}); err != nil || !result.Applied {
				t.Fatal(result, err)
			}
			run, err := s.GetRun(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			assertJSONValue(t, run.Output, completion)
		}
		thread, err := s.GetThread(ctx, "numbers-thread")
		if err != nil {
			t.Fatal(err)
		}
		assertJSONValue(t, thread.Values, map[string]interface{}{"previous": values, "result_1": values, "result_2": values})
		history, err := s.ListCheckpoints(ctx, "numbers-thread", 10, "")
		if err != nil || len(history) != 2 {
			t.Fatalf("history: count=%d err=%v", len(history), err)
		}
		for _, checkpoint := range history {
			var resultKey string
			for key := range checkpoint.Values {
				resultKey = key
			}
			if resultKey != "result_1" && resultKey != "result_2" {
				t.Fatalf("unexpected completion checkpoint: %#v", checkpoint.Values)
			}
			assertJSONValue(t, checkpoint.Values, map[string]interface{}{resultKey: values})
		}
	})
	t.Run("checkpoint_values_tasks_and_interrupts", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		values := numberValues(t, raw)
		now := time.Now().UTC()
		if err := s.CreateThread(ctx, &models.Thread{ThreadID: "numbers-thread", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveCheckpoint(ctx, "numbers-thread", &models.ThreadState{
			Checkpoint: models.ThreadCheckpoint{CheckpointID: "numbers-checkpoint", ThreadID: "numbers-thread"},
			Values:     values, Tasks: []interface{}{values}, Interrupts: []interface{}{values},
		}); err != nil {
			t.Fatal(err)
		}
		checkpoint, err := s.GetLatestCheckpoint(ctx, "numbers-thread")
		if err != nil {
			t.Fatal(err)
		}
		assertJSONValue(t, checkpoint.Values, values)
		assertJSONValue(t, checkpoint.Tasks, []interface{}{values})
		assertJSONValue(t, checkpoint.Interrupts, []interface{}{values})
		history, err := s.ListCheckpoints(ctx, "numbers-thread", 10, "")
		if err != nil || len(history) != 1 {
			t.Fatalf("history: count=%d err=%v", len(history), err)
		}
		assertJSONValue(t, history[0].Values, values)
		assertJSONValue(t, history[0].Tasks, []interface{}{values})
		assertJSONValue(t, history[0].Interrupts, []interface{}{values})
	})
	t.Run("cache_and_store_item_reads", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		values := numberValues(t, raw)
		if err := s.SaveCachedRunResult(ctx, &models.CachedRunResult{CacheKey: "numbers-cache", AgentID: "agent", Output: values, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		cached, err := s.GetCachedRunResult(ctx, "numbers-cache")
		if err != nil {
			t.Fatal(err)
		}
		assertJSONValue(t, cached.Output, values)
		if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"numbers"}, Key: "item", Value: values}); err != nil {
			t.Fatal(err)
		}
		item, err := s.GetItem(ctx, []string{"numbers"}, "item", false)
		if err != nil {
			t.Fatal(err)
		}
		assertJSONValue(t, item.Value, values)
		items, err := s.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"numbers"}, Limit: 10})
		if err != nil || len(items) != 1 {
			t.Fatalf("search: count=%d err=%v", len(items), err)
		}
		assertJSONValue(t, items[0].Value, values)
	})
}
