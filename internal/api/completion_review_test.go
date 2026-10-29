package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport"
)

func publishReviewEvent(t *testing.T, b transport.EventBroker, gen, seq int64, method, raw string) {
	t.Helper()
	if err := b.Publish(context.Background(), "run", &transport.RunEvent{Generation: gen, EventID: method + "-" + raw, Seq: seq, Method: method, Data: json.RawMessage(raw)}); err != nil {
		t.Fatal(err)
	}
}

func TestWaitIgnoresTerminalAndValuesFromSupersededAttempt(t *testing.T) {
	store, q, b, c := completionFixture(t)
	plane := api.NewServer(store, q, b, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	publishReviewEvent(t, b, 1, 1, "values", `{"answer":"stale"}`)
	publishReviewEvent(t, b, 1, 2, "end", `{"status":"success"}`)
	// Model an already dispatched newer attempt. The API recovery helper must
	// not be used here because that intentionally recovers fully delivered ends.
	if _, err := store.BeginRunAttempt(ctx, "run", 2); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/runs/run/wait", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { plane.Handler().ServeHTTP(rec, request); close(done) }()
	select {
	case <-done:
		t.Fatalf("wait returned on stale attempt: %d %s", rec.Code, rec.Body.String())
	case <-time.After(30 * time.Millisecond):
	}
	publishReviewEvent(t, b, 2, 1, "values", `{"answer":"current"}`)
	publishReviewEvent(t, b, 2, 2, "end", `{"status":"success"}`)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("wait never observed current completion")
	}
	var response models.RunWaitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || response.Run == nil || response.Run.Status != models.RunStatusSuccess || response.Values["answer"] != "current" {
		t.Fatalf("wait=%d %s", rec.Code, rec.Body.String())
	}
}

type failOnceUsageStore struct {
	*sqlite.SQLiteStore
	writes atomic.Int32
}

