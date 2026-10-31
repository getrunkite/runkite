package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
	natstransport "github.com/getrunkite/runkite/internal/transport/nats"
)

func cachedCompletionFixture(t *testing.T) (*Server, *sqlite.SQLiteStore, *sql.DB, *models.RunCreate) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	inject, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inject.Close() })
	now := time.Now().UTC()
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "cached", Name: "cached", Metadata: map[string]interface{}{"cache_ttl_seconds": 3600}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateThread(ctx, &models.Thread{ThreadID: "cache-state", Status: models.ThreadStatusIdle, Values: map[string]interface{}{"prior_id": json.Number("9007199254740993")}, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	req := &models.RunCreate{RunID: "cached-result", AgentID: "cached", Input: json.RawMessage(`{"q":"cached"}`)}
	if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{CacheKey: computeCacheKey(tenant.FromContext(ctx), req.AgentID, req.Input, req.Config), AgentID: req.AgentID, Output: map[string]interface{}{"answer": "cached"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus()), store, inject, req
}

type cacheHookSink struct {
	mu     sync.Mutex
	events []hooks.Event
}

func (s *cacheHookSink) Handle(_ context.Context, event hooks.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func TestCachedCompletionDoesNotRebillOrRefreshSource(t *testing.T) {
	s, store, _, req := cachedCompletionFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	expires := now.Add(20 * time.Minute)
	key := computeCacheKey(tenant.FromContext(ctx), req.AgentID, req.Input, req.Config)
	if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{CacheKey: key, AgentID: req.AgentID, Output: map[string]interface{}{"answer": "cached", "usage": map[string]interface{}{"prompt_tokens": 20, "completion_tokens": 4, "model": "original"}}, CreatedAt: now, ExpiresAt: expires}); err != nil {
		t.Fatal(err)
	}
	originalSource, err := store.GetCachedRunResult(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	sink := &cacheHookSink{}
	dispatcher := hooks.NewDispatcher()
	t.Cleanup(func() {
		if dispatcher != nil {
			_ = dispatcher.Close(ctx)
		}
	})
	dispatcher.Register(sink, hooks.RunComplete)
	s.SetHookDispatcher(dispatcher)
	run, assignment, err := s.createRunCtx(ctx, "cache-state", req)
	if err != nil || run == nil || assignment != nil {
		t.Fatalf("cached create: %+v %+v %v", run, assignment, err)
	}
	// Completion and HTTP retries may land on another plane replica.
	replica := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	replica.SetHookDispatcher(dispatcher)
	if accepted, err := replica.CompleteRun(ctx, &state.RunCompletion{RunID: run.RunID, Generation: 0, Status: models.RunStatusSuccess, ReplayOnly: true}); err != nil || !accepted {
		t.Fatalf("replayed cache finalization: accepted=%v err=%v", accepted, err)
	}
	if retry, err := replica.findRunForRetry(ctx, req.RunID, req, "cache-state"); err != nil || retry == nil {
		t.Fatalf("cached HTTP retry: %+v %v", retry, err)
	}
	in, out, usd, err := store.SumUsage(ctx, tenant.FromContext(ctx), "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil || in != 0 || out != 0 || usd != 0 {
		t.Fatalf("cached output billed original model usage again: in=%d out=%d usd=%v err=%v", in, out, usd, err)
	}
	source, err := store.GetCachedRunResult(ctx, key)
	if err != nil || !source.ExpiresAt.Equal(originalSource.ExpiresAt) || !source.CreatedAt.Equal(originalSource.CreatedAt) {
		t.Fatalf("cache read refreshed source TTL: %+v %v", source, err)
	}
	if err := dispatcher.Close(ctx); err != nil {
		t.Fatal(err)
	}
	dispatcher = nil
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.events) != 1 || sink.events[0].Data["cache_hit"] != true {
		t.Fatalf("cache terminal hook was repeated or lost its cache marker: %+v", sink.events)
	}
}

type cacheStoreWithoutAtomic struct{ state.Store }

func TestCachedCompletionCustomStoreWithoutAtomicBoundaryDispatches(t *testing.T) {
	s, store, _, req := cachedCompletionFixture(t)
	s.store = cacheStoreWithoutAtomic{store}
	run, assignment, err := s.createRunCtx(context.Background(), "cache-state", req)
	if err != nil || run == nil || assignment == nil || run.Status != models.RunStatusPending || run.Metadata["cache_hit"] == true {
		t.Fatalf("custom store without cache transaction bypassed dispatch: run=%+v assignment=%+v err=%v", run, assignment, err)
	}
}

type concurrentCachedStore struct {
	*sqlite.SQLiteStore
	arrivals sync.WaitGroup
}

func (s *concurrentCachedStore) CreateCachedRun(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	s.arrivals.Done()
	s.arrivals.Wait()
	return s.SQLiteStore.CreateCachedRun(ctx, run, caps)
}

func TestCachedCompletionConcurrentRetryReusesQuotaWinner(t *testing.T) {
	s, store, _, req := cachedCompletionFixture(t)
	barrier := &concurrentCachedStore{SQLiteStore: store}
	barrier.arrivals.Add(2)
	s.store = barrier
	s.admissionLimits = &AdmissionLimits{TenantDaily: 1}
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			request := *req
			run, assignment, err := s.createRunCtx(context.Background(), "cache-state", &request)
			if err != nil {
				var retry *errRunRetryRace
				if errors.As(err, &retry) {
					run = retry.run
				} else {
					t.Errorf("matching cached retry did not recover its winner: %v", err)
					return
				}
			}
			if run == nil || assignment != nil || run.RunID != req.RunID || run.Status != models.RunStatusSuccess {
				t.Errorf("matching cached retry: run=%+v assignment=%+v", run, assignment)
			}
		}()
	}
	wg.Wait()
	runs, err := store.SearchRuns(context.Background(), &models.RunSearchRequest{ThreadID: "cache-state", Limit: 10})
	if err != nil || len(runs) != 1 {
		t.Fatalf("matching retries consumed multiple daily slots: runs=%d err=%v", len(runs), err)
	}
}

