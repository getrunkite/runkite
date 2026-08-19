package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

// TestAdminOverview_AggregatesAcrossTenants proves /admin-api/overview counts
// resources across every tenant, not just whichever tenant an unscoped
// context would resolve to -- the entire point of the Admin API's system
// context.
func TestAdminOverview_AggregatesAcrossTenants(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")

	env.store.UpsertAgent(ctxA, &models.Agent{AgentID: "a1", Name: "a1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})
	env.store.UpsertAgent(ctxB, &models.Agent{AgentID: "b1", Name: "b1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	resp, err := http.Get(env.srv.URL + "/admin-api/overview")
	if err != nil {
		t.Fatalf("GET /admin-api/overview: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var overview struct {
		TotalAgents int `json:"total_agents"`
	}
	json.NewDecoder(resp.Body).Decode(&overview)
	if overview.TotalAgents != 2 {
		t.Errorf("expected 2 agents across both tenants, got %d", overview.TotalAgents)
	}
}

// TestAdminOverview_CountsPastFormerSampleLimit is a regression for the
// fetch-then-len overview bug: Search* with Limit 1000 made totals freeze
// at 1000 under soak load. Overview must keep reporting honest COUNT
// aggregates past that former page size.
func TestAdminOverview_CountsPastFormerSampleLimit(t *testing.T) {
	env := newTestEnv(t)
	ctx := tenant.WithContext(context.Background(), "overview-soak")
	now := time.Now().UTC()
	const n = 1001

	for i := 0; i < n; i++ {
		tid := "t-" + strconv.Itoa(i)
		if err := env.store.CreateThread(ctx, &models.Thread{
			ThreadID: tid, Status: models.ThreadStatusIdle,
			Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateThread %d: %v", i, err)
		}
		if err := env.store.CreateRun(ctx, &models.Run{
			RunID: "r-" + strconv.Itoa(i), ThreadID: tid, AgentID: "echo",
			Status: models.RunStatusSuccess, Metadata: map[string]interface{}{},
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateRun %d: %v", i, err)
		}
	}

	resp, err := http.Get(env.srv.URL + "/admin-api/overview")
	if err != nil {
		t.Fatalf("GET /admin-api/overview: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var overview struct {
		TotalThreads    int            `json:"total_threads"`
		TotalRuns       int            `json:"total_runs"`
		ThreadsByStatus map[string]int `json:"threads_by_status"`
		RunsByStatus    map[string]int `json:"runs_by_status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&overview); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if overview.TotalThreads != n {
		t.Errorf("total_threads = %d, want %d (must not freeze at former Search* sample of 1000)", overview.TotalThreads, n)
	}
	if overview.TotalRuns != n {
		t.Errorf("total_runs = %d, want %d", overview.TotalRuns, n)
	}
	if overview.ThreadsByStatus["idle"] != n {
		t.Errorf("threads_by_status.idle = %d, want %d; map=%v", overview.ThreadsByStatus["idle"], n, overview.ThreadsByStatus)
	}
	if overview.RunsByStatus["success"] != n {
		t.Errorf("runs_by_status.success = %d, want %d; map=%v", overview.RunsByStatus["success"], n, overview.RunsByStatus)
	}
}

// TestAdminListAgents_ExposesTenantID proves the admin view surfaces
// tenant_id even though models.Agent hides it (`json:"-"`) from the
// public Agent Protocol response shape.
func TestAdminListAgents_ExposesTenantID(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	env.store.UpsertAgent(ctxA, &models.Agent{AgentID: "a1", Name: "a1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	resp, err := http.Get(env.srv.URL + "/admin-api/agents")
	if err != nil {
		t.Fatalf("GET /admin-api/agents: %v", err)
	}
	defer resp.Body.Close()

	var agents []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&agents)
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0]["tenant_id"] != "tenant-a" {
		t.Errorf("expected tenant_id=tenant-a visible in the admin view, got %+v", agents[0])
	}
	if agents[0]["agent_id"] != "a1" {
		t.Errorf("expected the underlying agent fields to still be present, got %+v", agents[0])
	}
}

// TestAdminListThreads_SeesAcrossTenantsAndExposesTenantID proves both the
// cross-tenant visibility AND the tenant_id field together for threads.
func TestAdminListThreads_SeesAcrossTenantsAndExposesTenantID(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	ctxB := tenant.WithContext(context.Background(), "tenant-b")
	now := time.Now().UTC()
	env.store.CreateThread(ctxA, &models.Thread{ThreadID: "thread-a", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateThread(ctxB, &models.Thread{ThreadID: "thread-b", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := http.Get(env.srv.URL + "/admin-api/threads")
	if err != nil {
		t.Fatalf("GET /admin-api/threads: %v", err)
	}
	defer resp.Body.Close()

	var threads []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&threads)
	if len(threads) != 2 {
		t.Fatalf("expected 2 threads across both tenants, got %d: %+v", len(threads), threads)
	}
	seenTenants := map[string]bool{}
	for _, th := range threads {
		seenTenants[th["tenant_id"].(string)] = true
	}
	if !seenTenants["tenant-a"] || !seenTenants["tenant-b"] {
		t.Errorf("expected to see both tenant-a and tenant-b, got %+v", seenTenants)
	}
}

// TestAdminGetRun_SeesCrossTenantRunAndExposesTenantID proves a single
// admin run lookup also bypasses tenant scoping and shows tenant_id.
func TestAdminGetRun_SeesCrossTenantRunAndExposesTenantID(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	now := time.Now().UTC()
	env.store.CreateThread(ctxA, &models.Thread{ThreadID: "thread-a", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctxA, &models.Run{RunID: "run-a", ThreadID: "thread-a", AgentID: "echo_agent", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	// The default tenant context (no auth configured in newTestEnv) would
	// normally never see tenant-a's run -- proving this requires an
	// actual cross-tenant lookup, not a same-tenant coincidence.
	resp, err := http.Get(env.srv.URL + "/admin-api/runs/run-a")
	if err != nil {
		t.Fatalf("GET /admin-api/runs/run-a: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var run map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&run)
	if run["tenant_id"] != "tenant-a" {
		t.Errorf("expected tenant_id=tenant-a, got %+v", run)
	}
	if run["run_id"] != "run-a" {
		t.Errorf("expected run_id=run-a, got %+v", run)
	}
}

// TestAdminListThreadRuns_ExposesTenantID proves the per-thread admin
// runs list uses adminRunView (tenant_id visible), not the client-facing
// handleListThreadRuns which hides TenantID via json:"-".
func TestAdminListThreadRuns_ExposesTenantID(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	now := time.Now().UTC()
	env.store.CreateThread(ctxA, &models.Thread{ThreadID: "thread-a", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctxA, &models.Run{RunID: "run-a", ThreadID: "thread-a", AgentID: "echo_agent", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := http.Get(env.srv.URL + "/admin-api/threads/thread-a/runs")
	if err != nil {
		t.Fatalf("GET /admin-api/threads/thread-a/runs: %v", err)
	}
	defer resp.Body.Close()
	var runs []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&runs)
	if len(runs) != 1 || runs[0]["tenant_id"] != "tenant-a" {
		t.Fatalf("expected tenant_id=tenant-a on thread runs list, got %+v", runs)
	}
}

// TestAdminListRuns_ExposesTenantID is a regression test: SearchRuns's
// SELECT once omitted tenant_id (unlike GetRun, which had it from the
// start), so the list view silently showed an empty tenant_id per row
// while the single-run detail view showed it correctly.
func TestAdminListRuns_ExposesTenantID(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	now := time.Now().UTC()
	env.store.CreateThread(ctxA, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctxA, &models.Run{RunID: "r1", ThreadID: "t1", AgentID: "agent-a", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := http.Get(env.srv.URL + "/admin-api/runs")
	if err != nil {
		t.Fatalf("GET /admin-api/runs: %v", err)
	}
	defer resp.Body.Close()
	var runs []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&runs)
	if len(runs) != 1 || runs[0]["tenant_id"] != "tenant-a" {
		t.Fatalf("expected tenant_id=tenant-a in the list view, got %+v", runs)
	}
}

// TestAdminCancelRun_WorksAcrossTenants proves the admin write-action route
// reuses handleCancelRun under system context -- the default (no-tenant)
// caller in newTestEnv can cancel a tenant-a run it would never be able to
// reach via the client-facing /runs/{runID}/cancel route.
func TestAdminCancelRun_WorksAcrossTenants(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	now := time.Now().UTC()
	env.store.CreateThread(ctxA, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctxA, &models.Run{RunID: "r1", ThreadID: "t1", AgentID: "echo_agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := postJSON(env.srv.URL+"/admin-api/runs/r1/cancel", nil)
	if err != nil {
		t.Fatalf("POST /admin-api/runs/r1/cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	run, err := env.store.GetRun(ctxA, "r1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != models.RunStatusInterrupted {
		t.Errorf("expected run status interrupted after admin cancel, got %s", run.Status)
	}
}

// TestAdminDeleteThread_WorksAcrossTenants mirrors the cancel test above
// for the delete-thread write action.
func TestAdminDeleteThread_WorksAcrossTenants(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	now := time.Now().UTC()
	env.store.CreateThread(ctxA, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := deleteReq(env.srv.URL + "/admin-api/threads/t1")
	if err != nil {
		t.Fatalf("DELETE /admin-api/threads/t1: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}

	if _, err := env.store.GetThread(ctxA, "t1"); err == nil {
		t.Error("expected thread to be gone after admin delete, GetThread succeeded")
	}
}

// TestAdminRedeliverWebhook_ReplaysStoredPayload proves redelivery actually
// re-POSTs the dead letter's stored payload to its stored URL.
func TestAdminRedeliverWebhook_ReplaysStoredPayload(t *testing.T) {
	env := newTestEnv(t)
	var receivedBody []byte
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	dl := &models.WebhookDeadLetter{
		ID:        "dl-1",
		URL:       receiver.URL,
		EventType: "run_complete",
		RunID:     "r1",
		Payload:   []byte(`{"type":"run_complete","run_id":"r1"}`),
		Error:     "connection refused",
		Attempts:  3,
		FailedAt:  time.Now().UTC(),
	}
	if err := env.store.SaveWebhookDeadLetter(context.Background(), dl); err != nil {
		t.Fatalf("SaveWebhookDeadLetter: %v", err)
	}

	resp, err := postJSON(env.srv.URL+"/admin-api/webhooks/dead-letters/dl-1/redeliver", nil)
	if err != nil {
		t.Fatalf("POST redeliver: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var result map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&result)
	if result["delivered"] != true {
		t.Errorf("expected delivered=true, got %+v", result)
	}
	if string(receivedBody) != string(dl.Payload) {
		t.Errorf("expected receiver to get the stored payload %s, got %s", dl.Payload, receivedBody)
	}
}

// TestAdminRedeliverWebhook_NotFound proves an unknown dead-letter ID 404s
// instead of panicking or silently succeeding.
func TestAdminRedeliverWebhook_NotFound(t *testing.T) {
	env := newTestEnv(t)

	resp, err := postJSON(env.srv.URL+"/admin-api/webhooks/dead-letters/does-not-exist/redeliver", nil)
	if err != nil {
		t.Fatalf("POST redeliver: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// TestAdminListRuns_FiltersByQueryParams proves the ?status=/?agent_id=
// filters actually reach SearchRuns.
func TestAdminListRuns_FiltersByQueryParams(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "r1", ThreadID: "t1", AgentID: "agent-a", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "r2", ThreadID: "t1", AgentID: "agent-b", Status: models.RunStatusError, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := http.Get(env.srv.URL + "/admin-api/runs?status=success")
	if err != nil {
		t.Fatalf("GET /admin-api/runs?status=success: %v", err)
	}
	defer resp.Body.Close()
	var runs []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&runs)
	if len(runs) != 1 || runs[0]["run_id"] != "r1" {
		t.Fatalf("expected only r1 (status=success), got %+v", runs)
	}
}

// TestAdminListRuns_FiltersByParentAndRootRunID proves the Admin runs list
// exposes the same A2A delegation filters (?parent_run_id=&root_run_id=)
// as the client-facing POST /runs/search, so the Admin UI's Run Detail
// "Agent-to-agent" panel can list a run's direct children.
func TestAdminListRuns_FiltersByParentAndRootRunID(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	root := "root-run"
	env.store.CreateRun(ctx, &models.Run{RunID: root, ThreadID: "t1", AgentID: "coordinator", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "child-1", ThreadID: "t1", AgentID: "worker", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, ParentRunID: &root, RootRunID: &root, Depth: 1, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "child-2", ThreadID: "t1", AgentID: "worker", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, ParentRunID: &root, RootRunID: &root, Depth: 1, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "unrelated", ThreadID: "t1", AgentID: "solo", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	byParent, err := http.Get(env.srv.URL + "/admin-api/runs?parent_run_id=" + root)
	if err != nil {
		t.Fatalf("GET ?parent_run_id=: %v", err)
	}
	defer byParent.Body.Close()
	var parentRuns []map[string]interface{}
	json.NewDecoder(byParent.Body).Decode(&parentRuns)
	if len(parentRuns) != 2 {
		t.Fatalf("expected 2 direct children of %s, got %+v", root, parentRuns)
	}
	for _, r := range parentRuns {
		if r["parent_run_id"] != root {
			t.Errorf("expected parent_run_id=%s, got %+v", root, r)
		}
	}

	byRoot, err := http.Get(env.srv.URL + "/admin-api/runs?root_run_id=" + root)
	if err != nil {
		t.Fatalf("GET ?root_run_id=: %v", err)
	}
	defer byRoot.Body.Close()
	var rootRuns []map[string]interface{}
	json.NewDecoder(byRoot.Body).Decode(&rootRuns)
	if len(rootRuns) != 2 {
		t.Fatalf("expected 2 runs with root_run_id=%s, got %+v", root, rootRuns)
	}
}

// TestAdminList_LimitOffsetPaging proves Admin list endpoints page with
// ?limit=&offset= (bare array; has-more when len == limit) instead of
// silently truncating at a hard 1000-row sample.
func TestAdminList_LimitOffsetPaging(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		id := "t-" + strconv.Itoa(i)
		env.store.CreateThread(ctx, &models.Thread{
			ThreadID: id, Status: models.ThreadStatusIdle,
			Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
		})
	}

	page1, err := http.Get(env.srv.URL + "/admin-api/threads?limit=2&offset=0")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	defer page1.Body.Close()
	var first []map[string]interface{}
	json.NewDecoder(page1.Body).Decode(&first)
	if len(first) != 2 {
		t.Fatalf("expected page size 2, got %d: %+v", len(first), first)
	}

	page2, err := http.Get(env.srv.URL + "/admin-api/threads?limit=2&offset=2")
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	defer page2.Body.Close()
	var second []map[string]interface{}
	json.NewDecoder(page2.Body).Decode(&second)
	if len(second) != 2 {
		t.Fatalf("expected second page size 2, got %d: %+v", len(second), second)
	}
	seen := map[string]bool{}
	for _, th := range first {
		seen[th["thread_id"].(string)] = true
	}
	for _, th := range second {
		id := th["thread_id"].(string)
		if seen[id] {
			t.Fatalf("expected offset pages to be disjoint, %q appeared in both", id)
		}
	}

	// Cap: limit above adminListMaxLimit (200) must not error; just clamp.
	capped, err := http.Get(env.srv.URL + "/admin-api/threads?limit=9999&offset=0")
	if err != nil {
		t.Fatalf("capped: %v", err)
	}
	defer capped.Body.Close()
	if capped.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for oversized limit, got %d", capped.StatusCode)
	}
	var all []map[string]interface{}
	json.NewDecoder(capped.Body).Decode(&all)
	if len(all) != 5 {
		t.Fatalf("expected all 5 threads under clamped limit, got %d", len(all))
	}
}

// TestAdminList_CursorPaging walks Admin threads via ?cursor= / X-Next-Cursor
// and rejects invalid / conflicting paging params.
func TestAdminList_CursorPaging(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		id := "tc-" + strconv.Itoa(i)
		ts := base.Add(time.Duration(i) * time.Second)
		if err := env.store.CreateThread(ctx, &models.Thread{
			ThreadID: id, Status: models.ThreadStatusIdle,
			Metadata: map[string]interface{}{}, CreatedAt: ts, UpdatedAt: ts,
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	page1, err := http.Get(env.srv.URL + "/admin-api/threads?limit=2")
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	defer page1.Body.Close()
	if page1.StatusCode != http.StatusOK {
		t.Fatalf("page1 status %d", page1.StatusCode)
	}
	var first []map[string]interface{}
	json.NewDecoder(page1.Body).Decode(&first)
	if len(first) != 2 {
		t.Fatalf("expected page size 2, got %d", len(first))
	}
	cursor := page1.Header.Get("X-Next-Cursor")
	if cursor == "" {
		t.Fatal("expected X-Next-Cursor on a full page")
	}

	page2, err := http.Get(env.srv.URL + "/admin-api/threads?limit=2&cursor=" + cursor)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	defer page2.Body.Close()
	var second []map[string]interface{}
	json.NewDecoder(page2.Body).Decode(&second)
	if len(second) != 2 {
		t.Fatalf("expected second page size 2, got %d", len(second))
	}
	seen := map[string]bool{}
	for _, th := range first {
		seen[th["thread_id"].(string)] = true
	}
	for _, th := range second {
		id := th["thread_id"].(string)
		if seen[id] {
			t.Fatalf("expected cursor pages to be disjoint, %q appeared in both", id)
		}
	}

	bad, err := http.Get(env.srv.URL + "/admin-api/threads?limit=2&cursor=not-a-cursor")
	if err != nil {
		t.Fatalf("bad cursor: %v", err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid cursor, got %d", bad.StatusCode)
	}

	both, err := http.Get(env.srv.URL + "/admin-api/threads?limit=2&cursor=" + cursor + "&offset=0")
	if err != nil {
		t.Fatalf("cursor+offset: %v", err)
	}
	defer both.Body.Close()
	if both.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for cursor+offset, got %d", both.StatusCode)
	}
}

// TestAdminGetAgent_SeesCrossTenantAgentAndExposesTenantID proves
// handleAdminGetAgent (unlike handleAdminListAgents, already covered
// above) reuses the same system-context, tenant_id-visible convention
// for a single-agent lookup by ID.
func TestAdminGetAgent_SeesCrossTenantAgentAndExposesTenantID(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	env.store.UpsertAgent(ctxA, &models.Agent{AgentID: "a1", Name: "a1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	// The default tenant context (no auth configured in newTestEnv)
	// would normally never see tenant-a's agent -- proving this
	// requires an actual cross-tenant lookup, not a same-tenant
	// coincidence.
	resp, err := http.Get(env.srv.URL + "/admin-api/agents/a1")
	if err != nil {
		t.Fatalf("GET /admin-api/agents/a1: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var agent map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&agent)
	if agent["tenant_id"] != "tenant-a" || agent["agent_id"] != "a1" {
		t.Errorf("expected agent_id=a1, tenant_id=tenant-a, got %+v", agent)
	}
}

// TestAdminGetAgent_NotFoundReturns404 covers the miss path (never
// exercised anywhere else) through the same handleStoreError mapping
// every other admin GET-by-ID route relies on.
func TestAdminGetAgent_NotFoundReturns404(t *testing.T) {
	env := newTestEnv(t)
	resp, err := http.Get(env.srv.URL + "/admin-api/agents/never-registered")
	if err != nil {
		t.Fatalf("GET /admin-api/agents/never-registered: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// TestAdminListRegistryEntries_SeesAcrossTenantsAndExposesTenantID
// proves /admin-api/registry uses system context (every tenant's
// published entries visible) and the tenant_id-visible view convention,
// same as every other admin list route.
func TestAdminListRegistryEntries_SeesAcrossTenantsAndExposesTenantID(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	now := time.Now().UTC()
	if err := env.store.PublishRegistryEntry(ctxA, &models.RegistryEntry{
		Name: "entry-a", SourceType: "git", SourceRef: "https://example.com/repo",
		Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("PublishRegistryEntry: %v", err)
	}

	resp, err := http.Get(env.srv.URL + "/admin-api/registry")
	if err != nil {
		t.Fatalf("GET /admin-api/registry: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var entries []map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&entries)
	if len(entries) != 1 || entries[0]["tenant_id"] != "tenant-a" || entries[0]["name"] != "entry-a" {
		t.Fatalf("expected 1 entry (name=entry-a, tenant_id=tenant-a), got %+v", entries)
	}
}

// TestAdminGetThread_SeesCrossTenantThreadAndExposesTenantID proves
// handleAdminGetThread (unlike handleAdminListThreads, already covered
// above) reuses the same system-context, tenant_id-visible convention
// for a single-thread lookup by ID.
func TestAdminGetThread_SeesCrossTenantThreadAndExposesTenantID(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	now := time.Now().UTC()
	if err := env.store.CreateThread(ctxA, &models.Thread{
		ThreadID: "thread-a", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateThread: %v", err)
	}

	resp, err := http.Get(env.srv.URL + "/admin-api/threads/thread-a")
	if err != nil {
		t.Fatalf("GET /admin-api/threads/thread-a: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var thread map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&thread)
	if thread["tenant_id"] != "tenant-a" || thread["thread_id"] != "thread-a" {
		t.Errorf("expected thread_id=thread-a, tenant_id=tenant-a, got %+v", thread)
	}
}

// TestAdminGetThread_NotFoundReturns404 covers the miss path through
// the same handleStoreError mapping every other admin GET-by-ID route
// relies on.
func TestAdminGetThread_NotFoundReturns404(t *testing.T) {
	env := newTestEnv(t)
	resp, err := http.Get(env.srv.URL + "/admin-api/threads/never-created")
	if err != nil {
		t.Fatalf("GET /admin-api/threads/never-created: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// TestAdminStreamRun_WorksAcrossTenants proves handleAdminStreamRun
// (unlike handleAdminGetRun, already covered above) reuses the
// client-facing streamExistingRun under system context -- the default
// (no-tenant) caller in newTestEnv can stream a tenant-a run's events
// it would never be able to reach via the client-facing
// /runs/{runID}/stream route. The run is already terminal with no
// events ever published to the broker, so streamExistingRun's own
// "append a synthetic end if replay found none" path (see runs.go) is
// what proves this reached the real streaming code, not just a
// pass-through that returned early.
func TestAdminStreamRun_WorksAcrossTenants(t *testing.T) {
	env := newTestEnv(t)
	ctxA := tenant.WithContext(context.Background(), "tenant-a")
	now := time.Now().UTC()
	env.store.CreateThread(ctxA, &models.Thread{ThreadID: "thread-a", Status: models.ThreadStatusIdle, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctxA, &models.Run{RunID: "run-a", ThreadID: "thread-a", AgentID: "echo_agent", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := http.Get(env.srv.URL + "/admin-api/runs/run-a/stream")
	if err != nil {
		t.Fatalf("GET /admin-api/runs/run-a/stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "event: end") {
		t.Errorf("expected a synthetic terminal 'end' event for an already-success run with no broker history, got body: %s", body)
	}
	if !strings.Contains(string(body), `"status":"success"`) {
		t.Errorf("expected the synthetic end event to carry the run's real status, got body: %s", body)
	}
}

// TestAdminStreamRun_NotFoundReturns404 covers the miss path.
func TestAdminStreamRun_NotFoundReturns404(t *testing.T) {
	env := newTestEnv(t)
	resp, err := http.Get(env.srv.URL + "/admin-api/runs/never-created/stream")
	if err != nil {
		t.Fatalf("GET /admin-api/runs/never-created/stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}
