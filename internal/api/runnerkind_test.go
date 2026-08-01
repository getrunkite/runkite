package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

// TestCreateRun_RoutesToAgentsDeclaredRunnerKind proves a run is assigned
// to whichever runner_kind the target agent declared at bootstrap (see
// cmd/serve.go's bootstrapAgents stashing langgraph.json's top-level
// "runner_kind" into agent metadata) -- not hardcoded to python-langgraph.
// This is what makes mixing a Python runner and a TypeScript runner in
// one deployment actually route correctly: each only ever receives jobs
// for the agents whose config declared its own runner_kind.
func TestCreateRun_RoutesToAgentsDeclaredRunnerKind(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if err := env.store.UpsertAgent(ctx, &models.Agent{
		AgentID: "ts_agent", Name: "ts_agent",
		Metadata:     map[string]interface{}{"runner_kind": "typescript-langgraphjs"},
		Capabilities: map[string]interface{}{},
	}); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}

	resp, err := postJSON(env.srv.URL+"/threads", map[string]interface{}{})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	var thread map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&thread)
	resp.Body.Close()
	threadID := thread["thread_id"].(string)

	if _, err := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "ts_agent",
		"input":    map[string]interface{}{},
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	assignment, err := env.queue.Dequeue(ctx, "typescript-langgraphjs", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected a job queued for runner_kind=typescript-langgraphjs: %v", err)
	}
	if assignment.RunnerKind != "typescript-langgraphjs" {
		t.Errorf("expected RunnerKind=typescript-langgraphjs, got %q", assignment.RunnerKind)
	}
}

// TestCreateRun_DefaultsToPythonLangGraphWhenNoRunnerKindDeclared proves
// the fallback still matches every existing deployment's behavior: a
// registered agent that simply doesn't declare a runner_kind (the
// common case -- most agents never set this) is routed to
// python-langgraph, unchanged from before this feature existed.
func TestCreateRun_DefaultsToPythonLangGraphWhenNoRunnerKindDeclared(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "plain_agent")
	ctx := context.Background()

	resp, err := postJSON(env.srv.URL+"/threads", map[string]interface{}{})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	var thread map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&thread)
	resp.Body.Close()
	threadID := thread["thread_id"].(string)

	if _, err := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "plain_agent",
		"input":    map[string]interface{}{},
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected a job queued for the default runner_kind=python-langgraph: %v", err)
	}
}

// TestCreateRun_UnregisteredAgentRejected proves createRunCtx now fails
// fast on an unknown agent_id (before this fix, a typo'd agent_id got a
// 200 with a pending run that only ever failed later, asynchronously,
// once a runner tried and failed to load a graph that was never
// registered).
func TestCreateRun_UnregisteredAgentRejected(t *testing.T) {
	env := newTestEnv(t)

	resp, err := postJSON(env.srv.URL+"/threads", map[string]interface{}{})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	var thread map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&thread)
	resp.Body.Close()
	threadID := thread["thread_id"].(string)

	runResp, err := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "never_registered_agent",
		"input":    map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	expectStatus(t, runResp, 404)
}
