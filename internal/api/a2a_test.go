package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/api"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

func upsertA2AAgent(t *testing.T, env *testEnv, ctx context.Context, agentID string) {
	t.Helper()
	if err := env.store.UpsertAgent(ctx, &models.Agent{
		AgentID: agentID, Name: agentID, Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("UpsertAgent(%s): %v", agentID, err)
	}
}

// TestA2A_CreatesRunWithParentAndRootLinkage proves a basic delegation
// call sets parent_run_id, root_run_id, and depth correctly for a
// single hop (parent has no root of its own -- becomes the root).
func TestA2A_CreatesRunWithParentAndRootLinkage(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "parent_agent")
	upsertA2AAgent(t, env, ctx, "child_agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "parent-run", ThreadID: "t1", AgentID: "parent_agent", Status: models.RunStatusRunning, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{
		"agent_id":      "child_agent",
		"parent_run_id": "parent-run",
		"input":         map[string]interface{}{"messages": []interface{}{}},
	})
	if err != nil {
		t.Fatalf("POST /internal/a2a/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	if run.ParentRunID == nil || *run.ParentRunID != "parent-run" {
		t.Errorf("expected parent_run_id=parent-run, got %+v", run.ParentRunID)
	}
	if run.RootRunID == nil || *run.RootRunID != "parent-run" {
		t.Errorf("expected root_run_id=parent-run (parent has no root of its own), got %+v", run.RootRunID)
	}
	if run.Depth != 1 {
		t.Errorf("expected depth=1, got %d", run.Depth)
	}

	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job enqueued for async (wait=false) call: %v", err)
	}
	if assignment.GraphID != "child_agent" {
		t.Errorf("expected assignment for child_agent, got %s", assignment.GraphID)
	}
}

// TestA2A_RootPropagatesThroughMultipleHops proves a 3-level chain
// (grandparent -> parent -> child) all shares the same root_run_id, not
// just the immediate parent's own ID.
func TestA2A_RootPropagatesThroughMultipleHops(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "agent_a")
	upsertA2AAgent(t, env, ctx, "agent_b")
	upsertA2AAgent(t, env, ctx, "agent_c")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "root-run", ThreadID: "t1", AgentID: "agent_a", Status: models.RunStatusRunning, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	// Hop 1: root -> agent_b
	resp1, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{
		"agent_id": "agent_b", "parent_run_id": "root-run",
	})
	defer resp1.Body.Close()
	var run1 models.Run
	json.Unmarshal(readBody(t, resp1), &run1)
	if run1.Depth != 1 || run1.RootRunID == nil || *run1.RootRunID != "root-run" {
		t.Fatalf("hop 1: expected depth=1 root=root-run, got depth=%d root=%+v", run1.Depth, run1.RootRunID)
	}

	// Hop 2: agent_b's run -> agent_c (a sub-sub-agent)
	resp2, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{
		"agent_id": "agent_c", "parent_run_id": run1.RunID,
	})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("hop 2: expected 200, got %d: %s", resp2.StatusCode, readBody(t, resp2))
	}
	var run2 models.Run
	json.Unmarshal(readBody(t, resp2), &run2)
	if run2.Depth != 2 {
		t.Errorf("hop 2: expected depth=2, got %d", run2.Depth)
	}
	if run2.RootRunID == nil || *run2.RootRunID != "root-run" {
		t.Errorf("hop 2: expected root_run_id to still be root-run (not agent_b's run), got %+v", run2.RootRunID)
	}
	if run2.ParentRunID == nil || *run2.ParentRunID != run1.RunID {
		t.Errorf("hop 2: expected parent_run_id=%s, got %+v", run1.RunID, run2.ParentRunID)
	}
}

// TestA2A_DepthLimitEnforced proves a chain exceeding a configured
// max_depth is rejected with 400, not allowed to grow unbounded.
func TestA2A_DepthLimitEnforced(t *testing.T) {
	env := newTestEnv(t)
	env.apiServer.SetA2AMaxDepth(2)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "run-0", ThreadID: "t1", AgentID: "agent", Status: models.RunStatusRunning, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	// Depth 1: allowed (1 <= max_depth 2)
	resp1, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "agent", "parent_run_id": "run-0"})
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("depth 1: expected 200, got %d: %s", resp1.StatusCode, readBody(t, resp1))
	}
	var run1 models.Run
	json.Unmarshal(readBody(t, resp1), &run1)

	// Depth 2: allowed (2 <= max_depth 2)
	resp2, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "agent", "parent_run_id": run1.RunID})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("depth 2: expected 200, got %d: %s", resp2.StatusCode, readBody(t, resp2))
	}
	var run2 models.Run
	json.Unmarshal(readBody(t, resp2), &run2)

	// Depth 3: rejected (3 > max_depth 2)
	resp3, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "agent", "parent_run_id": run2.RunID})
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("depth 3: expected 400 (depth limit exceeded), got %d: %s", resp3.StatusCode, readBody(t, resp3))
	}
}

