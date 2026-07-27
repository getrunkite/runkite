package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

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
