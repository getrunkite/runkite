package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
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