// TestA2A_IdentityPropagatesToRunnerAssignment proves on_behalf_of
// reaches the enqueued RunAssignment's User field -- the sub-agent
// executes with the original caller's identity, not an anonymous or
// runner-level identity.
func TestA2A_IdentityPropagatesToRunnerAssignment(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "parent_agent")
	upsertA2AAgent(t, env, ctx, "child_agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "parent-run", ThreadID: "t1", AgentID: "parent_agent", Status: models.RunStatusRunning, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{
		"agent_id":      "child_agent",
		"parent_run_id": "parent-run",
		"on_behalf_of": map[string]interface{}{
			"identity":         "alice",
			"is_authenticated": true,
			"permissions":      []string{"read", "write"},
			"email":            "alice@example.com",
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}

	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job in queue: %v", err)
	}
	if assignment.User == nil || assignment.User.Identity != "alice" {
		t.Fatalf("expected assignment.User.Identity=alice, got %+v", assignment.User)
	}
	if assignment.User.Extra["email"] != "alice@example.com" {
		t.Errorf("expected forwarded extra field email, got %+v", assignment.User.Extra)
	}
}

// TestA2A_TenantDerivedFromParentNotTrustedFromRequest proves the
// sub-run's tenant always matches the PARENT run's tenant, regardless
// of any tenant_id a compromised/buggy caller might try to smuggle in
// via on_behalf_of (which has no tenant_id field to smuggle at all --
// this test proves the sub-run simply lands in the parent's tenant).
func TestA2A_TenantDerivedFromParentNotTrustedFromRequest(t *testing.T) {
	env := newTestEnv(t)
	ctxTenantA := tenant.WithContext(context.Background(), "tenant-a")
	upsertA2AAgent(t, env, ctxTenantA, "parent_agent")
	upsertA2AAgent(t, env, ctxTenantA, "child_agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctxTenantA, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctxTenantA, &models.Run{RunID: "parent-run", ThreadID: "t1", AgentID: "parent_agent", Status: models.RunStatusRunning, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{
		"agent_id": "child_agent", "parent_run_id": "parent-run",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	// Fetch with system context to see the actual stored tenant_id
	// (client-facing GetRun would filter by caller's own tenant).
	fetched, err := env.store.GetRun(tenant.SystemContext(context.Background()), run.RunID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if fetched.TenantID != "tenant-a" {
		t.Errorf("expected sub-run scoped to parent's tenant-a, got %q", fetched.TenantID)
	}
}

// TestA2A_MissingParentRunIDRejected and TestA2A_UnknownParentRun404
// cover the two basic validation paths.
func TestA2A_MissingParentRunIDRejected(t *testing.T) {
	env := newTestEnv(t)
	resp, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "agent"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing parent_run_id, got %d", resp.StatusCode)
	}
}

func TestA2A_UnknownParentRun404(t *testing.T) {
	env := newTestEnv(t)
	resp, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "agent", "parent_run_id": "does-not-exist"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown parent_run_id, got %d", resp.StatusCode)
	}
}

