// Package conformance provides backend-agnostic test suites for the state.Store
// interface. Every backend (SQLite, Postgres, MySQL) must pass this suite.
package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// StoreFactory creates a fresh, initialized Store for each test.
type StoreFactory func(t *testing.T) state.Store

// RunStoreSuite runs all conformance tests.
func RunStoreSuite(t *testing.T, factory StoreFactory) {
	t.Run("agents", func(t *testing.T) { runAgentTests(t, factory) })
	t.Run("threads", func(t *testing.T) { runThreadTests(t, factory) })
	t.Run("runs", func(t *testing.T) { runRunTests(t, factory) })
	t.Run("store_items", func(t *testing.T) { runStoreItemTests(t, factory) })
	t.Run("checkpoints", func(t *testing.T) { runCheckpointTests(t, factory) })
	t.Run("webhook_dead_letters", func(t *testing.T) { runWebhookDeadLetterTests(t, factory) })
	t.Run("run_cache", func(t *testing.T) { runRunCacheTests(t, factory) })
	t.Run("cron_schedules", func(t *testing.T) { runCronScheduleTests(t, factory) })
	t.Run("cascade", func(t *testing.T) { runCascadeTests(t, factory) })
	t.Run("tenant_isolation", func(t *testing.T) { runTenantIsolationTests(t, factory) })
	t.Run("retention", func(t *testing.T) { runRetentionTests(t, factory) })
	t.Run("empty_list_results_are_not_nil", func(t *testing.T) { runEmptyListTests(t, factory) })
}

// --------------------------------------------------------------------------
// Empty list results must be non-nil (JSON: [], not null). SDK clients call
// .map()/for-of on list/search response fields unconditionally; a nil Go
// slice marshals to JSON null and crashes them. Regression for a bug where
// "var items []*models.X" (nil until appended to) leaked into every list
// endpoint's zero-results case across all three backends.
// --------------------------------------------------------------------------

