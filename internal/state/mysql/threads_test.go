package mysql_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

func TestThread_CreateGetRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	thread := &models.Thread{
		ThreadID: "t1", Status: models.ThreadStatusIdle,
		Metadata: map[string]interface{}{"tag": "test"}, Values: map[string]interface{}{"count": float64(1)},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateThread(ctx, thread); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	got, err := s.GetThread(ctx, "t1")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if got.Status != models.ThreadStatusIdle || got.Metadata["tag"] != "test" || got.Values["count"] != float64(1) {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
}

func TestThread_CreateDuplicateConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	thread := &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}
	s.CreateThread(ctx, thread)

	err := s.CreateThread(ctx, thread)
	if _, ok := err.(*state.ErrConflict); !ok {
		t.Fatalf("expected ErrConflict for duplicate thread_id, got %v", err)
	}
}

func TestThread_GetNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetThread(context.Background(), "nope")
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestThread_UpdateMergesMetadataAndValues(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.CreateThread(ctx, &models.Thread{
		ThreadID: "t1", Status: models.ThreadStatusIdle,
		Metadata: map[string]interface{}{"a": "1"}, Values: map[string]interface{}{"x": "1"},
		CreatedAt: now, UpdatedAt: now,
	})

	updated, err := s.UpdateThread(ctx, "t1", &models.ThreadPatch{
		Metadata: map[string]interface{}{"b": "2"},
		Values:   map[string]interface{}{"y": "2"},
	})
	if err != nil {
		t.Fatalf("UpdateThread: %v", err)
	}
	if updated.Metadata["a"] != "1" || updated.Metadata["b"] != "2" {
		t.Fatalf("expected merged metadata (a and b both present), got %+v", updated.Metadata)
	}
	if updated.Values["x"] != "1" || updated.Values["y"] != "2" {
		t.Fatalf("expected merged values (x and y both present), got %+v", updated.Values)
	}

	// Persisted, not just returned in-memory.
	reloaded, _ := s.GetThread(ctx, "t1")
	if reloaded.Metadata["b"] != "2" {
		t.Fatalf("expected update persisted, got %+v", reloaded.Metadata)
	}
}

func TestThread_Delete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	if err := s.DeleteThread(ctx, "t1"); err != nil {
		t.Fatalf("DeleteThread: %v", err)
	}
	_, err := s.GetThread(ctx, "t1")
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	if err := s.DeleteThread(ctx, "t1"); err == nil {
		t.Fatal("expected error deleting an already-deleted thread")
	}
}

func TestThread_SearchByStatusAndMetadata(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{"team": "sales"}, CreatedAt: now, UpdatedAt: now})
	s.CreateThread(ctx, &models.Thread{ThreadID: "t2", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{"team": "support"}, CreatedAt: now, UpdatedAt: now})

	idle := models.ThreadStatusIdle
	byStatus, err := s.SearchThreads(ctx, &models.ThreadSearchRequest{Status: &idle, Limit: 10})
	if err != nil {
		t.Fatalf("SearchThreads(status): %v", err)
	}
	if len(byStatus) != 1 || byStatus[0].ThreadID != "t1" {
		t.Fatalf("expected only t1, got %+v", byStatus)
	}

	byMeta, err := s.SearchThreads(ctx, &models.ThreadSearchRequest{Metadata: map[string]interface{}{"team": "support"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchThreads(metadata): %v", err)
	}
	if len(byMeta) != 1 || byMeta[0].ThreadID != "t2" {
		t.Fatalf("expected only t2, got %+v", byMeta)
	}
}

func TestThread_SetThreadStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	if err := s.SetThreadStatus(ctx, "t1", models.ThreadStatusInterrupted); err != nil {
		t.Fatalf("SetThreadStatus: %v", err)
	}
	got, _ := s.GetThread(ctx, "t1")
	if got.Status != models.ThreadStatusInterrupted {
		t.Fatalf("expected status interrupted, got %s", got.Status)
	}
}

