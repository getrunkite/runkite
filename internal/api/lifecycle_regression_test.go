package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

// failQueue always rejects Enqueue -- used to prove rollbackCreatedRun
// leaves the thread claimable again after create+enqueue failure.
type failQueue struct{}

func (failQueue) Enqueue(context.Context, *transport.RunAssignment) error {
	return errors.New("queue down")
}
func (failQueue) Dequeue(context.Context, string, time.Duration) (*transport.RunAssignment, error) {
	return nil, errors.New("unused")
}
func (failQueue) Ack(context.Context, string, int64) (bool, error)   { return true, nil }
func (failQueue) Nack(context.Context, string) error                 { return nil }
func (failQueue) Renew(context.Context, string, int64) (bool, error) { return true, nil }
func (failQueue) Cancel(context.Context, string) error {
	return nil
}
func (failQueue) Len(context.Context) (int64, error) { return 0, nil }
func (failQueue) Ping(context.Context) error         { return nil }

func newLifecycleServer(t *testing.T, queue transport.JobQueue) (*Server, *sqlitestore.SQLiteStore) {
	t.Helper()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if queue == nil {
		queue = inprocess.NewQueue()
	}
	s := NewServer(store, queue, inprocess.NewBroker(), inprocess.NewCancelBus())
	return s, store
}