func runEmptyListTests(t *testing.T, factory StoreFactory) {
	ctx := tenant.WithContext(context.Background(), "empty-list-tenant")

	t.Run("SearchAgents", func(t *testing.T) {
		s := factory(t)
		got, err := s.SearchAgents(ctx, &models.AgentSearchRequest{Limit: 10})
		if err != nil {
			t.Fatalf("SearchAgents: %v", err)
		}
		if got == nil {
			t.Fatal("SearchAgents returned nil slice for zero results, want non-nil empty slice")
		}
	})

	t.Run("SearchThreads", func(t *testing.T) {
		s := factory(t)
		got, err := s.SearchThreads(ctx, &models.ThreadSearchRequest{Limit: 10})
		if err != nil {
			t.Fatalf("SearchThreads: %v", err)
		}
		if got == nil {
			t.Fatal("SearchThreads returned nil slice for zero results, want non-nil empty slice")
		}
	})

	t.Run("SearchRuns", func(t *testing.T) {
		s := factory(t)
		got, err := s.SearchRuns(ctx, &models.RunSearchRequest{Limit: 10})
		if err != nil {
			t.Fatalf("SearchRuns: %v", err)
		}
		if got == nil {
			t.Fatal("SearchRuns returned nil slice for zero results, want non-nil empty slice")
		}
	})

	t.Run("SearchItems", func(t *testing.T) {
		s := factory(t)
		got, err := s.SearchItems(ctx, &models.StoreSearchRequest{NamespacePrefix: []string{"nonexistent"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchItems: %v", err)
		}
		if got == nil {
			t.Fatal("SearchItems returned nil slice for zero results, want non-nil empty slice")
		}
	})

	t.Run("ListWebhookDeadLetters", func(t *testing.T) {
		s := factory(t)
		got, err := s.ListWebhookDeadLetters(ctx, 10)
		if err != nil {
			t.Fatalf("ListWebhookDeadLetters: %v", err)
		}
		if got == nil {
			t.Fatal("ListWebhookDeadLetters returned nil slice for zero results, want non-nil empty slice")
		}
	})

	t.Run("ListCronSchedules", func(t *testing.T) {
		s := factory(t)
		got, err := s.ListCronSchedules(ctx)
		if err != nil {
			t.Fatalf("ListCronSchedules: %v", err)
		}
		if got == nil {
			t.Fatal("ListCronSchedules returned nil slice for zero results, want non-nil empty slice")
		}
	})

	t.Run("ListCheckpoints", func(t *testing.T) {
		s := factory(t)
		got, err := s.ListCheckpoints(ctx, "nonexistent-thread", 10, "")
		if err != nil {
			t.Fatalf("ListCheckpoints: %v", err)
		}
		if got == nil {
			t.Fatal("ListCheckpoints returned nil slice for zero results, want non-nil empty slice")
		}
	})

	t.Run("ListNamespaces", func(t *testing.T) {
		s := factory(t)
		got, err := s.ListNamespaces(ctx, &models.StoreListNamespacesRequest{
			Prefix: []string{"nonexistent"},
			Limit:  10,
		})
		if err != nil {
			t.Fatalf("ListNamespaces: %v", err)
		}
		if got == nil {
			t.Fatal("ListNamespaces returned nil slice for zero results, want non-nil empty slice")
		}
	})
}

// --------------------------------------------------------------------------
// Multi-tenancy isolation (master plan: "Multi-tenancy: workspace/org/team
// hierarchy with isolated data")
// --------------------------------------------------------------------------

func runTenantIsolationTests(t *testing.T, factory StoreFactory) {
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	sysCtx := tenant.SystemContext(context.Background())

	t.Run("agents_same_id_different_tenants_dont_collide", func(t *testing.T) {
		s := factory(t)

		// Two tenants independently registering an agent with the exact
		// same human-chosen ID ("echo_agent" is a completely realistic
		// name any tenant might pick) must never collide or overwrite
		// each other -- this is the reason agents' primary key is
		// (tenant_id, agent_id), not agent_id alone.
		if err := s.UpsertAgent(ctxA, &models.Agent{AgentID: "echo_agent", Name: "Tenant A's Echo", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}); err != nil {
			t.Fatalf("UpsertAgent (tenant A): %v", err)
		}
		if err := s.UpsertAgent(ctxB, &models.Agent{AgentID: "echo_agent", Name: "Tenant B's Echo", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}); err != nil {
			t.Fatalf("UpsertAgent (tenant B): %v", err)
		}

		a, err := s.GetAgent(ctxA, "echo_agent")
		if err != nil {
			t.Fatalf("GetAgent (tenant A): %v", err)
		}
		if a.Name != "Tenant A's Echo" {
			t.Errorf("expected tenant A's own agent, got %+v", a)
		}

		b, err := s.GetAgent(ctxB, "echo_agent")
		if err != nil {
			t.Fatalf("GetAgent (tenant B): %v", err)
		}
		if b.Name != "Tenant B's Echo" {
			t.Errorf("expected tenant B's own agent, got %+v", b)
		}
	})

	t.Run("search_agents_never_leaks_across_tenants", func(t *testing.T) {
		s := factory(t)
		s.UpsertAgent(ctxA, &models.Agent{AgentID: "a1", Name: "alpha", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})
		s.UpsertAgent(ctxA, &models.Agent{AgentID: "a2", Name: "beta", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})
		s.UpsertAgent(ctxB, &models.Agent{AgentID: "b1", Name: "gamma", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

		agentsA, err := s.SearchAgents(ctxA, &models.AgentSearchRequest{Limit: 100})
		if err != nil {
			t.Fatalf("SearchAgents (tenant A): %v", err)
		}
		if len(agentsA) != 2 {
			t.Fatalf("expected tenant A to see exactly its own 2 agents, got %d: %+v", len(agentsA), agentsA)
		}
		for _, a := range agentsA {
			if a.AgentID == "b1" {
				t.Fatal("tenant A's search leaked tenant B's agent")
			}
		}
	})

	t.Run("threads_get_returns_not_found_across_tenants", func(t *testing.T) {
		s := factory(t)
		now := time.Now().UTC()
		thread := &models.Thread{ThreadID: "shared-uuid-thread", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateThread(ctxA, thread); err != nil {
			t.Fatalf("CreateThread (tenant A): %v", err)
		}

		// Tenant B guessing/reusing the exact same thread ID must see
		// ErrNotFound, not tenant A's thread -- this is the core isolation
		// guarantee, not just a search-list nicety. Returning NotFound
		// (not Forbidden) also avoids confirming the ID even exists.
		_, err := s.GetThread(ctxB, "shared-uuid-thread")
		var notFound *state.ErrNotFound
		if err == nil {
			t.Fatal("expected tenant B to NOT see tenant A's thread by ID")
		}
		if !isErrNotFound(err, &notFound) {
			t.Errorf("expected ErrNotFound, got %T: %v", err, err)
		}

		// The owning tenant can still read it normally.
		if _, err := s.GetThread(ctxA, "shared-uuid-thread"); err != nil {
			t.Errorf("expected tenant A to read its own thread: %v", err)
		}
	})

	t.Run("threads_delete_across_tenants_is_noop_not_data_loss", func(t *testing.T) {
		s := factory(t)
		now := time.Now().UTC()
		s.CreateThread(ctxA, &models.Thread{ThreadID: "protected-thread", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
		// Child rows must also survive a cross-tenant delete attempt.
		// (Caught a real Mongo bug: children deleted by thread_id before
		// the tenant-filtered parent delete failed.)
		s.CreateRun(ctxA, &models.Run{RunID: "protected-run", ThreadID: "protected-thread", AgentID: "echo_agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

		// Tenant B attempting to delete tenant A's thread (e.g. by
		// guessing/observing the ID) must fail, and -- critically --
		// must NOT actually delete it OR its runs/checkpoints.
		err := s.DeleteThread(ctxB, "protected-thread")
		if err == nil {
			t.Fatal("expected tenant B's delete of tenant A's thread to fail")
		}
		if _, getErr := s.GetThread(ctxA, "protected-thread"); getErr != nil {
			t.Fatalf("tenant A's thread was deleted by tenant B's request: %v", getErr)
		}
		if _, getErr := s.GetRun(ctxA, "protected-run"); getErr != nil {
			t.Fatalf("tenant A's run was deleted by tenant B's cross-tenant DeleteThread: %v", getErr)
		}
	})

	t.Run("runs_isolated_by_tenant", func(t *testing.T) {
		s := factory(t)
		now := time.Now().UTC()
		s.CreateThread(ctxA, &models.Thread{ThreadID: "run-isolation-thread-a", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
		s.CreateRun(ctxA, &models.Run{RunID: "shared-uuid-run", ThreadID: "run-isolation-thread-a", AgentID: "echo_agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

		if _, err := s.GetRun(ctxB, "shared-uuid-run"); err == nil {
			t.Fatal("expected tenant B to NOT see tenant A's run by ID")
		}
		run, err := s.GetRun(ctxA, "shared-uuid-run")
		if err != nil || run.TenantID != "tenant-a" {
			t.Fatalf("expected tenant A to read its own run with TenantID populated, got run=%+v err=%v", run, err)
		}

		// UpdateRunStatus from the wrong tenant context must not silently
		// mutate another tenant's run.
		if err := s.UpdateRunStatus(ctxB, "shared-uuid-run", models.RunStatusError, nil, "tenant B tried to fail tenant A's run"); err != nil {
			t.Fatalf("UpdateRunStatus (cross-tenant, expected a no-op not an error): %v", err)
		}
		stillPending, _ := s.GetRun(ctxA, "shared-uuid-run")
		if stillPending.Status != models.RunStatusPending {
			t.Fatalf("tenant B's cross-tenant UpdateRunStatus mutated tenant A's run: %+v", stillPending)
		}
	})

	t.Run("store_items_same_namespace_key_different_tenants", func(t *testing.T) {
		s := factory(t)
		// Both tenants use the exact same namespace/key -- entirely
		// plausible (e.g. both use ["config"], "settings") -- and must
		// get their OWN value back, not each other's.
		s.PutItem(ctxA, &models.StoreItem{Namespace: []string{"config"}, Key: "settings", Value: map[string]interface{}{"owner": "tenant-a"}})
		s.PutItem(ctxB, &models.StoreItem{Namespace: []string{"config"}, Key: "settings", Value: map[string]interface{}{"owner": "tenant-b"}})

		itemA, err := s.GetItem(ctxA, []string{"config"}, "settings", true)
		if err != nil {
			t.Fatalf("GetItem (tenant A): %v", err)
		}
		if itemA.Value["owner"] != "tenant-a" {
			t.Errorf("expected tenant A's own value, got %+v", itemA.Value)
		}

		itemB, err := s.GetItem(ctxB, []string{"config"}, "settings", true)
		if err != nil {
			t.Fatalf("GetItem (tenant B): %v", err)
		}
		if itemB.Value["owner"] != "tenant-b" {
			t.Errorf("expected tenant B's own value, got %+v", itemB.Value)
		}
	})

	t.Run("search_items_never_leaks_across_tenants", func(t *testing.T) {
		s := factory(t)
		s.PutItem(ctxA, &models.StoreItem{Namespace: []string{"docs"}, Key: "k1", Value: map[string]interface{}{}})
		s.PutItem(ctxB, &models.StoreItem{Namespace: []string{"docs"}, Key: "k2", Value: map[string]interface{}{}})

		items, err := s.SearchItems(ctxA, &models.StoreSearchRequest{Limit: 100})
		if err != nil {
			t.Fatalf("SearchItems (tenant A): %v", err)
		}
		if len(items) != 1 || items[0].Key != "k1" {
			t.Fatalf("expected tenant A to see only its own item, got %+v", items)
		}
	})

	t.Run("cron_schedules_same_name_different_tenants", func(t *testing.T) {
		s := factory(t)
		// Both tenants naming their schedule "daily-report" is the
		// realistic case this composite key exists for.
		s.UpsertCronSchedule(ctxA, &models.CronSchedule{Name: "daily-report", AgentID: "agent-a", Expression: "0 9 * * *", Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`), Enabled: true})
		s.UpsertCronSchedule(ctxB, &models.CronSchedule{Name: "daily-report", AgentID: "agent-b", Expression: "0 10 * * *", Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`), Enabled: true})

		listA, err := s.ListCronSchedules(ctxA)
		if err != nil {
			t.Fatalf("ListCronSchedules (tenant A): %v", err)
		}
		if len(listA) != 1 || listA[0].AgentID != "agent-a" {
			t.Fatalf("expected tenant A to see only its own schedule, got %+v", listA)
		}

		// Fire claims are ALSO tenant-scoped: both tenants can
		// independently claim the same fire_time for their own
		// same-named schedule without treading on each other.
		fireTime := time.Now().UTC().Truncate(time.Minute)
		wonA, errA := s.TryClaimCronFire(ctxA, "daily-report", fireTime)
		wonB, errB := s.TryClaimCronFire(ctxB, "daily-report", fireTime)
		if errA != nil || errB != nil || !wonA || !wonB {
			t.Fatalf("expected both tenants to independently claim the same fire_time for their own schedule: wonA=%v errA=%v wonB=%v errB=%v", wonA, errA, wonB, errB)
		}
	})

	t.Run("run_cache_isolated_by_tenant", func(t *testing.T) {
		s := factory(t)
		now := time.Now().UTC()
		// Deliberately reuse the SAME raw cache_key across tenants to
		// prove both the tenant_id WHERE filter AND that a save from B
		// cannot overwrite A's row (the real bug when cache_key was the
		// sole PRIMARY KEY + ON CONFLICT(cache_key): B's save updated
		// A's output in place while leaving tenant_id=A).
		if err := s.SaveCachedRunResult(ctxA, &models.CachedRunResult{CacheKey: "shared-key", AgentID: "echo_agent", Output: map[string]interface{}{"from": "tenant-a"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatalf("SaveCachedRunResult (tenant A): %v", err)
		}
		if err := s.SaveCachedRunResult(ctxB, &models.CachedRunResult{CacheKey: "shared-key", AgentID: "echo_agent", Output: map[string]interface{}{"from": "tenant-b"}, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatalf("SaveCachedRunResult (tenant B): %v", err)
		}

		if _, err := s.GetCachedRunResult(ctxB, "shared-key"); err != nil {
			t.Fatalf("expected tenant B to read its own cache entry: %v", err)
		}
		gotB, _ := s.GetCachedRunResult(ctxB, "shared-key")
		if gotB.Output["from"] != "tenant-b" {
			t.Fatalf("expected tenant B's output, got %+v", gotB.Output)
		}
		gotA, err := s.GetCachedRunResult(ctxA, "shared-key")
		if err != nil || gotA.Output["from"] != "tenant-a" {
			t.Fatalf("expected tenant A to still have its own output (not overwritten by B), got %+v err=%v", gotA, err)
		}
	})

	t.Run("system_context_bypasses_tenant_filter", func(t *testing.T) {
		s := factory(t)
		now := time.Now().UTC()
		s.CreateThread(ctxA, &models.Thread{ThreadID: "system-visible-thread", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

		// A system context (internal, trusted, control-plane-driven
		// callers only -- see internal/tenant's SystemContext doc) can
		// see the thread regardless of which tenant created it, exactly
		// like the gRPC bridge's StatusCallback needs to.
		if _, err := s.GetThread(sysCtx, "system-visible-thread"); err != nil {
			t.Fatalf("expected system context to see any tenant's thread: %v", err)
		}
	})
}

// isErrNotFound is a small helper so the test above doesn't need
// errors.As boilerplate inline.
func isErrNotFound(err error, target **state.ErrNotFound) bool {
	nf, ok := err.(*state.ErrNotFound)
	if ok {
		*target = nf
	}
	return ok
}

// --------------------------------------------------------------------------
// Agents (SS-009, AG-001..005)
// --------------------------------------------------------------------------

func runAgentTests(t *testing.T, factory StoreFactory) {
	t.Run("SS-009_upsert_get_roundtrip", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		agent := &models.Agent{
			AgentID:      "agent-001",
			Name:         "echo",
			Description:  "Echo agent",
			Metadata:     map[string]interface{}{"env": "test"},
			Capabilities: map[string]interface{}{"ap.io.streaming": true},
		}
		if err := s.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent: %v", err)
		}

		got, err := s.GetAgent(ctx, "agent-001")
		if err != nil {
			t.Fatalf("GetAgent: %v", err)
		}
		if got.Name != "echo" {
			t.Errorf("Name = %q, want %q", got.Name, "echo")
		}
		if got.Description != "Echo agent" {
			t.Errorf("Description = %q, want %q", got.Description, "Echo agent")
		}
		if got.Version != 1 {
			t.Errorf("Version = %d, want 1 on first insert", got.Version)
		}
	})

	t.Run("SS-009a_version_bumps_only_on_change", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		agent := &models.Agent{
			AgentID:      "agent-ver",
			Name:         "versioned",
			Metadata:     map[string]interface{}{"env": "test"},
			Capabilities: map[string]interface{}{},
		}
		if err := s.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent (initial): %v", err)
		}
		got, _ := s.GetAgent(ctx, "agent-ver")
		if got.Version != 1 {
			t.Fatalf("Version = %d, want 1 after first upsert", got.Version)
		}

		// Re-upserting an UNCHANGED definition (the bootstrap-on-every-
		// restart case) must NOT bump the version.
		if err := s.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent (unchanged): %v", err)
		}
		got, _ = s.GetAgent(ctx, "agent-ver")
		if got.Version != 1 {
			t.Errorf("Version = %d after re-upserting an unchanged agent, want 1 (unchanged)", got.Version)
		}

		// Upserting a CHANGED definition must bump the version.
		agent.Description = "now has a description"
		if err := s.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent (changed): %v", err)
		}
		got, _ = s.GetAgent(ctx, "agent-ver")
		if got.Version != 2 {
			t.Errorf("Version = %d after a real change, want 2", got.Version)
		}

		// Changing metadata (not just top-level fields) must also bump it.
		agent.Metadata["env"] = "prod"
		if err := s.UpsertAgent(ctx, agent); err != nil {
			t.Fatalf("UpsertAgent (metadata changed): %v", err)
		}
		got, _ = s.GetAgent(ctx, "agent-ver")
		if got.Version != 3 {
			t.Errorf("Version = %d after a metadata change, want 3", got.Version)
		}
	})

	t.Run("SS-009b_get_nonexistent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		_, err := s.GetAgent(ctx, "does-not-exist")
		if err == nil {
			t.Fatal("expected error for nonexistent agent")
		}
		if _, ok := err.(*state.ErrNotFound); !ok {
			t.Errorf("expected ErrNotFound, got %T: %v", err, err)
		}
	})

	t.Run("SS-009c_search", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		for _, name := range []string{"alpha", "beta", "gamma"} {
			s.UpsertAgent(ctx, &models.Agent{
				AgentID: "agent-" + name, Name: name,
				Capabilities: map[string]interface{}{},
			})
		}

		results, err := s.SearchAgents(ctx, &models.AgentSearchRequest{Limit: 10})
		if err != nil {
			t.Fatalf("SearchAgents: %v", err)
		}
		if len(results) != 3 {
			t.Errorf("got %d agents, want 3", len(results))
		}

		// Search by name
		results, err = s.SearchAgents(ctx, &models.AgentSearchRequest{Name: "bet", Limit: 10})
		if err != nil {
			t.Fatalf("SearchAgents(name): %v", err)
		}
		if len(results) != 1 || results[0].Name != "beta" {
			t.Errorf("name search: got %d results, want 1 (beta)", len(results))
		}
	})
}

// --------------------------------------------------------------------------
// Threads (SS-001..005)
// --------------------------------------------------------------------------

func runThreadTests(t *testing.T, factory StoreFactory) {
	t.Run("SS-001_create_get_roundtrip", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		thread := &models.Thread{
			ThreadID:  "thread-001",
			Status:    models.ThreadStatusIdle,
			Metadata:  map[string]interface{}{"purpose": "test"},
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.CreateThread(ctx, thread); err != nil {
			t.Fatalf("CreateThread: %v", err)
		}

		got, err := s.GetThread(ctx, "thread-001")
		if err != nil {
			t.Fatalf("GetThread: %v", err)
		}
		if got.Status != models.ThreadStatusIdle {
			t.Errorf("Status = %q, want %q", got.Status, models.ThreadStatusIdle)
		}
		if got.Metadata["purpose"] != "test" {
			t.Errorf("Metadata.purpose = %v, want 'test'", got.Metadata["purpose"])
		}
	})

	t.Run("SS-002_duplicate_create_conflict", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		thread := &models.Thread{ThreadID: "thread-dup", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateThread(ctx, thread); err != nil {
			t.Fatalf("first create: %v", err)
		}

		err := s.CreateThread(ctx, thread)
		if err == nil {
			t.Fatal("expected conflict error on duplicate create")
		}
		if _, ok := err.(*state.ErrConflict); !ok {
			t.Errorf("expected ErrConflict, got %T: %v", err, err)
		}
	})

	t.Run("SS-003_search_all", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		for i := 0; i < 3; i++ {
			s.CreateThread(ctx, &models.Thread{
				ThreadID: fmt.Sprintf("thread-%d", i), Status: models.ThreadStatusIdle,
				CreatedAt: now, UpdatedAt: now,
			})
		}

		results, err := s.SearchThreads(ctx, &models.ThreadSearchRequest{Limit: 10})
		if err != nil {
			t.Fatalf("SearchThreads: %v", err)
		}
		if len(results) != 3 {
			t.Errorf("got %d threads, want 3", len(results))
		}
	})

	t.Run("SS-004_search_by_status", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-idle", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-busy", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now})

		busyStatus := models.ThreadStatusBusy
		results, err := s.SearchThreads(ctx, &models.ThreadSearchRequest{Status: &busyStatus, Limit: 10})
		if err != nil {
			t.Fatalf("SearchThreads(busy): %v", err)
		}
		if len(results) != 1 || results[0].ThreadID != "t-busy" {
			t.Errorf("status filter: got %d results, want 1 (t-busy)", len(results))
		}
	})

	t.Run("SS-005_delete", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-del", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		if err := s.DeleteThread(ctx, "t-del"); err != nil {
			t.Fatalf("DeleteThread: %v", err)
		}
		_, err := s.GetThread(ctx, "t-del")
		if err == nil {
			t.Fatal("expected not-found after delete")
		}
	})

	t.Run("SS-005b_delete_nonexistent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		err := s.DeleteThread(ctx, "does-not-exist")
		if _, ok := err.(*state.ErrNotFound); !ok {
			t.Errorf("expected ErrNotFound, got %T: %v", err, err)
		}
	})

	t.Run("SS-update_thread_merge", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{
			ThreadID: "t-upd", Status: models.ThreadStatusIdle,
			Metadata:  map[string]interface{}{"a": "1", "b": "2"},
			CreatedAt: now, UpdatedAt: now,
		})

		got, err := s.UpdateThread(ctx, "t-upd", &models.ThreadPatch{
			Metadata: map[string]interface{}{"b": "updated", "c": "3"},
		})
		if err != nil {
			t.Fatalf("UpdateThread: %v", err)
		}
		// a should be preserved, b updated, c added
		if got.Metadata["a"] != "1" {
			t.Errorf("a = %v, want '1'", got.Metadata["a"])
		}
		if got.Metadata["b"] != "updated" {
			t.Errorf("b = %v, want 'updated'", got.Metadata["b"])
		}
		if got.Metadata["c"] != "3" {
			t.Errorf("c = %v, want '3'", got.Metadata["c"])
		}
	})
}

