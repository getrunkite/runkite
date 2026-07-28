package mysql_test

import (
	"context"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// Mirrors the shared conformance suite's SS-01x/SS-0[1-2]x cases
// (internal/state/conformance's runStoreItemTests) -- hand-written for
// now, same reason as every other file in this package.

func TestStore_PutGetRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	item := &models.StoreItem{
		Namespace: []string{"users", "alice"},
		Key:       "profile",
		Value:     map[string]interface{}{"name": "Alice", "role": "admin"},
	}
	if err := s.PutItem(ctx, item); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	got, err := s.GetItem(ctx, []string{"users", "alice"}, "profile", true)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Value["name"] != "Alice" || got.Value["role"] != "admin" {
		t.Errorf("Value = %+v, want name=Alice role=admin", got.Value)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
	if len(got.Namespace) != 2 || got.Namespace[0] != "users" || got.Namespace[1] != "alice" {
		t.Errorf("Namespace = %v, want [users alice]", got.Namespace)
	}
}

func TestStore_PutOverwrites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ns"}, Key: "k", Value: map[string]interface{}{"v": 1}}); err != nil {
		t.Fatalf("PutItem 1: %v", err)
	}
	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ns"}, Key: "k", Value: map[string]interface{}{"v": 2}}); err != nil {
		t.Fatalf("PutItem 2: %v", err)
	}

	got, err := s.GetItem(ctx, []string{"ns"}, "k", true)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got.Value["v"] != float64(2) {
		t.Errorf("Value.v = %v, want 2", got.Value["v"])
	}
}

func TestStore_GetNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.GetItem(ctx, []string{"ns"}, "does-not-exist", false); err == nil {
		t.Fatal("expected ErrNotFound")
	} else if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected *state.ErrNotFound, got %T: %v", err, err)
	}
}

func TestStore_Delete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ns"}, Key: "del", Value: map[string]interface{}{}}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	if err := s.DeleteItem(ctx, []string{"ns"}, "del"); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if _, err := s.GetItem(ctx, []string{"ns"}, "del", true); err == nil {
		t.Fatal("expected ErrNotFound after delete")
	} else if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected *state.ErrNotFound, got %T: %v", err, err)
	}
}

func TestStore_DeleteNonexistent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.DeleteItem(ctx, []string{"ns"}, "never-existed")
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Fatalf("expected *state.ErrNotFound, got %T: %v", err, err)
	}
}

func TestStore_SearchByNamespacePrefix(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"team-a", "docs"}, Key: "k1", Value: map[string]interface{}{}}))
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"team-a", "notes"}, Key: "k2", Value: map[string]interface{}{}}))
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"team-abc"}, Key: "k3", Value: map[string]interface{}{}}))
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"team-b"}, Key: "k4", Value: map[string]interface{}{}}))

	// Prefix ["team-a"] must match team-a/* but NOT the longer sibling
	// "team-abc" -- the whole reason the namespace encoding wraps each
	// segment in delimiters instead of doing a naive string prefix.
	results, err := s.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"team-a"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("prefix search: got %d items, want 2 (team-a/docs + team-a/notes)", len(results))
		for _, r := range results {
			t.Logf("  ns=%v key=%s", r.Namespace, r.Key)
		}
	}
}

func TestStore_SearchByFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"filter-ns"}, Key: "k1", Value: map[string]interface{}{"status": "active"}}))
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"filter-ns"}, Key: "k2", Value: map[string]interface{}{"status": "inactive"}}))

	results, err := s.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"filter-ns"}, Filter: map[string]interface{}{"status": "active"}, Limit: 10})
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(results) != 1 || results[0].Key != "k1" {
		t.Fatalf("expected only k1 to match status=active, got %d results: %+v", len(results), results)
	}
}

func TestStore_ListNamespaces(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"a", "b"}, Key: "k1", Value: map[string]interface{}{}}))
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"a", "c"}, Key: "k2", Value: map[string]interface{}{}}))
	must(s.PutItem(ctx, &models.StoreItem{Namespace: []string{"a", "b"}, Key: "k3", Value: map[string]interface{}{}}))

	ns, err := s.ListNamespaces(ctx, &models.StoreListNamespacesRequest{Limit: 100})
	if err != nil {
		t.Fatalf("ListNamespaces: %v", err)
	}
	if len(ns) != 2 {
		t.Errorf("got %d namespaces, want 2 (a/b, a/c)", len(ns))
	}
}