func (s *failOnceUsageStore) WriteUsageEvent(ctx context.Context, event *models.UsageEvent) error {
	if s.writes.Add(1) == 1 {
		return errors.New("injected transient usage outage")
	}
	return s.SQLiteStore.WriteUsageEvent(ctx, event)
}
func TestCompletionRetryRecoversPostCommitUsageWrite(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &failOnceUsageStore{SQLiteStore: store}
	plane := api.NewServer(fault, q, b, c)
	ctx := context.Background()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	publishReviewEvent(t, b, 1, 1, "values", `{"answer":"kept","usage":{"prompt_tokens":5,"completion_tokens":2,"model":"synthetic"}}`)
	completion := &state.RunCompletion{RunID: "run", Generation: 1, Status: models.RunStatusSuccess}
	// Persistence may commit before a dependent write fails. Replaying that
	// committed completion must still finish the idempotent dependent work.
	_, _ = plane.CompleteRun(ctx, completion)
	if accepted, err := plane.CompleteRun(ctx, completion); err != nil || !accepted {
		t.Fatalf("retry=%v %v", accepted, err)
	}
	tin, tout, _, err := store.SumUsage(ctx, "default", "agent", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || tin != 5 || tout != 2 {
		t.Fatalf("usage after retry: in=%d out=%d err=%v writes=%d", tin, tout, err, fault.writes.Load())
	}
}

type pausedCancelReadStore struct {
	*sqlite.SQLiteStore
	captured atomic.Bool
	ready    chan struct{}
	resume   chan struct{}
}

func (s *pausedCancelReadStore) GetRun(ctx context.Context, id string) (*models.Run, error) {
	run, err := s.SQLiteStore.GetRun(ctx, id)
	if err == nil && id == "run" && s.captured.CompareAndSwap(false, true) {
		close(s.ready)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return run, err
}
func TestLateCancelDoesNotOverwriteCompletionOrNewThreadClaim(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &pausedCancelReadStore{SQLiteStore: store, ready: make(chan struct{}), resume: make(chan struct{})}
	plane := api.NewServer(fault, q, b, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	publishReviewEvent(t, b, 1, 1, "values", `{"answer":"kept"}`)
	request := httptest.NewRequest(http.MethodPost, "/runs/run/cancel", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { plane.Handler().ServeHTTP(rec, request); close(done) }()
	select {
	case <-fault.ready:
	case <-ctx.Done():
		t.Fatal("cancel did not read initial run")
	}
	if accepted, err := plane.CompleteRun(ctx, &state.RunCompletion{RunID: "run", Generation: 1, Status: models.RunStatusSuccess}); err != nil || !accepted {
		close(fault.resume)
		t.Fatalf("completion=%v %v", accepted, err)
	}
	if claimed, err := store.TryClaimThread(ctx, "thread"); err != nil || !claimed {
		close(fault.resume)
		t.Fatalf("new claim=%v %v", claimed, err)
	}
	now := time.Now().UTC()
	if err := store.CreateRun(ctx, &models.Run{RunID: "next", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}); err != nil {
		close(fault.resume)
		t.Fatal(err)
	}
	close(fault.resume)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("cancel did not finish")
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.RunStatusSuccess || thread.Status != models.ThreadStatusBusy || thread.Values["answer"] != "kept" {
		t.Fatalf("late cancel overwrote winner: run=%+v thread=%+v", run, thread)
	}
}

func TestCancelTargetsAttemptAdvancedAfterInitialRead(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &pausedCancelReadStore{SQLiteStore: store, ready: make(chan struct{}), resume: make(chan struct{})}
	plane := api.NewServer(fault, q, b, c)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	if job, err := q.Dequeue(ctx, "kind", time.Second); err != nil || job == nil {
		t.Fatalf("initial dequeue=%v %v", job, err)
	}
	request := httptest.NewRequest(http.MethodPost, "/runs/run/cancel", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { plane.Handler().ServeHTTP(rec, request); close(done) }()
	select {
	case <-fault.ready:
	case <-ctx.Done():
		t.Fatal("cancel did not read initial attempt")
	}
	if _, _, err := q.ReclaimStale(ctx, -time.Second, 3); err != nil {
		close(fault.resume)
		t.Fatal(err)
	}
	if job, err := q.Dequeue(ctx, "kind", time.Second); err != nil || job == nil || job.Generation != 2 {
		close(fault.resume)
		t.Fatalf("replacement dequeue=%v %v", job, err)
	}
	if _, err := store.BeginRunAttempt(ctx, "run", 2); err != nil {
		close(fault.resume)
		t.Fatal(err)
	}
	close(fault.resume)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("cancel did not finish")
	}
	run, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.GetThread(ctx, "thread")
	if err != nil {
		t.Fatal(err)
	}
	inflight, err := q.LookupInflight(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || run.Status != models.RunStatusInterrupted || thread.Status != models.ThreadStatusInterrupted || inflight != nil {
		t.Fatalf("advanced cancellation: status=%d run=%+v thread=%+v inflight=%+v", rec.Code, run, thread, inflight)
	}
}

func TestCancelPersistenceFailureRetainsQueueLease(t *testing.T) {
	store, q, b, c := completionFixture(t)
	fault := &completionFaultStore{Store: store, fail: true}
	plane := api.NewServer(fault, q, b, c)
	ctx := context.Background()
	if _, err := store.BeginRunAttempt(ctx, "run", 1); err != nil {
		t.Fatal(err)
	}
	if job, err := q.Dequeue(ctx, "kind", time.Second); err != nil || job == nil {
		t.Fatalf("dequeue=%v %v", job, err)
	}
	request := httptest.NewRequest(http.MethodPost, "/runs/run/cancel", nil)
	rec := httptest.NewRecorder()
	plane.Handler().ServeHTTP(rec, request)
	run, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	inflight, err := q.LookupInflight(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code < 500 || run.Status != models.RunStatusPending || inflight == nil {
		t.Fatalf("failed cancellation discarded work: status=%d run=%+v inflight=%+v", rec.Code, run, inflight)
	}
}
