package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type cronDispatchCompletionStore struct {
	state.Store
	fail, ambiguous bool
}

func (s *cronDispatchCompletionStore) CompleteRun(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	if s.fail {
		return state.CompletionResult{}, errors.New("synthetic completion outage")
	}
	result, err := s.Store.CompleteRun(ctx, c)
	if err == nil && s.ambiguous {
		return result, errors.New("synthetic completion acknowledgement lost")
	}
	return result, err
}

type cronDispatchCreationStore struct {
	state.Store
	after func(*models.Run)
	err   error
}

func (s *cronDispatchCreationStore) result(run *models.Run, err error) error {
	if err != nil {
		return err
	}
	if s.after != nil {
		s.after(run)
	}
	return s.err
}
func (s *cronDispatchCreationStore) CreateRun(ctx context.Context, run *models.Run) error {
	return s.result(run, s.Store.CreateRun(ctx, run))
}
func (s *cronDispatchCreationStore) CreateRunAdmitted(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	return s.result(run, s.Store.CreateRunAdmitted(ctx, run, caps))
}
func (s *cronDispatchCreationStore) CreateCachedRun(ctx context.Context, run *models.Run, caps *state.RunAdmissionCaps) error {
	return s.result(run, s.Store.(state.CachedRunCreator).CreateCachedRun(ctx, run, caps))
}

type cronDispatchCloseFailureBroker struct{ transport.EventBroker }

func (*cronDispatchCloseFailureBroker) Close(string) error {
	return errors.New("synthetic finalization outage")
}

func TestScheduledRunRetryRequiresPositiveOriginalRollback(t *testing.T) {
	for _, mode := range []string{"never_started", "completed", "active", "store_error", "ambiguous_commit", "missing_identity", "replacement", "finalization_error"} {
		t.Run(mode, func(t *testing.T) {
			ctx := tenant.ScopedContext(context.Background(), " tenant-a ")
			cause := fmt.Errorf("synthetic queue acknowledgement: %w", &state.ErrConflict{Resource: "queue", ID: "synthetic"})
			queue := &enqueueReceiptErrorQueue{Queue: inprocess.NewQueue(), receiptErr: cause}
			plane, store := newLifecycleServer(t, queue)
			if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "store_error":
				plane.store = &cronDispatchCompletionStore{Store: store, fail: true}
			case "ambiguous_commit":
				plane.store = &cronDispatchCompletionStore{Store: store, ambiguous: true}
			case "missing_identity":
				plane.store = &cronDispatchCreationStore{Store: store, after: func(run *models.Run) { run.IncarnationID, run.StorageIdentity = "", "" }}
			case "finalization_error":
				plane.broker = &cronDispatchCloseFailureBroker{EventBroker: plane.broker}
			}
			queue.afterAccept = func(dispatchCtx context.Context, assignment *transport.RunAssignment) {
				if tenant.IsSystem(dispatchCtx) || tenant.FromContext(dispatchCtx) != " tenant-a " || assignment.TenantID != " tenant-a " {
					t.Fatal("scheduled creation or enqueue lost its exact tenant scope")
				}
				switch mode {
				case "completed", "active":
					rollbackAcceptAttempt(t, plane, queue.Queue, assignment)
					if mode == "completed" {
						rollbackCompleteAttempt(t, plane, assignment)
					}
				case "replacement":
					if err := store.DeleteRun(dispatchCtx, assignment.RunID); err != nil {
						t.Fatal(err)
					}
					now := time.Now().UTC()
					if err := store.CreateRun(dispatchCtx, &models.Run{RunID: assignment.RunID, ThreadID: assignment.ThreadID, AgentID: "agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}); err != nil {
						t.Fatal(err)
					}
				}
			}
			run, err := plane.DispatchScheduledRun(tenant.SystemContext(ctx), "daily", "agent", nil, nil)
			var disposition *ScheduledRunDispatchError
			var conflict *state.ErrConflict
			if run == nil || !errors.Is(err, cause) || !errors.As(err, &disposition) || !errors.As(err, &conflict) {
				t.Fatal("scheduled error lost its original run, cause or wrapped conflict")
			}
			if disposition.RetrySafe() != (mode == "never_started") {
				t.Fatalf("retry disposition=%t for %s", disposition.RetrySafe(), mode)
			}
			current, readErr := store.GetRun(ctx, run.RunID)
			if readErr != nil || current.TenantID != " tenant-a " {
				t.Fatal("original run was not retained in the exact tenant")
			}
			if mode == "never_started" {
				if current.Status != models.RunStatusError || current.Error != "enqueue failed" {
					t.Fatal("retry proof did not correspond to a committed pre-attempt failure")
				}
				if accepted, err := store.BeginRunAttempt(ctx, run.RunID, 1); err != nil || accepted {
					t.Fatal("a delayed queued copy could execute after positive rollback proof")
				}
			}
		})
	}
}