func TestStore_LargeValue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	bigStr := make([]byte, 1024*1024) // 1MB
	for i := range bigStr {
		bigStr[i] = byte('A' + (i % 26))
	}
	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"big"}, Key: "large", Value: map[string]interface{}{"data": string(bigStr)}}); err != nil {
		t.Fatalf("PutItem large: %v", err)
	}

	got, err := s.GetItem(ctx, []string{"big"}, "large", true)
	if err != nil {
		t.Fatalf("GetItem large: %v", err)
	}
	if len(got.Value["data"].(string)) != len(bigStr) {
		t.Errorf("large value length = %d, want %d", len(got.Value["data"].(string)), len(bigStr))
	}
}

// TTL tests -- ttlMinutes is a small fraction of a minute so these run
// in well under a second rather than actually waiting real minutes.

func TestStore_TTLItemAccessibleBeforeExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ttl := 10.0 / 60.0 // 10 seconds

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k1", Value: map[string]interface{}{"v": 1}, TTLMinutes: &ttl}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	got, err := s.GetItem(ctx, []string{"ttl"}, "k1", false)
	if err != nil {
		t.Fatalf("GetItem before expiry: %v", err)
	}
	if got.Value["v"] != float64(1) {
		t.Errorf("Value.v = %v, want 1", got.Value["v"])
	}
}

func TestStore_TTLItemExpiresAndReadsAsAbsent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ttl := 0.6 / 60.0 // 0.6 seconds

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k2", Value: map[string]interface{}{}, TTLMinutes: &ttl}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)

	_, err := s.GetItem(ctx, []string{"ttl"}, "k2", false)
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound for expired item, got %v (%T)", err, err)
	}
}

func TestStore_TTLNoTTLMeansNoExpiration(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k3", Value: map[string]interface{}{"v": 1}}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)

	got, err := s.GetItem(ctx, []string{"ttl"}, "k3", false)
	if err != nil {
		t.Fatalf("item with no TTL should never expire: %v", err)
	}
	if got.Value["v"] != float64(1) {
		t.Errorf("Value.v = %v, want 1", got.Value["v"])
	}
}

func TestStore_TTLRefreshOnReadExtendsExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ttl := 1.0 / 60.0 // 1 second

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k4", Value: map[string]interface{}{}, TTLMinutes: &ttl}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	// Read with refreshTTL=true at t=0.5s -- resets the countdown to
	// another full 1s from now.
	time.Sleep(500 * time.Millisecond)
	if _, err := s.GetItem(ctx, []string{"ttl"}, "k4", true); err != nil {
		t.Fatalf("GetItem at t=0.5s (before original expiry): %v", err)
	}

	// t=1.3s: past the ORIGINAL 1s deadline, but well inside the
	// REFRESHED deadline (0.5s + 1s = 1.5s) -- must still be present.
	time.Sleep(800 * time.Millisecond)
	if _, err := s.GetItem(ctx, []string{"ttl"}, "k4", false); err != nil {
		t.Fatalf("item should still be alive past its original TTL because a refreshing read extended it: %v", err)
	}

	// t=1.9s: past the refreshed deadline (1.5s) too -- now expired.
	time.Sleep(600 * time.Millisecond)
	_, err := s.GetItem(ctx, []string{"ttl"}, "k4", false)
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Errorf("expected ErrNotFound once the refreshed TTL also elapses, got %v (%T)", err, err)
	}
}

func TestStore_TTLRefreshFalseDoesNotExtendExpiry(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ttl := 0.8 / 60.0 // 0.8 seconds

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k5", Value: map[string]interface{}{}, TTLMinutes: &ttl}); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	time.Sleep(400 * time.Millisecond)
	if _, err := s.GetItem(ctx, []string{"ttl"}, "k5", false); err != nil {
		t.Fatalf("GetItem at t=0.4s (before expiry): %v", err)
	}

	time.Sleep(600 * time.Millisecond)
	_, err := s.GetItem(ctx, []string{"ttl"}, "k5", false)
	if _, ok := err.(*state.ErrNotFound); !ok {
		t.Errorf("a refreshTTL=false read must not have extended the deadline, expected ErrNotFound, got %v (%T)", err, err)
	}
}

