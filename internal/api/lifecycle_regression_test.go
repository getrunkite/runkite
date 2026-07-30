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

	"github.com/sharanharsoor/runkite/internal/models"
	sqlitestore "github.com/sharanharsoor/runkite/internal/state/sqlite"
	"github.com/sharanharsoor/runkite/internal/tenant"
	"github.com/sharanharsoor/runkite/internal/transport"
	"github.com/sharanharsoor/runkite/internal/transport/inprocess"
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
func (failQueue) Ack(context.Context, string) error   { return nil }
func (failQueue) Nack(context.Context, string) error  { return nil }
func (failQueue) Renew(context.Context, string) error { return nil }
func (failQueue) Cancel(context.Context, string) error {
	return nil
}
func (failQueue) Len(context.Context) (int64, error) { return 0, nil }

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

func TestThreadHasOtherActiveRun_NotMaskedByCacheHits(t *testing.T) {
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
	// Flood newer success rows the way cache hits do -- unfiltered
	// newest-50 search used to miss the older pending run.
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

	if !threadHasOtherActiveRun(ctx, store, threadID, "exclude-none") {
		t.Fatal("pending run hidden behind cache-hit successes")
	}
	if threadHasOtherActiveRun(ctx, store, threadID, "pending-1") {
		t.Fatal("excluding the only pending run should report no other active")
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
func TestCreateRunCtx_CreateRunRaceReleasesLoserThreadClaim(t *testing.T) {
	s, store := newLifecycleServer(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()

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
	var raceErr *errRunRetryRace
	if !errors.As(err, &raceErr) {
		t.Fatalf("expected errRunRetryRace, got %T: %v", err, err)
	}
	if raceErr.run.RunID != sharedRunID || raceErr.run.ThreadID != winnerThread {
		t.Fatalf("expected the WINNER's run (thread=%s), got thread=%s", winnerThread, raceErr.run.ThreadID)
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