// --------------------------------------------------------------------------
// Runs (SS-006..008)
// --------------------------------------------------------------------------

func runRunTests(t *testing.T, factory StoreFactory) {
	t.Run("SS-006_create_get_roundtrip", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		// Create thread first (FK)
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-run", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		run := &models.Run{
			RunID:     "run-001",
			ThreadID:  "t-run",
			AgentID:   "agent-x",
			Status:    models.RunStatusPending,
			Metadata:  map[string]interface{}{"tag": "test"},
			Input:     json.RawMessage(`{"messages":[{"role":"user","content":"hi"}]}`),
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}

		got, err := s.GetRun(ctx, "run-001")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status != models.RunStatusPending {
			t.Errorf("Status = %q, want %q", got.Status, models.RunStatusPending)
		}
		if got.ThreadID != "t-run" {
			t.Errorf("ThreadID = %q, want %q", got.ThreadID, "t-run")
		}
	})

	t.Run("SS-006a_a2a_parent_root_depth_persist", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-a2a", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
		s.CreateRun(ctx, &models.Run{RunID: "a2a-root", ThreadID: "t-a2a", Status: models.RunStatusRunning, CreatedAt: now, UpdatedAt: now})

		parentID := "a2a-root"
		rootID := "a2a-root"
		child := &models.Run{
			RunID: "a2a-child", ThreadID: "t-a2a", Status: models.RunStatusPending,
			CreatedAt: now, UpdatedAt: now,
			ParentRunID: &parentID, RootRunID: &rootID, Depth: 1,
		}
		if err := s.CreateRun(ctx, child); err != nil {
			t.Fatalf("CreateRun (delegated): %v", err)
		}

		// Round-trip via GetRun
		got, err := s.GetRun(ctx, "a2a-child")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.ParentRunID == nil || *got.ParentRunID != "a2a-root" {
			t.Errorf("GetRun: ParentRunID = %+v, want a2a-root", got.ParentRunID)
		}
		if got.RootRunID == nil || *got.RootRunID != "a2a-root" {
			t.Errorf("GetRun: RootRunID = %+v, want a2a-root", got.RootRunID)
		}
		if got.Depth != 1 {
			t.Errorf("GetRun: Depth = %d, want 1", got.Depth)
		}

		// Round-trip via SearchRuns too -- a real gap found before: some
		// backends' list-query column sets have drifted from their
		// single-row-get counterparts (see master plan's "SearchRuns's
		// SELECT once omitted tenant_id" regression note).
		results, err := s.SearchRuns(ctx, &models.RunSearchRequest{ThreadID: "t-a2a", Limit: 10})
		if err != nil {
			t.Fatalf("SearchRuns: %v", err)
		}
		var foundChild bool
		for _, r := range results {
			if r.RunID == "a2a-child" {
				foundChild = true
				if r.ParentRunID == nil || *r.ParentRunID != "a2a-root" || r.Depth != 1 {
					t.Errorf("SearchRuns: parent/depth not persisted, got parent=%+v depth=%d", r.ParentRunID, r.Depth)
				}
			}
			if r.RunID == "a2a-root" && (r.ParentRunID != nil || r.Depth != 0) {
				t.Errorf("SearchRuns: top-level run should have nil parent and depth 0, got parent=%+v depth=%d", r.ParentRunID, r.Depth)
			}
		}
		if !foundChild {
			t.Fatal("SearchRuns: delegated run not found in results")
		}
	})

	t.Run("SS-007_update_status", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-st", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
		s.CreateRun(ctx, &models.Run{RunID: "run-st", ThreadID: "t-st", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now})

		if err := s.UpdateRunStatus(ctx, "run-st", models.RunStatusSuccess, []byte(`{"result":"ok"}`), ""); err != nil {
			t.Fatalf("UpdateRunStatus: %v", err)
		}

		got, err := s.GetRun(ctx, "run-st")
		if err != nil {
			t.Fatalf("GetRun after update: %v", err)
		}
		if got.Status != models.RunStatusSuccess {
			t.Errorf("Status = %q, want %q", got.Status, models.RunStatusSuccess)
		}
	})

	t.Run("SS-008_search_by_status_and_thread", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-search", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
		s.CreateRun(ctx, &models.Run{RunID: "r-1", ThreadID: "t-search", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now})
		s.CreateRun(ctx, &models.Run{RunID: "r-2", ThreadID: "t-search", Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now})

		pending := models.RunStatusPending
		results, err := s.SearchRuns(ctx, &models.RunSearchRequest{ThreadID: "t-search", Status: &pending, Limit: 10})
		if err != nil {
			t.Fatalf("SearchRuns: %v", err)
		}
		if len(results) != 1 || results[0].RunID != "r-1" {
			t.Errorf("expected 1 pending run (r-1), got %d", len(results))
		}
	})
}