func TestStore_TTLSearchExcludesExpiredItems(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ttl := 0.5 / 60.0 // 0.5 seconds
	no := false

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl-search"}, Key: "alive", Value: map[string]interface{}{}}); err != nil {
		t.Fatalf("PutItem (alive): %v", err)
	}
	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl-search"}, Key: "expiring", Value: map[string]interface{}{}, TTLMinutes: &ttl}); err != nil {
		t.Fatalf("PutItem (expiring): %v", err)
	}
	time.Sleep(1000 * time.Millisecond)

	results, err := s.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"ttl-search"}, Limit: 10, RefreshTTL: &no})
	if err != nil {
		t.Fatalf("SearchItems: %v", err)
	}
	if len(results) != 1 || results[0].Key != "alive" {
		t.Errorf("expected only the non-expired item, got %d results: %+v", len(results), results)
	}
}

func TestStore_PutWithNilTTLClearsExistingTTL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ttl := 0.6 / 60.0 // 0.6 seconds

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "clear", Value: map[string]interface{}{"v": 1}, TTLMinutes: &ttl}); err != nil {
		t.Fatalf("PutItem (with TTL): %v", err)
	}
	// Re-put WITHOUT a TTL -- must clear expiry (LangGraph PutOp.ttl=None
	// semantics: not "leave existing TTL alone").
	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "clear", Value: map[string]interface{}{"v": 2}}); err != nil {
		t.Fatalf("PutItem (clearing TTL): %v", err)
	}
	time.Sleep(1200 * time.Millisecond)

	got, err := s.GetItem(ctx, []string{"ttl"}, "clear", false)
	if err != nil {
		t.Fatalf("item whose TTL was cleared by a nil-ttl put must still be present: %v", err)
	}
	if got.Value["v"] != float64(2) {
		t.Errorf("Value.v = %v, want 2", got.Value["v"])
	}
}

func TestStore_PruneExpiredStoreItemsDeletesRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ttl := 0.5 / 60.0 // 0.5 seconds

	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl-prune"}, Key: "gone", Value: map[string]interface{}{}, TTLMinutes: &ttl}); err != nil {
		t.Fatalf("PutItem (gone): %v", err)
	}
	if err := s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl-prune"}, Key: "keep", Value: map[string]interface{}{}}); err != nil {
		t.Fatalf("PutItem (keep): %v", err)
	}
	time.Sleep(1000 * time.Millisecond)

	n, err := s.PruneExpiredStoreItems(ctx)
	if err != nil {
		t.Fatalf("PruneExpiredStoreItems: %v", err)
	}
	if n < 1 {
		t.Errorf("expected at least 1 expired row pruned, got %d", n)
	}
	if _, err := s.GetItem(ctx, []string{"ttl-prune"}, "keep", false); err != nil {
		t.Fatalf("non-TTL item must survive prune: %v", err)
	}
}

// TestStore_TenantIsolation isn't in the shared conformance suite as a
// dedicated store-item case, but store_items carries the same
// (tenant_id, namespace, key) composite PK design as Agents/Registry
// (two tenants CAN independently own the same namespace+key), so it's
// worth checking directly: tenant B putting under the same
// namespace+key as tenant A must not clobber tenant A's item, and
// tenant B's read/search/delete must never see tenant A's data.
func TestStore_TenantIsolation(t *testing.T) {
	s := newTestStore(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")

	if err := s.PutItem(ctxA, &models.StoreItem{Namespace: []string{"shared-ns"}, Key: "shared-key", Value: map[string]interface{}{"owner": "a"}}); err != nil {
		t.Fatalf("PutItem (tenant A): %v", err)
	}
	if err := s.PutItem(ctxB, &models.StoreItem{Namespace: []string{"shared-ns"}, Key: "shared-key", Value: map[string]interface{}{"owner": "b"}}); err != nil {
		t.Fatalf("PutItem (tenant B): %v", err)
	}

	gotA, err := s.GetItem(ctxA, []string{"shared-ns"}, "shared-key", false)
	if err != nil {
		t.Fatalf("GetItem (tenant A): %v", err)
	}
	if gotA.Value["owner"] != "a" {
		t.Errorf("tenant A's item was clobbered by tenant B's put, got owner=%v", gotA.Value["owner"])
	}

	if err := s.DeleteItem(ctxB, []string{"shared-ns"}, "shared-key"); err != nil {
		t.Fatalf("DeleteItem (tenant B): %v", err)
	}
	if _, err := s.GetItem(ctxA, []string{"shared-ns"}, "shared-key", false); err != nil {
		t.Fatalf("tenant A's item must survive tenant B's delete of its own copy: %v", err)
	}
}