func TestCachedCompletionPreservesMergedCheckpoint(t *testing.T) {
	s, store, _, req := cachedCompletionFixture(t)
	run, assignment, err := s.createRunCtx(context.Background(), "cache-state", req)
	if err != nil || run == nil || assignment != nil || run.Status != models.RunStatusSuccess {
		t.Fatalf("cache result: run=%+v assignment=%+v err=%v", run, assignment, err)
	}
	checkpoint, err := store.GetLatestCheckpoint(context.Background(), "cache-state")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Values["prior_id"] != json.Number("9007199254740993") || checkpoint.Values["answer"] != "cached" {
		t.Fatalf("cache checkpoint lost prior state: %#v", checkpoint.Values)
	}
	var output map[string]interface{}
	if err := json.Unmarshal(run.Output, &output); err != nil || len(output) != 1 || output["answer"] != "cached" {
		t.Fatalf("run output should remain its result patch: %s err=%v", run.Output, err)
	}
}

func TestCachedCompletionRollsBackPersistenceFailures(t *testing.T) {
	for _, statement := range []string{
		`CREATE TRIGGER fail_cache_checkpoint BEFORE INSERT ON thread_checkpoints BEGIN SELECT RAISE(ABORT, 'checkpoint unavailable'); END`,
		`CREATE TRIGGER fail_cache_thread BEFORE UPDATE OF values_json ON threads BEGIN SELECT RAISE(ABORT, 'thread unavailable'); END`,
		`CREATE TRIGGER fail_cache_output BEFORE UPDATE OF output ON runs BEGIN SELECT RAISE(ABORT, 'output unavailable'); END`,
	} {
		t.Run(statement, func(t *testing.T) {
			s, store, inject, req := cachedCompletionFixture(t)
			if _, err := inject.Exec(statement); err != nil {
				t.Fatal(err)
			}
			run, assignment, err := s.createRunCtx(context.Background(), "cache-state", req)
			if err == nil || run != nil || assignment != nil {
				t.Fatalf("failed cache commit reported success: run=%+v assignment=%+v err=%v", run, assignment, err)
			}
			_, err = store.GetRun(context.Background(), req.RunID)
			var missing *state.ErrNotFound
			if !errors.As(err, &missing) {
				t.Fatalf("failed cache commit left a durable run: %v", err)
			}
			thread, err := store.GetThread(context.Background(), "cache-state")
			if err != nil || thread.Status != models.ThreadStatusIdle || len(thread.Values) != 1 || thread.Values["prior_id"] != json.Number("9007199254740993") {
				t.Fatalf("failed cache commit changed the thread: thread=%+v err=%v", thread, err)
			}
		})
	}
}

func TestTransportRunIdentityRejectedBeforeThreadOrCachedRunCreation(t *testing.T) {
	for _, id := range []string{"*", ">", "tenant.*", "tenant.>", "Unicode-日本語", "id:part", "a..b"} {
		t.Run(id, func(t *testing.T) {
			s, store, _, req := cachedCompletionFixture(t)
			s.queue = &natstransport.Queue{}
			req.RunID = id
			payload, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/threads/new-invalid-thread/runs", bytes.NewReader(payload))
			recorder := httptest.NewRecorder()
			s.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("unsupported transport ID reached creation: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var missing *state.ErrNotFound
			if _, err := store.GetThread(context.Background(), "new-invalid-thread"); !errors.As(err, &missing) {
				t.Fatalf("unsupported transport ID created thread: %v", err)
			}
			if _, err := store.GetRun(context.Background(), id); !errors.As(err, &missing) {
				t.Fatalf("unsupported transport ID created cached run: %v", err)
			}
		})
	}
}