// --------------------------------------------------------------------------
// Store Items (SS-010..016)
// --------------------------------------------------------------------------

func runStoreItemTests(t *testing.T, factory StoreFactory) {
	t.Run("SS-010_put_get_roundtrip", func(t *testing.T) {
		s := factory(t)
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
		if got.Value["name"] != "Alice" {
			t.Errorf("Value.name = %v, want 'Alice'", got.Value["name"])
		}
		if got.CreatedAt.IsZero() {
			t.Error("CreatedAt should not be zero")
		}
	})

	t.Run("SS-011_put_overwrites", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ns"}, Key: "k", Value: map[string]interface{}{"v": 1}})
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ns"}, Key: "k", Value: map[string]interface{}{"v": 2}})

		got, _ := s.GetItem(ctx, []string{"ns"}, "k", true)
		if got.Value["v"] != float64(2) {
			t.Errorf("Value.v = %v, want 2", got.Value["v"])
		}
	})

	t.Run("SS-012_delete", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ns"}, Key: "del", Value: map[string]interface{}{}})

		if err := s.DeleteItem(ctx, []string{"ns"}, "del"); err != nil {
			t.Fatalf("DeleteItem: %v", err)
		}
		_, err := s.GetItem(ctx, []string{"ns"}, "del", true)
		if _, ok := err.(*state.ErrNotFound); !ok {
			t.Errorf("expected ErrNotFound after delete, got %T", err)
		}
	})

	t.Run("SS-013_search_by_namespace_prefix", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"team-a", "docs"}, Key: "k1", Value: map[string]interface{}{}})
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"team-a", "notes"}, Key: "k2", Value: map[string]interface{}{}})
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"team-abc"}, Key: "k3", Value: map[string]interface{}{}})
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"team-b"}, Key: "k4", Value: map[string]interface{}{}})

		// Search prefix ["team-a"] should match team-a/* but NOT team-abc
		results, err := s.SearchItems(ctx, &models.StoreSearchRequest{
			NamespacePrefix: []string{"team-a"},
			Limit:           10,
		})
		if err != nil {
			t.Fatalf("SearchItems: %v", err)
		}
		if len(results) != 2 {
			t.Errorf("prefix search: got %d items, want 2 (team-a/docs + team-a/notes)", len(results))
			for _, r := range results {
				t.Logf("  ns=%v key=%s", r.Namespace, r.Key)
			}
		}
	})

	t.Run("SS-014_list_namespaces", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"a", "b"}, Key: "k1", Value: map[string]interface{}{}})
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"a", "c"}, Key: "k2", Value: map[string]interface{}{}})
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"a", "b"}, Key: "k3", Value: map[string]interface{}{}})

		ns, err := s.ListNamespaces(ctx, &models.StoreListNamespacesRequest{Limit: 100})
		if err != nil {
			t.Fatalf("ListNamespaces: %v", err)
		}
		if len(ns) != 2 {
			t.Errorf("got %d namespaces, want 2 (a/b, a/c)", len(ns))
		}
	})

	t.Run("SS-016_large_value", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		// 1MB value
		bigStr := make([]byte, 1024*1024)
		for i := range bigStr {
			bigStr[i] = byte('A' + (i % 26))
		}
		s.PutItem(ctx, &models.StoreItem{
			Namespace: []string{"big"},
			Key:       "large",
			Value:     map[string]interface{}{"data": string(bigStr)},
		})

		got, err := s.GetItem(ctx, []string{"big"}, "large", true)
		if err != nil {
			t.Fatalf("GetItem large: %v", err)
		}
		if len(got.Value["data"].(string)) != len(bigStr) {
			t.Errorf("large value length = %d, want %d", len(got.Value["data"].(string)), len(bigStr))
		}
	})

	// SS-017..022: TTL support (real gap: store.aput(..., ttl=...) hard-
	// failed with "TTL is not supported by RunkiteStore" because the
	// documented LangGraph BaseStore feature was never implemented).
	// ttlMinutes here is a small fraction of a minute so these tests run
	// in well under a second rather than actually waiting real minutes.

	t.Run("SS-017_ttl_item_accessible_before_expiry", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		ttl := 10.0 / 60.0 // 10 seconds

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k1", Value: map[string]interface{}{"v": 1}, TTLMinutes: &ttl})

		got, err := s.GetItem(ctx, []string{"ttl"}, "k1", false)
		if err != nil {
			t.Fatalf("GetItem before expiry: %v", err)
		}
		if got.Value["v"] != float64(1) {
			t.Errorf("Value.v = %v, want 1", got.Value["v"])
		}
	})

	t.Run("SS-018_ttl_item_expires_and_reads_as_absent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		ttl := 0.6 / 60.0 // 0.6 seconds

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k2", Value: map[string]interface{}{}, TTLMinutes: &ttl})
		time.Sleep(1200 * time.Millisecond)

		_, err := s.GetItem(ctx, []string{"ttl"}, "k2", false)
		if _, ok := err.(*state.ErrNotFound); !ok {
			t.Errorf("expected ErrNotFound for expired item, got %v (%T)", err, err)
		}
	})

	t.Run("SS-019_ttl_no_ttl_means_no_expiration", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		// No TTLMinutes at all -- must never expire, regardless of how
		// long the test suite takes to reach this point.
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k3", Value: map[string]interface{}{"v": 1}})
		time.Sleep(1200 * time.Millisecond)

		got, err := s.GetItem(ctx, []string{"ttl"}, "k3", false)
		if err != nil {
			t.Fatalf("item with no TTL should never expire: %v", err)
		}
		if got.Value["v"] != float64(1) {
			t.Errorf("Value.v = %v, want 1", got.Value["v"])
		}
	})

	t.Run("SS-020_ttl_refresh_on_read_extends_expiry", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		ttl := 1.0 / 60.0 // 1 second

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k4", Value: map[string]interface{}{}, TTLMinutes: &ttl})

		// Read with refreshTTL=true at t=0.5s (well inside the original
		// 1s window) -- resets the countdown to another full 1s from now.
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
	})

	t.Run("SS-021_ttl_refresh_false_does_not_extend_expiry", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		ttl := 0.8 / 60.0 // 0.8 seconds

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "k5", Value: map[string]interface{}{}, TTLMinutes: &ttl})

		// Read with refreshTTL=false partway through -- must NOT push
		// the deadline out.
		time.Sleep(400 * time.Millisecond)
		if _, err := s.GetItem(ctx, []string{"ttl"}, "k5", false); err != nil {
			t.Fatalf("GetItem at t=0.4s (before expiry): %v", err)
		}

		// t=1.0s: past the ORIGINAL 0.8s deadline, which a
		// refreshTTL=false read should not have moved.
		time.Sleep(600 * time.Millisecond)
		_, err := s.GetItem(ctx, []string{"ttl"}, "k5", false)
		if _, ok := err.(*state.ErrNotFound); !ok {
			t.Errorf("a refreshTTL=false read must not have extended the deadline, expected ErrNotFound, got %v (%T)", err, err)
		}
	})

	t.Run("SS-022_ttl_search_excludes_expired_items", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		ttl := 0.5 / 60.0 // 0.5 seconds
		no := false

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl-search"}, Key: "alive", Value: map[string]interface{}{}})
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl-search"}, Key: "expiring", Value: map[string]interface{}{}, TTLMinutes: &ttl})
		time.Sleep(1000 * time.Millisecond)

		results, err := s.SearchItems(ctx, &models.StoreSearchRequest{
			NamespacePrefix: []string{"ttl-search"},
			Limit:           10,
			RefreshTTL:      &no,
		})
		if err != nil {
			t.Fatalf("SearchItems: %v", err)
		}
		if len(results) != 1 || results[0].Key != "alive" {
			t.Errorf("expected only the non-expired item, got %d results: %+v", len(results), results)
		}
	})

	t.Run("SS-023_put_with_nil_ttl_clears_existing_ttl", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		ttl := 0.6 / 60.0 // 0.6 seconds

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "clear", Value: map[string]interface{}{"v": 1}, TTLMinutes: &ttl})
		// Re-put WITHOUT a TTL -- must clear expiry (LangGraph PutOp.ttl=None
		// semantics: not "leave existing TTL alone").
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl"}, Key: "clear", Value: map[string]interface{}{"v": 2}})
		time.Sleep(1200 * time.Millisecond)

		got, err := s.GetItem(ctx, []string{"ttl"}, "clear", false)
		if err != nil {
			t.Fatalf("item whose TTL was cleared by a nil-ttl put must still be present: %v", err)
		}
		if got.Value["v"] != float64(2) {
			t.Errorf("Value.v = %v, want 2", got.Value["v"])
		}
	})

	t.Run("SS-024_prune_expired_store_items_deletes_rows", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		ttl := 0.5 / 60.0 // 0.5 seconds

		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl-prune"}, Key: "gone", Value: map[string]interface{}{}, TTLMinutes: &ttl})
		s.PutItem(ctx, &models.StoreItem{Namespace: []string{"ttl-prune"}, Key: "keep", Value: map[string]interface{}{}})
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
	})
}