// TestA2A_CancelCascadesToDelegatedChildren proves cancelling a run
// also cancels everything it delegated to, directly or transitively --
// master plan follow-up: without this, a cancelled parent leaves
// orphaned child runs still executing with no way for the caller to
// have stopped them. An unrelated top-level run must be untouched.
func TestA2A_CancelCascadesToDelegatedChildren(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "root_agent")
	upsertA2AAgent(t, env, ctx, "child_agent")
	upsertA2AAgent(t, env, ctx, "grandchild_agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t-cascade", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "root-run", ThreadID: "t-cascade", AgentID: "root_agent", Status: models.RunStatusRunning, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp1, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "child_agent", "parent_run_id": "root-run"})
	defer resp1.Body.Close()
	var childRun models.Run
	json.Unmarshal(readBody(t, resp1), &childRun)

	resp2, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "grandchild_agent", "parent_run_id": childRun.RunID})
	defer resp2.Body.Close()
	var grandchildRun models.Run
	json.Unmarshal(readBody(t, resp2), &grandchildRun)

	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t-unrelated", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "unrelated-run", ThreadID: "t-unrelated", AgentID: "root_agent", Status: models.RunStatusRunning, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	respCancel, err := postJSON(env.srv.URL+"/runs/root-run/cancel", map[string]interface{}{})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	defer respCancel.Body.Close()
	if respCancel.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", respCancel.StatusCode, readBody(t, respCancel))
	}

	if root, _ := env.store.GetRun(ctx, "root-run"); root.Status != models.RunStatusInterrupted {
		t.Errorf("expected root-run interrupted, got %s", root.Status)
	}
	if child, _ := env.store.GetRun(ctx, childRun.RunID); child.Status != models.RunStatusInterrupted {
		t.Errorf("expected child run interrupted (cascade), got %s", child.Status)
	}
	if grandchild, _ := env.store.GetRun(ctx, grandchildRun.RunID); grandchild.Status != models.RunStatusInterrupted {
		t.Errorf("expected grandchild run interrupted (cascade), got %s", grandchild.Status)
	}
	if unrelated, _ := env.store.GetRun(ctx, "unrelated-run"); unrelated.Status != models.RunStatusRunning {
		t.Errorf("expected unrelated top-level run untouched, got %s", unrelated.Status)
	}
}

// TestA2A_CancelCascadesOnlyDownwardNotToAncestors proves the cascade
// is one-directional: cancelling a run in the MIDDLE of a delegation
// tree cancels its own descendants but leaves its ancestors running --
// cascading upward would be surprising and wrong (a sub-agent choosing
// to bail, or being cancelled individually, shouldn't kill the caller
// that spawned it).
func TestA2A_CancelCascadesOnlyDownwardNotToAncestors(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "root_agent")
	upsertA2AAgent(t, env, ctx, "child_agent")
	upsertA2AAgent(t, env, ctx, "grandchild_agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t-cascade2", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "root-run-2", ThreadID: "t-cascade2", AgentID: "root_agent", Status: models.RunStatusRunning, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp1, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "child_agent", "parent_run_id": "root-run-2"})
	defer resp1.Body.Close()
	var childRun models.Run
	json.Unmarshal(readBody(t, resp1), &childRun)

	resp2, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "grandchild_agent", "parent_run_id": childRun.RunID})
	defer resp2.Body.Close()
	var grandchildRun models.Run
	json.Unmarshal(readBody(t, resp2), &grandchildRun)

	// Cancel the CHILD (middle of the tree), not the root.
	respCancel, err := postJSON(env.srv.URL+"/runs/"+childRun.RunID+"/cancel", map[string]interface{}{})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	defer respCancel.Body.Close()
	if respCancel.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", respCancel.StatusCode, readBody(t, respCancel))
	}

	if child, _ := env.store.GetRun(ctx, childRun.RunID); child.Status != models.RunStatusInterrupted {
		t.Errorf("expected the cancelled child itself to be interrupted, got %s", child.Status)
	}
	if grandchild, _ := env.store.GetRun(ctx, grandchildRun.RunID); grandchild.Status != models.RunStatusInterrupted {
		t.Errorf("expected the grandchild (descendant of the cancelled child) to cascade, got %s", grandchild.Status)
	}
	if root, _ := env.store.GetRun(ctx, "root-run-2"); root.Status != models.RunStatusRunning {
		t.Errorf("expected the root (ancestor of the cancelled child) to be UNTOUCHED, got %s", root.Status)
	}
}

// TestA2A_CancelAlreadyTerminalRunDoesNotErrorOrCascadeTwice proves
// cancelling an already-terminal run is a no-op (matching
// cancelRunCore's existing single-run behavior) and doesn't panic or
// error when walking for descendants to cascade to.
func TestA2A_CancelAlreadyTerminalRunDoesNotErrorOrCascadeTwice(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "root_agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t-term", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "already-done", ThreadID: "t-term", AgentID: "root_agent", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := postJSON(env.srv.URL+"/runs/already-done/cancel", map[string]interface{}{})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for cancelling an already-terminal run, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	if run, _ := env.store.GetRun(ctx, "already-done"); run.Status != models.RunStatusSuccess {
		t.Errorf("expected status to remain success (cancel of terminal run is a no-op), got %s", run.Status)
	}
}