func TestThread_TryClaimThread(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	claimed, err := s.TryClaimThread(ctx, "t1")
	if err != nil || !claimed {
		t.Fatalf("expected first claim to succeed, got claimed=%v err=%v", claimed, err)
	}
	got, _ := s.GetThread(ctx, "t1")
	if got.Status != models.ThreadStatusBusy {
		t.Fatalf("expected status busy after claim, got %s", got.Status)
	}

	claimedAgain, err := s.TryClaimThread(ctx, "t1")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimedAgain {
		t.Fatal("expected second claim on an already-busy thread to fail")
	}
}

// TestThread_ConcurrentClaimOnlyOneWins is a direct empirical check of
// the doc comment's TOCTOU-safety claim: N goroutines racing
// TryClaimThread on the SAME idle thread must have exactly one winner.
func TestThread_ConcurrentClaimOnlyOneWins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	s.CreateThread(ctx, &models.Thread{ThreadID: "race-thread", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	const concurrency = 20
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := s.TryClaimThread(ctx, "race-thread")
			if err != nil {
				t.Errorf("TryClaimThread: %v", err)
				return
			}
			if claimed {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner among %d concurrent claims, got %d", concurrency, wins)
	}
}

// TestThread_TenantIsolation matches this schema's actual isolation
// model: unlike Agents/Registry (composite tenant-scoped primary keys,
// so two tenants CAN independently own the same natural key),
// threads.thread_id is globally unique -- PRIMARY KEY (thread_id) alone,
// same shape as Postgres/SQLite -- so isolation here is enforced via
// tenant-filtered WHERE clauses on access, not via a composite key
// letting two tenants each create their own row under the same ID.
// A second tenant attempting to reuse an existing thread_id must see a
// real conflict (the row already exists, full stop), and a tenant
// that doesn't own a thread_id must see ErrNotFound rather than
// another tenant's content when looking it up -- not "each tenant gets
// its own independent copy."
func TestThread_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	now := time.Now().UTC()

	if err := s.CreateThread(ctxA, &models.Thread{ThreadID: "shared-uuid-thread", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{"who": "a"}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateThread (tenant A): %v", err)
	}

	// Tenant B attempting to reuse the exact same thread_id must see a
	// real conflict, not silently succeed with its own independent row --
	// the PK is global, so this is a genuine collision, not a
	// per-tenant namespace.
	createErr := s.CreateThread(ctxB, &models.Thread{ThreadID: "shared-uuid-thread", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{"who": "b"}, CreatedAt: now, UpdatedAt: now})
	if _, ok := createErr.(*state.ErrConflict); !ok {
		t.Fatalf("expected tenant B's create with a colliding thread_id to report ErrConflict, got %v", createErr)
	}

	// Tenant B guessing/reusing the exact same thread ID must see
	// ErrNotFound on lookup, not tenant A's content -- the core
	// isolation guarantee, not a search-list nicety. NotFound (not
	// Forbidden) also avoids confirming the ID even exists.
	_, err := s.GetThread(ctxB, "shared-uuid-thread")
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected tenant B to see ErrNotFound for tenant A's thread, got %v", err)
	}

	// Tenant B deleting a thread_id it doesn't own must not affect
	// tenant A's row -- a scoped no-op (ErrNotFound), not silent data
	// loss for the actual owner.
	deleteErr := s.DeleteThread(ctxB, "shared-uuid-thread")
	if _, ok := deleteErr.(*state.ErrNotFound); !ok {
		t.Fatalf("expected tenant B's delete of a thread it doesn't own to report ErrNotFound, got %v", deleteErr)
	}
	stillThere, err := s.GetThread(ctxA, "shared-uuid-thread")
	if err != nil || stillThere.Metadata["who"] != "a" {
		t.Fatalf("expected tenant A's thread to survive tenant B's no-op delete attempt, got %+v err=%v", stillThere, err)
	}
}