// --------------------------------------------------------------------------
// Checkpoints (CP-001..005)
// --------------------------------------------------------------------------

func runCheckpointTests(t *testing.T, factory StoreFactory) {
	t.Run("CP-001_save_get_roundtrip", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-cp1", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		created := now.Format(time.RFC3339)
		ts := &models.ThreadState{
			Values: map[string]interface{}{"messages": []interface{}{"hello"}},
			Next:   []string{},
			Checkpoint: models.ThreadCheckpoint{
				ThreadID:     "t-cp1",
				CheckpointNS: "",
				CheckpointID: "cp-001",
			},
			Metadata:   map[string]interface{}{"source": "test"},
			CreatedAt:  &created,
			Tasks:      []interface{}{},
			Interrupts: []interface{}{},
		}
		if err := s.SaveCheckpoint(ctx, "t-cp1", ts); err != nil {
			t.Fatalf("SaveCheckpoint: %v", err)
		}

		got, err := s.GetLatestCheckpoint(ctx, "t-cp1")
		if err != nil {
			t.Fatalf("GetLatestCheckpoint: %v", err)
		}
		if got.Checkpoint.CheckpointID != "cp-001" {
			t.Errorf("CheckpointID = %q, want %q", got.Checkpoint.CheckpointID, "cp-001")
		}
		if got.Checkpoint.ThreadID != "t-cp1" {
			t.Errorf("ThreadID = %q, want %q", got.Checkpoint.ThreadID, "t-cp1")
		}
		if got.Values["messages"] == nil {
			t.Error("Values.messages should not be nil")
		}
	})

	t.Run("CP-002_get_latest_returns_newest", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-cp2", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		for i, id := range []string{"cp-a", "cp-b", "cp-c"} {
			ts := time.Now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			s.SaveCheckpoint(ctx, "t-cp2", &models.ThreadState{
				Values:     map[string]interface{}{"step": id},
				Next:       []string{},
				Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp2", CheckpointNS: "", CheckpointID: id},
				Metadata:   map[string]interface{}{},
				CreatedAt:  &ts,
				Tasks:      []interface{}{},
				Interrupts: []interface{}{},
			})
			time.Sleep(10 * time.Millisecond) // ensure distinct timestamps
		}

		got, err := s.GetLatestCheckpoint(ctx, "t-cp2")
		if err != nil {
			t.Fatalf("GetLatestCheckpoint: %v", err)
		}
		if got.Checkpoint.CheckpointID != "cp-c" {
			t.Errorf("latest = %q, want %q", got.Checkpoint.CheckpointID, "cp-c")
		}
	})

	t.Run("CP-003_list_ordering_and_limit", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-cp3", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		for i := 0; i < 5; i++ {
			ts := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			s.SaveCheckpoint(ctx, "t-cp3", &models.ThreadState{
				Values:     map[string]interface{}{"i": i},
				Next:       []string{},
				Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp3", CheckpointNS: "", CheckpointID: fmt.Sprintf("cp-%d", i)},
				Metadata:   map[string]interface{}{},
				CreatedAt:  &ts,
				Tasks:      []interface{}{},
				Interrupts: []interface{}{},
			})
		}

		// List all
		all, err := s.ListCheckpoints(ctx, "t-cp3", 100, "")
		if err != nil {
			t.Fatalf("ListCheckpoints: %v", err)
		}
		if len(all) != 5 {
			t.Fatalf("got %d checkpoints, want 5", len(all))
		}
		// Newest first
		if all[0].Checkpoint.CheckpointID != "cp-4" {
			t.Errorf("first = %q, want cp-4", all[0].Checkpoint.CheckpointID)
		}

		// Limit
		limited, err := s.ListCheckpoints(ctx, "t-cp3", 2, "")
		if err != nil {
			t.Fatalf("ListCheckpoints(limit=2): %v", err)
		}
		if len(limited) != 2 {
			t.Errorf("got %d, want 2", len(limited))
		}
	})

	t.Run("CP-004_list_before_filter", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-cp4", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		for i := 0; i < 5; i++ {
			ts := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			s.SaveCheckpoint(ctx, "t-cp4", &models.ThreadState{
				Values:     map[string]interface{}{"i": i},
				Next:       []string{},
				Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp4", CheckpointNS: "", CheckpointID: fmt.Sprintf("cpb-%d", i)},
				Metadata:   map[string]interface{}{},
				CreatedAt:  &ts,
				Tasks:      []interface{}{},
				Interrupts: []interface{}{},
			})
		}

		// Before cpb-3 → should get cpb-2, cpb-1, cpb-0
		before, err := s.ListCheckpoints(ctx, "t-cp4", 100, "cpb-3")
		if err != nil {
			t.Fatalf("ListCheckpoints(before): %v", err)
		}
		if len(before) != 3 {
			t.Fatalf("got %d checkpoints before cpb-3, want 3", len(before))
		}
		if before[0].Checkpoint.CheckpointID != "cpb-2" {
			t.Errorf("first before = %q, want cpb-2", before[0].Checkpoint.CheckpointID)
		}
	})

	t.Run("CP-005_parent_checkpoint_tracking", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-cp5", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		ts1 := now.Format(time.RFC3339)
		s.SaveCheckpoint(ctx, "t-cp5", &models.ThreadState{
			Values:     map[string]interface{}{"v": 1},
			Next:       []string{},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp5", CheckpointNS: "", CheckpointID: "parent-cp"},
			Metadata:   map[string]interface{}{},
			CreatedAt:  &ts1,
			Tasks:      []interface{}{},
			Interrupts: []interface{}{},
		})

		ts2 := now.Add(time.Second).Format(time.RFC3339)
		s.SaveCheckpoint(ctx, "t-cp5", &models.ThreadState{
			Values:     map[string]interface{}{"v": 2},
			Next:       []string{"next_node"},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp5", CheckpointNS: "", CheckpointID: "child-cp"},
			Metadata:   map[string]interface{}{},
			CreatedAt:  &ts2,
			ParentCheckpoint: &models.ThreadCheckpoint{
				ThreadID:     "t-cp5",
				CheckpointNS: "",
				CheckpointID: "parent-cp",
			},
			Tasks:      []interface{}{},
			Interrupts: []interface{}{},
		})

		got, err := s.GetLatestCheckpoint(ctx, "t-cp5")
		if err != nil {
			t.Fatalf("GetLatestCheckpoint: %v", err)
		}
		if got.ParentCheckpoint == nil {
			t.Fatal("ParentCheckpoint should not be nil")
		}
		if got.ParentCheckpoint.CheckpointID != "parent-cp" {
			t.Errorf("parent = %q, want parent-cp", got.ParentCheckpoint.CheckpointID)
		}
		if len(got.Next) != 1 || got.Next[0] != "next_node" {
			t.Errorf("Next = %v, want [next_node]", got.Next)
		}
	})

	t.Run("CP-006_get_nonexistent_thread", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		_, err := s.GetLatestCheckpoint(ctx, "nonexistent")
		if err == nil {
			t.Fatal("expected error for nonexistent thread checkpoint")
		}
		if _, ok := err.(*state.ErrNotFound); !ok {
			t.Errorf("expected ErrNotFound, got %T: %v", err, err)
		}
	})

	t.Run("CP-007_cascade_delete_with_thread", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-cp7", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		ts := now.Format(time.RFC3339)
		s.SaveCheckpoint(ctx, "t-cp7", &models.ThreadState{
			Values:     map[string]interface{}{"v": 1},
			Next:       []string{},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-cp7", CheckpointNS: "", CheckpointID: "cp-del"},
			Metadata:   map[string]interface{}{},
			CreatedAt:  &ts,
			Tasks:      []interface{}{},
			Interrupts: []interface{}{},
		})

		if err := s.DeleteThread(ctx, "t-cp7"); err != nil {
			t.Fatalf("DeleteThread: %v", err)
		}

		_, err := s.GetLatestCheckpoint(ctx, "t-cp7")
		if err == nil {
			t.Error("checkpoints should be deleted when thread is deleted (CASCADE)")
		}
	})
}

