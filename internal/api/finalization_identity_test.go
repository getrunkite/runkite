package api_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/state/sqlite"
)

type pausedFinalizationHoldStore struct {
	*sqlite.SQLiteStore
	paused atomic.Bool
	read   chan struct{}
	resume chan struct{}
}

func (s *pausedFinalizationHoldStore) ReleaseRunUsageHold(ctx context.Context, runID, incarnation string) error {
	if s.paused.CompareAndSwap(false, true) {
		close(s.read)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.SQLiteStore.ReleaseRunUsageHold(ctx, runID, incarnation)
}

func TestFinalizationCannotReleaseRecreatedRunsHold(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	store, queue, broker, cancels := completionFixture(t)
	original, err := store.GetRun(ctx, "run")
	if err != nil {
		t.Fatal(err)
	}
	identity := state.RunIdentity(original)
	if accepted, err := store.BeginRunAttempt(ctx, "run", identity, 1); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	if err := store.UpsertRunUsageHold(ctx, identity, &models.UsageHold{RunID: "run", TenantID: "default", AgentID: "agent", TokensHold: 3}); err != nil {
		t.Fatal(err)
	}
	paused := &pausedFinalizationHoldStore{SQLiteStore: store, read: make(chan struct{}), resume: make(chan struct{})}
	plane := api.NewServer(paused, queue, broker, cancels)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = plane.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: identity, RunID: "run", Generation: 1, Status: models.RunStatusSuccess})
	}()
	select {
	case <-paused.read:
	case <-ctx.Done():
		t.Fatal("completion did not reach finalization")
	}
	if err := store.DeleteRun(ctx, "run"); err != nil {
		close(paused.resume)
		t.Fatal(err)
	}
	replacement := *original
	replacement.IncarnationID, replacement.StorageIdentity = "", ""
	if err := store.CreateRun(ctx, &replacement); err != nil {
		close(paused.resume)
		t.Fatal(err)
	}
	if err := store.UpsertRunUsageHold(ctx, state.RunIdentity(&replacement), &models.UsageHold{RunID: "run", TenantID: "default", AgentID: "agent", TokensHold: 17}); err != nil {
		close(paused.resume)
		t.Fatal(err)
	}
	close(paused.resume)
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("finalization did not finish after replacement")
	}
	_, held, count, err := store.SumUsageHolds(ctx, "default", "agent", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || count != 1 || held != 17 {
		t.Fatal("old completion released the replacement's budget hold", count, held, err)
	}
}
