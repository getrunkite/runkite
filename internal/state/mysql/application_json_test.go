package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlcompletion"
)

type nativeJSONRow struct {
	raw []byte
	err error
}

func (r nativeJSONRow) Scan(values ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	*values[0].(*[]byte) = r.raw
	return nil
}

func TestNativeApplicationJSONValidation(t *testing.T) {
	for _, test := range []struct {
		name, original, native string
		accepted               bool
	}{
		{"equivalent representations", `{"id":9007199254740993,"exponent":1e2}`, `{"exponent":100.0,"id":9007199254740993}`, true},
		{"integer rounding", `{"id":9007199254740993}`, `{"id":9007199254740992}`, false},
		{"decimal rounding", `{"value":0.123456789123456789}`, `{"value":0.12345678912345678}`, false},
		{"nested rounding", `{"items":[{"value":123456789012345678901234567890}]}`, `{"items":[{"value":1.2345678901234568e29}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			row := func(context.Context, string, ...interface{}) sqlcompletion.Row {
				return nativeJSONRow{raw: []byte(test.native)}
			}
			err := validateNativeApplicationJSON(context.Background(), row, []byte(test.original))
			if (err == nil) != test.accepted {
				t.Fatalf("validation accepted=%v err=%v", test.accepted, err)
			}
		})
	}
	called := false
	row := func(context.Context, string, ...interface{}) sqlcompletion.Row {
		called = true
		return nativeJSONRow{err: errors.New("synthetic database error")}
	}
	if err := validateNativeApplicationJSON(context.Background(), row, []byte(`{"id":1} {}`)); err == nil || called {
		t.Fatal("invalid JSON reached the database")
	}
	if err := validateNativeApplicationJSON(context.Background(), row, []byte(`{"id":1}`)); err == nil {
		t.Fatal("native conversion error was ignored")
	}
}

func numericMySQLStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN is required for native JSON validation checks")
	}
	store, err := New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestMySQLApplicationJSONNativePrecision(t *testing.T) {
	store := numericMySQLStore(t)
	ctx := context.Background()
	for _, raw := range []string{
		`{"id":9007199254740993,"negative":-9223372036854775808,"unsigned":18446744073709551615,"fraction":0.125}`,
		`{"array":[1e2,0.5,{"id":9007199254740993}]}`,
	} {
		if err := store.validateApplicationJSON(ctx, []byte(raw)); err != nil {
			t.Fatalf("native exact value rejected: %v", err)
		}
	}
	for _, raw := range []string{
		`{"id":12345678901234567890123456789012345}`,
		`{"fraction":0.123456789123456789}`,
		`{"number":1e10000}`,
	} {
		if err := store.validateApplicationJSON(ctx, []byte(raw)); err == nil {
			t.Fatal("native lossy value silently accepted")
		}
	}
}

func TestMySQLUnsupportedApplicationNumberLeavesRunRecoverable(t *testing.T) {
	store := numericMySQLStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	threadID, runID := "numbers-thread-"+suffix, "numbers-run-"+suffix
	now := time.Now().UTC()
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "numbers-agent", Name: "numbers-agent"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: threadID, Status: models.ThreadStatusBusy, Values: map[string]interface{}{"retained": json.Number("9007199254740993")}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DeleteThread(context.Background(), threadID) })
	if err := store.CreateRun(ctx, &models.Run{RunID: runID, ThreadID: threadID, AgentID: "numbers-agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if accepted, err := store.BeginRunAttempt(ctx, runID, 1); err != nil || !accepted {
		t.Fatalf("begin attempt: %v %v", accepted, err)
	}
	values := map[string]interface{}{"result": json.Number("12345678901234567890123456789012345")}
	if _, err := store.CompleteRun(ctx, &state.RunCompletion{RunID: runID, Generation: 1, Status: models.RunStatusSuccess, Values: values}); err == nil {
		t.Fatal("unsupported completion was accepted")
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil || run.Status != models.RunStatusRunning || run.Output != nil {
		t.Fatalf("rejected completion changed run: %+v %v", run, err)
	}
	thread, err := store.GetThread(ctx, threadID)
	if err != nil || len(thread.Values) != 1 || !jsonvalue.Equal(thread.Values["retained"], json.Number("9007199254740993")) {
		t.Fatalf("rejected completion changed thread: %+v %v", thread, err)
	}
	if _, err := store.GetLatestCheckpoint(ctx, threadID); err == nil {
		t.Fatal("rejected completion wrote a checkpoint")
	}
	cacheKey := "numbers-cache-" + suffix
	if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{CacheKey: cacheKey, AgentID: "numbers-agent", Output: values, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err == nil {
		t.Fatal("unsupported cache output was accepted")
	}
	if _, err := store.GetCachedRunResult(ctx, cacheKey); err == nil {
		t.Fatal("unsupported cache output was written")
	}
	itemKey := "numbers-item-" + suffix
	if err := store.PutItem(ctx, &models.StoreItem{Namespace: []string{"numbers"}, Key: itemKey, Value: values}); err == nil {
		t.Fatal("unsupported store item was accepted")
	}
	if _, err := store.GetItem(ctx, []string{"numbers"}, itemKey, false); err == nil {
		t.Fatal("unsupported store item was written")
	}
}

func TestMySQLAdmittedRunPreservesNumbersAndRejectsUnsupportedInput(t *testing.T) {
	store := numericMySQLStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	threadID := "numbers-admission-thread-" + suffix
	agentID := "numbers-admission-agent-" + suffix
	now := time.Now().UTC()
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: agentID, Name: agentID}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: threadID, Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.DeleteThread(context.Background(), threadID) })
	run := &models.Run{
		RunID: "numbers-admission-run-" + suffix, ThreadID: threadID, AgentID: agentID, Status: models.RunStatusPending,
		Input: json.RawMessage(`{"id":9007199254740993}`), Config: json.RawMessage(`{"id":18446744073709551615,"fraction":0.125}`),
		CreatedAt: now, UpdatedAt: now,
	}
	caps := &state.RunAdmissionCaps{AgentConcurrent: 10}
	if err := store.CreateRunAdmitted(ctx, run, caps); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetRun(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(raw json.RawMessage) interface{} {
		var value interface{}
		if err := jsonvalue.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !jsonvalue.Equal(decode(run.Input), decode(saved.Input)) || !jsonvalue.Equal(decode(run.Config), decode(saved.Config)) {
		t.Fatalf("admission changed application data: input=%s config=%s", saved.Input, saved.Config)
	}
	for _, field := range []string{"input", "config"} {
		invalid := *run
		invalid.RunID = "numbers-invalid-" + field + "-" + suffix
		if field == "input" {
			invalid.Input = json.RawMessage(`{"id":12345678901234567890123456789012345}`)
		} else {
			invalid.Config = json.RawMessage(`{"id":12345678901234567890123456789012345}`)
		}
		if err := store.CreateRunAdmitted(ctx, &invalid, caps); err == nil {
			t.Fatalf("admission accepted unsupported %s", field)
		}
		if _, err := store.GetRun(ctx, invalid.RunID); err == nil {
			t.Fatalf("admission wrote unsupported %s", field)
		}
	}
}