// --------------------------------------------------------------------------
// Webhook dead-letters (event hooks / webhook delivery)
// --------------------------------------------------------------------------

func runWebhookDeadLetterTests(t *testing.T, factory StoreFactory) {
	t.Run("save_and_list_ordered_newest_first", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		base := time.Now().UTC()
		for i, id := range []string{"dl-1", "dl-2", "dl-3"} {
			dl := &models.WebhookDeadLetter{
				ID:        id,
				URL:       "https://example.com/hook",
				EventType: "run_complete",
				RunID:     "run-" + id,
				Payload:   json.RawMessage(`{"type":"run_complete"}`),
				Error:     "connection refused",
				Attempts:  3,
				FailedAt:  base.Add(time.Duration(i) * time.Second),
			}
			if err := s.SaveWebhookDeadLetter(ctx, dl); err != nil {
				t.Fatalf("SaveWebhookDeadLetter(%s): %v", id, err)
			}
		}

		got, err := s.ListWebhookDeadLetters(ctx, 10)
		if err != nil {
			t.Fatalf("ListWebhookDeadLetters: %v", err)
		}
		if len(got) != 3 {
			t.Fatalf("expected 3 dead letters, got %d", len(got))
		}
		// Newest (dl-3, latest FailedAt) first.
		if got[0].ID != "dl-3" || got[2].ID != "dl-1" {
			t.Errorf("expected newest-first order, got %v, %v, %v", got[0].ID, got[1].ID, got[2].ID)
		}
		if got[0].Attempts != 3 || got[0].Error != "connection refused" || got[0].URL != "https://example.com/hook" {
			t.Errorf("dead letter fields not preserved: %+v", got[0])
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(got[0].Payload, &payload); err != nil || payload["type"] != "run_complete" {
			t.Errorf("payload not preserved: %s (err=%v)", got[0].Payload, err)
		}
	})

	t.Run("list_respects_limit", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		for i := 0; i < 5; i++ {
			s.SaveWebhookDeadLetter(ctx, &models.WebhookDeadLetter{
				ID: fmt.Sprintf("dl-limit-%d", i), URL: "u", EventType: "error", RunID: "r",
				Payload: json.RawMessage(`{}`), FailedAt: time.Now().UTC(),
			})
		}
		got, err := s.ListWebhookDeadLetters(ctx, 2)
		if err != nil {
			t.Fatalf("ListWebhookDeadLetters: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("expected limit=2 to be respected, got %d results", len(got))
		}
	})

	t.Run("list_empty_when_none_saved", func(t *testing.T) {
		s := factory(t)
		got, err := s.ListWebhookDeadLetters(context.Background(), 10)
		if err != nil {
			t.Fatalf("ListWebhookDeadLetters: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("expected no dead letters, got %d", len(got))
		}
	})
}

// --------------------------------------------------------------------------
// Run cache (LLM response caching)
// --------------------------------------------------------------------------

func runRunCacheTests(t *testing.T, factory StoreFactory) {
	t.Run("save_and_get_roundtrip", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		now := time.Now().UTC().Truncate(time.Second)
		result := &models.CachedRunResult{
			CacheKey:  "key-1",
			AgentID:   "agent-cache",
			Output:    map[string]interface{}{"messages": []interface{}{"hi"}},
			CreatedAt: now,
			ExpiresAt: now.Add(1 * time.Hour),
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
	})

	t.Run("expired_entry_is_not_returned", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		now := time.Now().UTC()
		expired := &models.CachedRunResult{
			CacheKey: "key-expired", AgentID: "agent-cache",
			Output:    map[string]interface{}{"v": 1},
			CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-1 * time.Hour), // already expired
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
	})

	t.Run("get_nonexistent", func(t *testing.T) {
		s := factory(t)
		_, err := s.GetCachedRunResult(context.Background(), "does-not-exist")
		if err == nil {
			t.Fatal("expected error for nonexistent cache key")
		}
		if _, ok := err.(*state.ErrNotFound); !ok {
			t.Errorf("expected ErrNotFound, got %T: %v", err, err)
		}
	})

	t.Run("save_overwrites_existing_key", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.SaveCachedRunResult(ctx, &models.CachedRunResult{
			CacheKey: "key-overwrite", AgentID: "agent-cache",
			Output: map[string]interface{}{"v": 1}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		})
		s.SaveCachedRunResult(ctx, &models.CachedRunResult{
			CacheKey: "key-overwrite", AgentID: "agent-cache",
			Output: map[string]interface{}{"v": 2}, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		})

		got, err := s.GetCachedRunResult(ctx, "key-overwrite")
		if err != nil {
			t.Fatal(err)
		}
		if got.Output["v"] != float64(2) {
			t.Errorf("expected overwritten value 2, got %v", got.Output["v"])
		}
	})
}

// --------------------------------------------------------------------------
// Cron scheduler
// --------------------------------------------------------------------------

