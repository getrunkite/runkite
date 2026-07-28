package mysql_test

import (
	"context"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// Mirrors the shared conformance suite's runRunCacheTests.

func TestRunCache_SaveAndGetRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	result := &models.CachedRunResult{
		CacheKey: "key-1", AgentID: "agent-cache",
		Output:    map[string]interface{}{"messages": []interface{}{"hi"}},
		CreatedAt: now, ExpiresAt: now.Add(1 * time.Hour),
	}
	if err := s.SaveCachedRunResult(ctx, result); err != nil {
		t.Fatalf("SaveCachedRunResult: %v", err)
	}

	got, err := s.GetCachedRunResult(ctx, "key-1")
	if err != nil {
		t.Fatalf("GetCachedRunResult: %v", err)
	}
	if got.AgentID != "agent-cache" {
		t.Errorf("AgentID = %q, want agent-cache", got.AgentID)
	}
	msgs, _ := got.Output["messages"].([]interface{})
	if len(msgs) != 1 || msgs[0] != "hi" {
		t.Errorf("Output not preserved: %+v", got.Output)
	}
}

func TestRunCache_ExpiredEntryIsNotReturned(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	expired := &models.CachedRunResult{
		CacheKey: "key-expired", AgentID: "agent-cache",
		Output:    map[string]interface{}{"v": 1},
		CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-1 * time.Hour),
	}
	if err := s.SaveCachedRunResult(ctx, expired); err != nil {
		t.Fatalf("SaveCachedRunResult: %v", err)
	}

	_, err := s.GetCachedRunResult(ctx, "key-expired")
	if err == nil {
		t.Fatal("expected ErrNotFound for an expired cache entry")
	}
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestRunCache_GetNonexistent(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetCachedRunResult(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("expected error for nonexistent cache key")
	}
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

func TestRunCache_SaveOverwritesExistingKey(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.SaveCachedRunResult(ctx, &models.CachedRunResult{
		CacheKey: "key-overwrite", AgentID: "agent-cache",
		Output: map[string]interface{}{"v": 1}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveCachedRunResult 1: %v", err)
	}
	if err := s.SaveCachedRunResult(ctx, &models.CachedRunResult{
		CacheKey: "key-overwrite", AgentID: "agent-cache",
		Output: map[string]interface{}{"v": 2}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveCachedRunResult 2: %v", err)
	}

	got, err := s.GetCachedRunResult(ctx, "key-overwrite")
	if err != nil {
		t.Fatal(err)
	}
	if got.Output["v"] != float64(2) {
		t.Errorf("expected overwritten value 2, got %v", got.Output["v"])
	}
}

// TestRunCache_TenantIsolation isn't in the shared conformance suite,
// but run_cache has the same tenant-scoped composite PK design as
// Agents/Registry/Store, so it's worth checking directly: two tenants
// caching the same logical cache_key independently (defense in depth
// even though computeCacheKey already embeds tenant_id upstream).
func TestRunCache_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	now := time.Now().UTC()

	if err := s.SaveCachedRunResult(ctxA, &models.CachedRunResult{
		CacheKey: "shared-key", AgentID: "agent-a",
		Output: map[string]interface{}{"owner": "a"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveCachedRunResult (tenant A): %v", err)
	}

	if _, err := s.GetCachedRunResult(ctxB, "shared-key"); err == nil {
		t.Fatal("tenant B must not see tenant A's cache entry")
	} else if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected ErrNotFound, got %T: %v", err, err)
	}

	if err := s.SaveCachedRunResult(ctxB, &models.CachedRunResult{
		CacheKey: "shared-key", AgentID: "agent-b",
		Output: map[string]interface{}{"owner": "b"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveCachedRunResult (tenant B): %v", err)
	}
	gotA, err := s.GetCachedRunResult(ctxA, "shared-key")
	if err != nil {
		t.Fatalf("GetCachedRunResult (tenant A after B's save): %v", err)
	}
	if gotA.Output["owner"] != "a" {
		t.Errorf("tenant A's cache entry was clobbered by tenant B's save, got owner=%v", gotA.Output["owner"])
	}
}