func TestCreateRunCtx_ReleasesClaimOnA2AParentMissing(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	ctx := context.Background()
	threadID := "claim-release-a2a"
	_ = store.CreateThread(ctx, &models.Thread{
		ThreadID: threadID, Status: models.ThreadStatusIdle,
		Metadata: map[string]interface{}{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	missing := "no-such-parent"
	_, _, err := s.createRunCtx(ctx, threadID, &models.RunCreate{
		AgentID: "agent", ParentRunID: &missing,
	})
	if err == nil {
		t.Fatal("expected error for missing A2A parent")
	}
	th, err := store.GetThread(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if th.Status != models.ThreadStatusIdle {
		t.Fatalf("thread stuck %q after createRunCtx failure; want idle", th.Status)
	}
}

func TestEnqueueFailure_RollsBackBusyThread(t *testing.T) {
	s, store := newLifecycleServer(t, failQueue{})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	ctx := context.Background()
	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "agent", Name: "agent"}); err != nil {
		t.Fatal(err)
	}
	threadID := "enqueue-fail"
	_ = store.CreateThread(ctx, &models.Thread{
		ThreadID: threadID, Status: models.ThreadStatusIdle,
		Metadata: map[string]interface{}{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	body := `{"agent_id":"agent"}`
	resp, err := http.Post(srv.URL+"/threads/"+threadID+"/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", resp.StatusCode)
	}

	th, err := store.GetThread(ctx, threadID)
	if err != nil {
		t.Fatal(err)
	}
	if th.Status != models.ThreadStatusIdle {
		t.Fatalf("thread stuck %q after enqueue failure; want idle", th.Status)
	}

	// A follow-up create must be able to claim the thread again.
	s.queue = inprocess.NewQueue()
	run, _, err := s.createRunCtx(ctx, threadID, &models.RunCreate{AgentID: "agent"})
	if err != nil {
		t.Fatalf("second create after rollback: %v", err)
	}
	if run == nil {
		t.Fatal("expected run on second create")
	}
}

// TestReleaseThreadIfNoOtherActive_NotMaskedByCacheHits proves the
// atomic release predicate looks at pending/running status, not a
// newest-N flood of cache-hit success rows (the bug that made the old
// SearchRuns helper miss an older pending run and idle a busy thread).
func TestReleaseThreadIfNoOtherActive_NotMaskedByCacheHits(t *testing.T) {
	_, store := newLifecycleServer(t, nil)
	ctx := context.Background()
	threadID := "active-behind-hits"
	_ = store.CreateThread(ctx, &models.Thread{
		ThreadID: threadID, Status: models.ThreadStatusBusy,
		Metadata: map[string]interface{}{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})

	now := time.Now().UTC()
	pending := &models.Run{
		RunID: "pending-1", ThreadID: threadID, AgentID: "a", AssistantID: "a",
		Status: models.RunStatusPending, Metadata: map[string]interface{}{},
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	}
	if err := store.CreateRun(ctx, pending); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		r := &models.Run{
			RunID: fmt.Sprintf("hit-%d", i), ThreadID: threadID, AgentID: "a", AssistantID: "a",
			Status: models.RunStatusSuccess, Metadata: map[string]interface{}{"cache_hit": true},
			CreatedAt: now.Add(time.Duration(i) * time.Second), UpdatedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := store.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	released, err := store.ReleaseThreadIfNoOtherActive(ctx, threadID, "exclude-none", models.ThreadStatusIdle)
	if err != nil {
		t.Fatal(err)
	}
	if released {
		t.Fatal("must not release while pending-1 is still active behind cache-hit successes")
	}
	released, err = store.ReleaseThreadIfNoOtherActive(ctx, threadID, "pending-1", models.ThreadStatusIdle)
	if err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("excluding the only pending run should allow release")
	}
}

func TestClampSearchLimit(t *testing.T) {
	if got := clampSearchLimit(0, 10); got != 10 {
		t.Fatalf("default: got %d", got)
	}
	if got := clampSearchLimit(1_000_000, 10); got != maxSearchLimit {
		t.Fatalf("cap: got %d", got)
	}
	if got := clampSearchLimit(25, 10); got != 25 {
		t.Fatalf("passthrough: got %d", got)
	}
}

func TestReadJSON_RejectsOversizedBody(t *testing.T) {
	huge := `{"x":"` + strings.Repeat("a", maxJSONBodyBytes+10) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(huge))
	var v map[string]interface{}
	if err := readJSON(req, &v); err == nil {
		t.Fatal("expected error for oversized JSON body")
	}
}

// TestCreateRunCtx_CreateRunRaceReleasesLoserThreadClaim is a regression
// test for an orphan-busy-thread bug: two concurrent create-run requests
// with the same client-supplied run_id but different thread_ids race on
// CreateRun's run_id-is-primary-key uniqueness. The loser's OWN thread
// (which it separately, successfully claimed via TryClaimThread just
// before losing the CreateRun race) has no run on it and must be released
// back to idle -- otherwise it stays "busy" forever with nothing left to
// ever idle it. Forces the race deterministically (pre-inserting the
// "winning" run on a different thread) instead of relying on goroutine
// timing, so this can't flake.
//
// Also covers the fingerprint check createRunCtx applies against a
// run_id retry: this exact scenario -- same run_id, different thread_id
// -- is precisely the client mistake / run_id collision that check
// catches, so the correct outcome here is a clear state.ErrConflict, not
// silently dispatching the OTHER thread's run via errRunRetryRace as if
// it were a normal retry. The orphan-busy-thread assertion below (the
// actual bug this test exists for) is unaffected either way -- the
// defer that releases the loser's thread claim fires on ANY non-nil
// error, regardless of which one.
func TestCreateRunCtx_CreateRunRaceReleasesLoserThreadClaim(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "a", Name: "a"}); err != nil {
		t.Fatal(err)
	}

	winnerThread := "winner-thread"
	loserThread := "loser-thread"
	sharedRunID := "client-retry-run-id"
	for _, id := range []string{winnerThread, loserThread} {
		if err := store.CreateThread(ctx, &models.Thread{
			ThreadID: id, Status: models.ThreadStatusIdle,
			Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the winner having already inserted its run under the
	// shared run_id, on a DIFFERENT thread than this call will use.
	if err := store.CreateRun(ctx, &models.Run{
		RunID: sharedRunID, ThreadID: winnerThread, AgentID: "a", AssistantID: "a",
		Status: models.RunStatusPending, Metadata: map[string]interface{}{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// This call successfully claims loserThread (nothing else holds it),
	// then loses at CreateRun on the run_id collision.
	run, assignment, err := s.createRunCtx(ctx, loserThread, &models.RunCreate{AgentID: "a", RunID: sharedRunID})
	if run != nil || assignment != nil {
		t.Fatalf("expected nil run/assignment on a retry-race loss, got run=%v assignment=%v", run, assignment)
	}
	var conflictErr *state.ErrConflict
	if !errors.As(err, &conflictErr) {
		t.Fatalf("expected *state.ErrConflict (fingerprint mismatch: different thread_id), got %T: %v", err, err)
	}
	if !strings.Contains(conflictErr.Reason, "thread") {
		t.Fatalf("expected a thread-id mismatch reason, got %q", conflictErr.Reason)
	}

	// The critical assertion: loserThread must not be stuck busy forever.
	th, getErr := store.GetThread(ctx, loserThread)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if th.Status != models.ThreadStatusIdle {
		t.Fatalf("loser thread stuck %q after losing the run_id race; want idle (orphan-busy-thread bug)", th.Status)
	}
}

// TestCreateRunCtx_TryClaimRaceReturnsRetrySentinelNotFakeCacheHit is a
// regression test proving the TryClaimThread race fallback returns a
// distinct errRunRetryRace, NOT the (run, nil, nil) "cache hit" sentinel
// createRunCtx also uses for genuine LLM cache hits. Conflating the two
// would make callers (streamRun/waitForRunResult) treat a still-PENDING
// run as a definitively-complete synthetic success -- see errRunRetryRace's
// own doc comment for why that's a real correctness bug, not just style.
func TestCreateRunCtx_TryClaimRaceReturnsRetrySentinelNotFakeCacheHit(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := store.UpsertAgent(ctx, &models.Agent{AgentID: "a", Name: "a"}); err != nil {
		t.Fatal(err)
	}

	threadID := "busy-retry-thread"
	sharedRunID := "in-flight-run-id"
	if err := store.CreateThread(ctx, &models.Thread{
		ThreadID: threadID, Status: models.ThreadStatusBusy, // already claimed by "the original request"
		Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// The "original request" has claimed the thread but its run is still
	// genuinely PENDING -- not finished, not safe to render as a success.
	if err := store.CreateRun(ctx, &models.Run{
		RunID: sharedRunID, ThreadID: threadID, AgentID: "a", AssistantID: "a",
		Status: models.RunStatusPending, Metadata: map[string]interface{}{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	run, assignment, err := s.createRunCtx(ctx, threadID, &models.RunCreate{AgentID: "a", RunID: sharedRunID})
	if run != nil || assignment != nil {
		t.Fatalf("expected nil run/assignment (not a cache-hit-style success), got run=%v assignment=%v", run, assignment)
	}
	var raceErr *errRunRetryRace
	if !errors.As(err, &raceErr) {
		t.Fatalf("expected errRunRetryRace, got %T: %v", err, err)
	}
	if raceErr.run.Status != models.RunStatusPending {
		t.Fatalf("sentinel's own run should carry the REAL (pending) status, got %q", raceErr.run.Status)
	}
}

// TestHandleCreateRunError_DispatchesRaceAndFallsBackToStoreError is a
// regression test proving handleCreateRunError's own contract:
// it exists specifically so no future create-run handler can forget the
// retry-race dispatch the way the old two-separate-calls convention
// could. Proves both halves of that single call directly: an
// errRunRetryRace gets dispatched through respond (never touching w),
// and any OTHER error (e.g. state.ErrConflict) falls through to
// handleStoreError's own normal status-code mapping.
func TestHandleCreateRunError_DispatchesRaceAndFallsBackToStoreError(t *testing.T) {
	s, _ := newLifecycleServer(t, nil)

	t.Run("retry_race_dispatches_through_respond_not_handleStoreError", func(t *testing.T) {
		w := httptest.NewRecorder()
		winner := &models.Run{RunID: "winner-run", ThreadID: "t1", Status: models.RunStatusPending}
		var dispatched *models.Run
		s.handleCreateRunError(w, &errRunRetryRace{run: winner}, func(existing *models.Run) {
			dispatched = existing
		})
		if dispatched != winner {
			t.Fatalf("expected respond to be called with the race's own run, got %v", dispatched)
		}
		// respond, not writeJSON/writeError, owns the response for this
		// path -- handleCreateRunError itself must not also write to w.
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("expected handleCreateRunError to leave the response untouched for a dispatched race, got code=%d body=%q", w.Code, w.Body.String())
		}
	})

	t.Run("non_race_error_falls_back_to_handleStoreError_mapping", func(t *testing.T) {
		w := httptest.NewRecorder()
		respondCalled := false
		s.handleCreateRunError(w, &state.ErrConflict{Resource: "run", ID: "x"}, func(*models.Run) {
			respondCalled = true
		})
		if respondCalled {
			t.Fatal("respond must not be called for a non-race error")
		}
		if w.Code != http.StatusConflict {
			t.Fatalf("expected handleStoreError's own 409 mapping for *state.ErrConflict, got %d", w.Code)
		}
	})
}

func TestCacheHit_ConflictsWhenThreadBusy(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	ctx := context.Background()
	threadID := "busy-cache"
	_ = store.CreateThread(ctx, &models.Thread{
		ThreadID: threadID, Status: models.ThreadStatusBusy,
		Metadata: map[string]interface{}{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	_ = store.UpsertAgent(ctx, &models.Agent{
		AgentID: "cached-agent", Name: "cached",
		Metadata:     map[string]interface{}{"cache_ttl_seconds": 60},
		Capabilities: map[string]interface{}{},
		Version:      1,
	})
	input := json.RawMessage(`{"q":1}`)
	key := computeCacheKey(tenant.FromContext(ctx), "cached-agent", input, nil)
	now := time.Now().UTC()
	if err := store.SaveCachedRunResult(ctx, &models.CachedRunResult{
		CacheKey: key, AgentID: "cached-agent",
		Output:    map[string]interface{}{"ok": true},
		CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	_, hit, err := s.tryServeCachedRun(ctx, "run-cache", threadID, &models.RunCreate{
		AgentID: "cached-agent", Input: input,
	}, time.Now().UTC())
	if err == nil {
		t.Fatal("expected conflict when serving cache hit on busy thread")
	}
	if hit {
		t.Fatal("hit should be false on conflict")
	}
}

// failDeleteRunStore wraps a real state.Store but forces DeleteRun to
// fail -- everything else (including the cancel side effects
// cancelRunCore performs before ever reaching DeleteRun) goes through to
// the real store unchanged.
type failDeleteRunStore struct {
	state.Store
	err error
}

func (f failDeleteRunStore) DeleteRun(ctx context.Context, runID string) error { return f.err }

// TestCancelRun_RollbackDeleteFailure_ReturnsErrorNotSilent204 is a
// regression test for a real bug caught on review: action=rollback
// swallowed a DeleteRun failure and still returned nil, nil, which the
// HTTP handler renders as 204 No Content -- indistinguishable from a
// successful rollback. A client would believe the run was deleted while
// a GET could still find it (as "interrupted", since the cancel half
// genuinely did succeed and isn't undone by the delete failing). The
// delete failure must surface as an error response instead, the same as
// a plain DELETE /runs/{id} failing (deleteRunGuarded's own contract).
func TestCancelRun_RollbackDeleteFailure_ReturnsErrorNotSilent204(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	ctx := context.Background()

	threadID := "rollback-fail-thread"
	if err := store.CreateThread(ctx, &models.Thread{
		ThreadID: threadID, Status: models.ThreadStatusBusy,
		Metadata: map[string]interface{}{}, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	runID := "rollback-fail-run"
	if err := store.CreateRun(ctx, &models.Run{
		RunID: runID, ThreadID: threadID, AgentID: "a1", Status: models.RunStatusPending,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Metadata: map[string]interface{}{},
	}); err != nil {
		t.Fatal(err)
	}

	// Swap in the failing store AFTER seeding, so cancelRunSingle's own
	// GetRun/UpdateRunStatus/SetThreadStatus calls (which must succeed
	// for this test to isolate DeleteRun specifically) still hit the
	// real store underneath.
	s.store = failDeleteRunStore{Store: store, err: errors.New("disk full")}

	req := httptest.NewRequest(http.MethodPost, "/runs/"+runID+"/cancel?action=rollback", nil)
	w := httptest.NewRecorder()
	s.cancelRun(w, req, runID)

	if w.Code == http.StatusNoContent {
		t.Fatalf("expected an error response when DeleteRun fails, got a silent 204 (client would wrongly believe the run was deleted)")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 for a generic DeleteRun error, got %d: %s", w.Code, w.Body.String())
	}

	// The cancel half genuinely succeeded and is not undone by the
	// delete failing -- confirm the run is still findable, now
	// interrupted, through the REAL underlying store.
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("expected the run to still exist after a failed rollback delete, got error: %v", err)
	}
	if run.Status != models.RunStatusInterrupted {
		t.Fatalf("expected the cancel side effect to have applied despite the delete failure, got status=%s", run.Status)
	}
}

// TestFingerprintMismatch_MultiTargetAliasRetryNeverFalseConflicts is a
// regression test for a real bug caught on review: comparing a retry's
// agent name against a FRESH call to AliasResolver.Resolve would
// re-roll a weighted-random pick every time (see Resolve's own doc
// comment) -- a retry landing on a DIFFERENT real target than the
// original request did would then look like a genuine agent mismatch,
// turning a legitimate retry of a multi-target alias into a false
// conflict. The fix compares against the alias NAME recorded on the
// original run (Metadata's own "requested_alias", set once and never
// re-rolled) instead of re-resolving.
//
// A 50/50 split makes the old, buggy comparison (run.AgentID against a
// freshly re-resolved target) fail with extremely high probability
// within a handful of calls if it's still re-rolling -- this runs 30 to
// make that failure essentially certain if the bug were still present,
// while the fix itself has no randomness in its own decision at all.
func TestFingerprintMismatch_MultiTargetAliasRetryNeverFalseConflicts(t *testing.T) {
	s, _ := newLifecycleServer(t, nil)
	s.SetAliasResolver(NewAliasResolver(map[string]config.AgentAliasEntry{
		"my_agent": {Targets: map[string]int{"my_agent_v1": 1, "my_agent_v2": 1}},
	}))

	// The original request resolved to v1 and recorded that it was
	// requested through the "my_agent" alias -- exactly what
	// createRunCtx itself writes to Metadata on a real alias hit.
	run := &models.Run{
		RunID: "alias-retry-run", ThreadID: "t1", AgentID: "my_agent_v1",
		Metadata: map[string]interface{}{"requested_alias": "my_agent"},
	}

	for i := 0; i < 30; i++ {
		retry := &models.RunCreate{AgentID: "my_agent"}
		if reason := s.fingerprintMismatch(run, retry, ""); reason != "" {
			t.Fatalf("iteration %d: expected a retry through the same alias to never conflict regardless of which real target Resolve happens to pick, got %q", i, reason)
		}
	}
}

// TestFingerprintMismatch_DifferentAliasIsStillAMismatch proves the fix
// above didn't just make alias comparisons vacuously always pass -- a
// retry through a GENUINELY different alias name than the original
// request used is still correctly flagged.
func TestFingerprintMismatch_DifferentAliasIsStillAMismatch(t *testing.T) {
	s, _ := newLifecycleServer(t, nil)
	s.SetAliasResolver(NewAliasResolver(map[string]config.AgentAliasEntry{
		"my_agent":    {Targets: map[string]int{"my_agent_v1": 1}},
		"other_alias": {Targets: map[string]int{"my_agent_v1": 1}},
	}))
	run := &models.Run{
		RunID: "alias-retry-run-2", ThreadID: "t1", AgentID: "my_agent_v1",
		Metadata: map[string]interface{}{"requested_alias": "my_agent"},
	}
	reason := s.fingerprintMismatch(run, &models.RunCreate{AgentID: "other_alias"}, "")
	if reason == "" {
		t.Fatal("expected a mismatch: the retry named a different alias than the original run recorded, even though both happen to resolve to the same real target")
	}
}