// TestA2A_CostAggregationSumsUsageAcrossDelegationTree proves GET
// /runs/{runID}/cost sums a conventional output.usage object across
// every run in a delegation tree (root + descendants), and that
// querying by ANY run's ID in the tree resolves to the same rollup.
// Runs with no usage object contribute zero, not an error.
func TestA2A_CostAggregationSumsUsageAcrossDelegationTree(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "root_agent")
	upsertA2AAgent(t, env, ctx, "child_agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t-cost", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "cost-root", ThreadID: "t-cost", AgentID: "root_agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp1, _ := postJSON(env.srv.URL+"/internal/a2a/runs", map[string]interface{}{"agent_id": "child_agent", "parent_run_id": "cost-root"})
	defer resp1.Body.Close()
	var childRun models.Run
	json.Unmarshal(readBody(t, resp1), &childRun)

	// Root reports usage; child reports usage too (with only prompt+
	// completion, no explicit total -- must be filled in as a
	// fallback); an unrelated top-level run must not be included.
	if err := env.store.UpdateRunStatus(ctx, "cost-root", models.RunStatusSuccess,
		[]byte(`{"messages":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"cost_usd":0.01}}`), ""); err != nil {
		t.Fatalf("UpdateRunStatus (root): %v", err)
	}
	if err := env.store.UpdateRunStatus(ctx, childRun.RunID, models.RunStatusSuccess,
		[]byte(`{"messages":[],"usage":{"prompt_tokens":20,"completion_tokens":10}}`), ""); err != nil {
		t.Fatalf("UpdateRunStatus (child): %v", err)
	}

	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t-cost-unrelated", Status: models.ThreadStatusBusy, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "cost-unrelated", ThreadID: "t-cost-unrelated", AgentID: "root_agent", Status: models.RunStatusPending, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})
	env.store.UpdateRunStatus(ctx, "cost-unrelated", models.RunStatusSuccess, []byte(`{"usage":{"prompt_tokens":9999}}`), "")

	for _, queryRunID := range []string{"cost-root", childRun.RunID} {
		resp, err := http.Get(env.srv.URL + "/runs/" + queryRunID + "/cost")
		if err != nil {
			t.Fatalf("GET /runs/%s/cost: %v", queryRunID, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
		}
		var summary api.RunCostSummary
		if err := json.Unmarshal(readBody(t, resp), &summary); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if summary.RootRunID != "cost-root" {
			t.Errorf("query by %s: expected root_run_id=cost-root, got %q", queryRunID, summary.RootRunID)
		}
		if summary.RunCount != 2 {
			t.Errorf("query by %s: expected run_count=2 (root+child, not the unrelated run), got %d", queryRunID, summary.RunCount)
		}
		if summary.Usage.PromptTokens != 120 || summary.Usage.CompletionTokens != 60 || summary.Usage.TotalTokens != 180 {
			t.Errorf("query by %s: expected summed tokens 120/60/180, got %+v", queryRunID, summary.Usage)
		}
		if summary.Usage.CostUSD != 0.01 {
			t.Errorf("query by %s: expected cost_usd=0.01 (only root reported it), got %v", queryRunID, summary.Usage.CostUSD)
		}
	}
}

// TestA2A_CostAggregationNoUsageReportedReturnsZeros proves a run (or
// tree) that never reported any usage returns an all-zero summary, not
// an error -- the whole point of the best-effort convention.
func TestA2A_CostAggregationNoUsageReportedReturnsZeros(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	upsertA2AAgent(t, env, ctx, "quiet_agent")

	now := time.Now().UTC()
	env.store.CreateThread(ctx, &models.Thread{ThreadID: "t-no-usage", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now})
	env.store.CreateRun(ctx, &models.Run{RunID: "no-usage-run", ThreadID: "t-no-usage", AgentID: "quiet_agent", Status: models.RunStatusSuccess, Metadata: map[string]interface{}{}, CreatedAt: now, UpdatedAt: now})

	resp, err := http.Get(env.srv.URL + "/runs/no-usage-run/cost")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var summary api.RunCostSummary
	json.Unmarshal(readBody(t, resp), &summary)
	if summary.RunCount != 1 || summary.Usage != (api.RunUsage{}) {
		t.Errorf("expected 1 run with all-zero usage, got %+v", summary)
	}
}