func runCronScheduleTests(t *testing.T, factory StoreFactory) {
	t.Run("upsert_and_list_roundtrip", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		sched := &models.CronSchedule{
			Name: "daily-report", AgentID: "report-agent", Expression: "0 9 * * *",
			Timezone: "America/New_York", Input: json.RawMessage(`{"type":"daily"}`),
			Config: json.RawMessage(`{}`), Enabled: true,
		}
		if err := s.UpsertCronSchedule(ctx, sched); err != nil {
			t.Fatalf("UpsertCronSchedule: %v", err)
		}

		list, err := s.ListCronSchedules(ctx)
		if err != nil {
			t.Fatalf("ListCronSchedules: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("expected 1 schedule, got %d", len(list))
		}
		got := list[0]
		if got.Name != "daily-report" || got.AgentID != "report-agent" || got.Expression != "0 9 * * *" {
			t.Errorf("schedule fields wrong: %+v", got)
		}
		if got.Timezone != "America/New_York" || !got.Enabled {
			t.Errorf("timezone/enabled wrong: %+v", got)
		}
		var input map[string]interface{}
		json.Unmarshal(got.Input, &input)
		if input["type"] != "daily" {
			t.Errorf("input not preserved: %s", got.Input)
		}
	})

	t.Run("upsert_updates_existing", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		s.UpsertCronSchedule(ctx, &models.CronSchedule{
			Name: "sched-1", AgentID: "agent-a", Expression: "* * * * *", Enabled: true,
			Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`),
		})
		s.UpsertCronSchedule(ctx, &models.CronSchedule{
			Name: "sched-1", AgentID: "agent-b", Expression: "0 * * * *", Enabled: false,
			Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`),
		})

		list, _ := s.ListCronSchedules(ctx)
		if len(list) != 1 {
			t.Fatalf("expected upsert to update, not duplicate: got %d schedules", len(list))
		}
		if list[0].AgentID != "agent-b" || list[0].Expression != "0 * * * *" || list[0].Enabled {
			t.Errorf("expected updated fields, got %+v", list[0])
		}
	})

	t.Run("delete", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		s.UpsertCronSchedule(ctx, &models.CronSchedule{
			Name: "to-delete", AgentID: "a", Expression: "* * * * *",
			Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`),
		})
		if err := s.DeleteCronSchedule(ctx, "to-delete"); err != nil {
			t.Fatalf("DeleteCronSchedule: %v", err)
		}
		list, _ := s.ListCronSchedules(ctx)
		if len(list) != 0 {
			t.Errorf("expected schedule deleted, got %d remaining", len(list))
		}
	})

	t.Run("claim_fire_exactly_once", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		fireTime := time.Now().UTC().Truncate(time.Minute)

		// First claim for this exact (schedule, fire_time) wins.
		won, err := s.TryClaimCronFire(ctx, "sched-x", fireTime)
		if err != nil {
			t.Fatalf("TryClaimCronFire: %v", err)
		}
		if !won {
			t.Fatal("expected the first claim to win")
		}

		// A second attempt for the SAME schedule+fire_time -- simulating a
		// second control-plane instance racing for the same tick -- must
		// lose, not error.
		wonAgain, err := s.TryClaimCronFire(ctx, "sched-x", fireTime)
		if err != nil {
			t.Fatalf("TryClaimCronFire (second attempt): %v", err)
		}
		if wonAgain {
			t.Fatal("expected the second claim for the same (schedule, fire_time) to lose")
		}

		// A DIFFERENT fire_time for the same schedule is an independent
		// claim -- must succeed (this is what makes recurring fires work
		// at all, not just the first one ever).
		nextFireTime := fireTime.Add(time.Minute)
		wonNext, err := s.TryClaimCronFire(ctx, "sched-x", nextFireTime)
		if err != nil {
			t.Fatalf("TryClaimCronFire (next fire time): %v", err)
		}
		if !wonNext {
			t.Fatal("expected a claim for a different fire_time to succeed independently")
		}
	})

	t.Run("claim_independent_per_schedule", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		fireTime := time.Now().UTC().Truncate(time.Minute)

		won1, _ := s.TryClaimCronFire(ctx, "sched-a", fireTime)
		won2, _ := s.TryClaimCronFire(ctx, "sched-b", fireTime)
		if !won1 || !won2 {
			t.Fatalf("expected independent schedules to claim the same fire_time independently: won1=%v won2=%v", won1, won2)
		}
	})

	t.Run("release_allows_reclaim", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		fireTime := time.Now().UTC().Truncate(time.Minute)

		won, err := s.TryClaimCronFire(ctx, "sched-retry", fireTime)
		if err != nil || !won {
			t.Fatalf("initial claim: won=%v err=%v", won, err)
		}
		if err := s.ReleaseCronClaim(ctx, "sched-retry", fireTime); err != nil {
			t.Fatalf("ReleaseCronClaim: %v", err)
		}
		wonAgain, err := s.TryClaimCronFire(ctx, "sched-retry", fireTime)
		if err != nil {
			t.Fatalf("TryClaimCronFire after release: %v", err)
		}
		if !wonAgain {
			t.Fatal("expected claim to succeed again after ReleaseCronClaim")
		}
	})

	t.Run("last_fire_time_none_for_never_claimed", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		_, found, err := s.GetLastCronFireTime(ctx, "never-fired")
		if err != nil {
			t.Fatalf("GetLastCronFireTime: %v", err)
		}
		if found {
			t.Fatal("expected found=false for a schedule with no claims yet")
		}
	})

	t.Run("last_fire_time_returns_most_recent", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		t1 := time.Now().UTC().Truncate(time.Minute)
		t2 := t1.Add(time.Minute)
		t3 := t1.Add(2 * time.Minute)

		// Claim out of order to prove this is a MAX, not "last inserted".
		s.TryClaimCronFire(ctx, "multi-fire", t2)
		s.TryClaimCronFire(ctx, "multi-fire", t1)
		s.TryClaimCronFire(ctx, "multi-fire", t3)

		last, found, err := s.GetLastCronFireTime(ctx, "multi-fire")
		if err != nil {
			t.Fatalf("GetLastCronFireTime: %v", err)
		}
		if !found {
			t.Fatal("expected found=true")
		}
		if !last.Equal(t3) {
			t.Errorf("expected the latest claimed fire_time %v, got %v", t3, last)
		}
	})
}

// --------------------------------------------------------------------------
// Cascade deletes
// --------------------------------------------------------------------------

func runCascadeTests(t *testing.T, factory StoreFactory) {
	t.Run("cascade_delete_thread_deletes_runs", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-cas", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
		s.CreateRun(ctx, &models.Run{RunID: "r-cas-1", ThreadID: "t-cas", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now})
		s.CreateRun(ctx, &models.Run{RunID: "r-cas-2", ThreadID: "t-cas", Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now})

		if err := s.DeleteThread(ctx, "t-cas"); err != nil {
			t.Fatalf("DeleteThread: %v", err)
		}

		// Both runs should be gone
		_, err1 := s.GetRun(ctx, "r-cas-1")
		_, err2 := s.GetRun(ctx, "r-cas-2")
		if err1 == nil || err2 == nil {
			t.Error("runs should be deleted when thread is deleted (CASCADE)")
		}
	})
}

// --------------------------------------------------------------------------
// Retention (master plan gap: no automatic cleanup existed -- runs and
// checkpoints grew unbounded until an explicit DELETE /threads/{id})
// --------------------------------------------------------------------------

func runRetentionTests(t *testing.T, factory StoreFactory) {
	t.Run("PruneRuns_deletes_old_terminal_runs_only", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		cutoff := now.Add(-24 * time.Hour)

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-prune-runs", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		old := now.Add(-48 * time.Hour)
		s.CreateRun(ctx, &models.Run{RunID: "r-old-success", ThreadID: "t-prune-runs", Status: models.RunStatusSuccess, CreatedAt: old, UpdatedAt: old})
		s.CreateRun(ctx, &models.Run{RunID: "r-old-error", ThreadID: "t-prune-runs", Status: models.RunStatusError, CreatedAt: old, UpdatedAt: old})
		s.CreateRun(ctx, &models.Run{RunID: "r-old-interrupted", ThreadID: "t-prune-runs", Status: models.RunStatusInterrupted, CreatedAt: old, UpdatedAt: old})
		s.CreateRun(ctx, &models.Run{RunID: "r-old-timeout", ThreadID: "t-prune-runs", Status: models.RunStatusTimeout, CreatedAt: old, UpdatedAt: old})
		// Old but still pending/running -- must survive regardless of age.
		s.CreateRun(ctx, &models.Run{RunID: "r-old-pending", ThreadID: "t-prune-runs", Status: models.RunStatusPending, CreatedAt: old, UpdatedAt: old})
		s.CreateRun(ctx, &models.Run{RunID: "r-old-running", ThreadID: "t-prune-runs", Status: models.RunStatusRunning, CreatedAt: old, UpdatedAt: old})
		// Recent terminal run -- must survive, newer than cutoff.
		s.CreateRun(ctx, &models.Run{RunID: "r-recent-success", ThreadID: "t-prune-runs", Status: models.RunStatusSuccess, CreatedAt: now, UpdatedAt: now})

		n, err := s.PruneRuns(tenant.SystemContext(ctx), cutoff)
		if err != nil {
			t.Fatalf("PruneRuns: %v", err)
		}
		if n != 4 {
			t.Errorf("expected 4 runs pruned (success/error/interrupted/timeout), got %d", n)
		}

		for _, id := range []string{"r-old-success", "r-old-error", "r-old-interrupted", "r-old-timeout"} {
			if _, err := s.GetRun(ctx, id); err == nil {
				t.Errorf("%s should have been pruned", id)
			}
		}
		if _, err := s.GetRun(ctx, "r-old-pending"); err != nil {
			t.Error("r-old-pending must survive regardless of age -- only terminal statuses are pruned")
		}
		if _, err := s.GetRun(ctx, "r-old-running"); err != nil {
			t.Error("r-old-running must survive regardless of age -- only terminal statuses are pruned")
		}
		if _, err := s.GetRun(ctx, "r-recent-success"); err != nil {
			t.Error("r-recent-success must survive -- newer than the cutoff")
		}
	})

	t.Run("PruneRuns_scoped_to_tenant_unless_system_context", func(t *testing.T) {
		s := factory(t)
		ctxA := tenant.WithContext(context.Background(), "tenant-a")
		ctxB := tenant.WithContext(context.Background(), "tenant-b")
		old := time.Now().UTC().Add(-48 * time.Hour)

		s.CreateThread(ctxA, &models.Thread{ThreadID: "t-a", Status: models.ThreadStatusIdle, CreatedAt: old, UpdatedAt: old})
		s.CreateThread(ctxB, &models.Thread{ThreadID: "t-b", Status: models.ThreadStatusIdle, CreatedAt: old, UpdatedAt: old})
		s.CreateRun(ctxA, &models.Run{RunID: "r-a", ThreadID: "t-a", Status: models.RunStatusSuccess, CreatedAt: old, UpdatedAt: old})
		s.CreateRun(ctxB, &models.Run{RunID: "r-b", ThreadID: "t-b", Status: models.RunStatusSuccess, CreatedAt: old, UpdatedAt: old})

		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		n, err := s.PruneRuns(ctxA, cutoff)
		if err != nil {
			t.Fatalf("PruneRuns (tenant-a scoped): %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 run pruned for tenant-a, got %d", n)
		}
		if _, err := s.GetRun(tenant.SystemContext(context.Background()), "r-a"); err == nil {
			t.Error("r-a (tenant-a) should have been pruned")
		}
		if _, err := s.GetRun(tenant.SystemContext(context.Background()), "r-b"); err != nil {
			t.Error("r-b (tenant-b) must survive -- PruneRuns(ctxA, ...) must not touch other tenants")
		}
	})

	t.Run("PruneCheckpoints_keeps_last_N_per_thread", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-prune-cp", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
		for i, id := range []string{"cp-1", "cp-2", "cp-3", "cp-4", "cp-5"} {
			created := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			if err := s.SaveCheckpoint(ctx, "t-prune-cp", &models.ThreadState{
				Values:     map[string]interface{}{"step": id},
				Next:       []string{},
				Checkpoint: models.ThreadCheckpoint{ThreadID: "t-prune-cp", CheckpointNS: "", CheckpointID: id},
				Metadata:   map[string]interface{}{},
				CreatedAt:  &created,
				Tasks:      []interface{}{},
				Interrupts: []interface{}{},
			}); err != nil {
				t.Fatalf("SaveCheckpoint %s: %v", id, err)
			}
		}

		n, err := s.PruneCheckpoints(tenant.SystemContext(ctx), 2)
		if err != nil {
			t.Fatalf("PruneCheckpoints: %v", err)
		}
		if n != 3 {
			t.Errorf("expected 3 checkpoints pruned (keeping 2 of 5), got %d", n)
		}

		remaining, err := s.ListCheckpoints(ctx, "t-prune-cp", 10, "")
		if err != nil {
			t.Fatalf("ListCheckpoints after prune: %v", err)
		}
		if len(remaining) != 2 {
			t.Fatalf("expected 2 checkpoints remaining, got %d", len(remaining))
		}
		for _, cp := range remaining {
			if cp.Checkpoint.CheckpointID == "cp-1" || cp.Checkpoint.CheckpointID == "cp-2" || cp.Checkpoint.CheckpointID == "cp-3" {
				t.Errorf("expected only the 2 newest checkpoints to survive, found %q", cp.Checkpoint.CheckpointID)
			}
		}
	})

	t.Run("PruneCheckpoints_independent_per_thread", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()

		s.CreateThread(ctx, &models.Thread{ThreadID: "t-busy", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-quiet", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})

		for i := 0; i < 5; i++ {
			created := now.Add(time.Duration(i) * time.Second).Format(time.RFC3339)
			s.SaveCheckpoint(ctx, "t-busy", &models.ThreadState{
				Values: map[string]interface{}{}, Next: []string{},
				Checkpoint: models.ThreadCheckpoint{ThreadID: "t-busy", CheckpointID: fmt.Sprintf("busy-%d", i)},
				Metadata:   map[string]interface{}{}, CreatedAt: &created, Tasks: []interface{}{}, Interrupts: []interface{}{},
			})
		}
		quietCreated := now.Format(time.RFC3339)
		s.SaveCheckpoint(ctx, "t-quiet", &models.ThreadState{
			Values: map[string]interface{}{}, Next: []string{},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-quiet", CheckpointID: "quiet-1"},
			Metadata:   map[string]interface{}{}, CreatedAt: &quietCreated, Tasks: []interface{}{}, Interrupts: []interface{}{},
		})

		if _, err := s.PruneCheckpoints(tenant.SystemContext(ctx), 2); err != nil {
			t.Fatalf("PruneCheckpoints: %v", err)
		}

		busyRemaining, _ := s.ListCheckpoints(ctx, "t-busy", 10, "")
		if len(busyRemaining) != 2 {
			t.Errorf("t-busy: expected 2 checkpoints remaining, got %d", len(busyRemaining))
		}
		quietRemaining, _ := s.ListCheckpoints(ctx, "t-quiet", 10, "")
		if len(quietRemaining) != 1 {
			t.Errorf("t-quiet (only ever had 1): expected 1 checkpoint remaining (never pruned below what it had), got %d", len(quietRemaining))
		}
	})

	t.Run("PruneCronClaims_deletes_old_claims_only", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()

		oldFire := time.Now().UTC().Add(-48 * time.Hour)
		recentFire := time.Now().UTC()
		if _, err := s.TryClaimCronFire(ctx, "sched-old", oldFire); err != nil {
			t.Fatalf("TryClaimCronFire (old): %v", err)
		}
		if _, err := s.TryClaimCronFire(ctx, "sched-recent", recentFire); err != nil {
			t.Fatalf("TryClaimCronFire (recent): %v", err)
		}

		cutoff := time.Now().UTC().Add(-24 * time.Hour)
		n, err := s.PruneCronClaims(tenant.SystemContext(ctx), cutoff)
		if err != nil {
			t.Fatalf("PruneCronClaims: %v", err)
		}
		if n != 1 {
			t.Errorf("expected 1 claim pruned, got %d", n)
		}

		// A pruned claim's fire time should be claimable again (its
		// dedup record is gone); an unpruned one should still be
		// rejected as a duplicate.
		wonAgainOld, err := s.TryClaimCronFire(ctx, "sched-old", oldFire)
		if err != nil {
			t.Fatalf("TryClaimCronFire (old, after prune): %v", err)
		}
		if !wonAgainOld {
			t.Error("expected the pruned claim's fire time to be re-claimable")
		}
		wonAgainRecent, err := s.TryClaimCronFire(ctx, "sched-recent", recentFire)
		if err != nil {
			t.Fatalf("TryClaimCronFire (recent, after prune): %v", err)
		}
		if wonAgainRecent {
			t.Error("expected the unpruned recent claim to still reject a duplicate")
		}
	})

	t.Run("PruneCheckpoints_zero_or_negative_is_a_noop", func(t *testing.T) {
		s := factory(t)
		ctx := context.Background()
		now := time.Now().UTC()
		s.CreateThread(ctx, &models.Thread{ThreadID: "t-noop", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
		created := now.Format(time.RFC3339)
		s.SaveCheckpoint(ctx, "t-noop", &models.ThreadState{
			Values: map[string]interface{}{}, Next: []string{},
			Checkpoint: models.ThreadCheckpoint{ThreadID: "t-noop", CheckpointID: "cp-solo"},
			Metadata:   map[string]interface{}{}, CreatedAt: &created, Tasks: []interface{}{}, Interrupts: []interface{}{},
		})

		for _, keepLast := range []int{0, -1} {
			n, err := s.PruneCheckpoints(tenant.SystemContext(ctx), keepLast)
			if err != nil {
				t.Fatalf("PruneCheckpoints(keepLast=%d): %v", keepLast, err)
			}
			if n != 0 {
				t.Errorf("PruneCheckpoints(keepLast=%d) should be a no-op, pruned %d", keepLast, n)
			}
		}
		remaining, _ := s.ListCheckpoints(ctx, "t-noop", 10, "")
		if len(remaining) != 1 {
			t.Errorf("expected the checkpoint to survive a no-op prune, got %d remaining", len(remaining))
		}
	})
}