func TestScheduledRunCreationCommitErrorsRetainOriginalCandidate(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			ctx := tenant.ScopedContext(context.Background(), "tenant-a")
			queue := inprocess.NewQueue()
			plane, store := newLifecycleServer(t, queue)
			metadata := map[string]interface{}{}
			if cached {
				metadata["cache_ttl_seconds"] = 60
				now := time.Now().UTC()
				if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{CacheKey: computeCacheKey("tenant-a", "agent", nil, nil), AgentID: "agent", Output: map[string]interface{}{"answer": "cached"}, CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent", Metadata: metadata}); err != nil {
				t.Fatal(err)
			}
			cause := errors.New("synthetic creation commit acknowledgement lost")
			plane.store = &cronDispatchCreationStore{Store: store, err: cause}
			run, err := plane.DispatchScheduledRun(ctx, "daily", "agent", nil, nil)
			var disposition *ScheduledRunDispatchError
			if run == nil || !errors.Is(err, cause) || !errors.As(err, &disposition) || disposition.RetrySafe() {
				t.Fatal("ambiguous creation commit became a safe retry")
			}
			current, err := store.GetRun(ctx, run.RunID)
			if err != nil || state.RunIdentity(current) == "" || (!cached && state.RunIdentity(current) != state.RunIdentity(run)) {
				t.Fatal("ambiguous creation lost its original durable row")
			}
			if cached && (current.Status != models.RunStatusSuccess || len(current.Output) == 0) {
				t.Fatal("fixture did not commit a cache hit before losing acknowledgement")
			}
			if count, err := queue.Len(ctx); err != nil || count != 0 {
				t.Fatal("a creation error published an assignment")
			}
		})
	}
}

func TestScheduledRunPrecreationFailureRemainsRetryable(t *testing.T) {
	plane, store := newLifecycleServer(t, nil)
	ctx := tenant.ScopedContext(context.Background(), "tenant-a")
	run, err := plane.DispatchScheduledRun(ctx, "daily", "missing-agent", nil, nil)
	var disposition *ScheduledRunDispatchError
	var missing *state.ErrNotFound
	if run != nil || !errors.As(err, &missing) || errors.As(err, &disposition) {
		t.Fatal("definite precreation failure acquired an uncertain-dispatch disposition")
	}
	runs, err := store.SearchRuns(ctx, &models.RunSearchRequest{Limit: 10})
	if err != nil || len(runs) != 0 {
		t.Fatal("precreation failure persisted a run")
	}
}

func TestScheduledDispositionDoesNotChangeHTTPEnqueueError(t *testing.T) {
	plane, store := newLifecycleServer(t, failQueue{})
	ctx := tenant.ScopedContext(context.Background(), "tenant-a")
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/threads/http-thread/runs", strings.NewReader(`{"agent_id":"agent"}`)).WithContext(ctx)
	plane.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "failed to enqueue run") || strings.Contains(w.Body.String(), "retrySafe") {
		t.Fatal("internal scheduled retry disposition changed the HTTP error contract")
	}
}

func TestScheduledRunAtomicAdmissionRejectionCanRetryAfterCapacityReturns(t *testing.T) {
	plane, store := newLifecycleServer(t, nil)
	ctx := tenant.ScopedContext(context.Background(), "tenant-a")
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
		t.Fatal(err)
	}
	plane.SetAdmissionLimits(&AdmissionLimits{AgentConcurrent: 1})
	first, _, err := plane.createRunCtx(ctx, "existing-work", &models.RunCreate{AgentID: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := plane.DispatchScheduledRun(ctx, "daily", "agent", nil, nil)
	var admission *state.ErrAdmissionLimitExceeded
	var disposition *ScheduledRunDispatchError
	if run != nil || !errors.As(err, &admission) || errors.As(err, &disposition) {
		t.Fatal("atomic admission rejection was treated as ambiguous creation")
	}
	threadID := canonicalCronThreadID("tenant-a", "daily")
	if runs, err := store.SearchRuns(ctx, &models.RunSearchRequest{ThreadID: threadID, Limit: 10}); err != nil || len(runs) != 0 {
		t.Fatal("admission rejection inserted a scheduled run")
	}
	if err := store.UpdateRunStatus(ctx, first.RunID, models.RunStatusError, nil, "synthetic capacity release"); err != nil {
		t.Fatal(err)
	}
	run, err = plane.DispatchScheduledRun(ctx, "daily", "agent", nil, nil)
	if err != nil || run == nil || run.ThreadID != threadID {
		t.Fatal("scheduled work could not retry after the known admission rejection")
	}
}
