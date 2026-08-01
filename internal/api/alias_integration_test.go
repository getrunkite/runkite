package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/models"
)

// TestAlias_RunCreationResolvesToRealTarget proves a run created
// against an alias name actually dispatches to (and is stored under)
// one of the configured real targets, never the alias name itself, and
// records which alias was requested for observability.
func TestAlias_RunCreationResolvesToRealTarget(t *testing.T) {
	env := newTestEnv(t)
	env.apiServer.SetAliasResolver(api.NewAliasResolver(map[string]config.AgentAliasEntry{
		"my_agent": {Targets: map[string]int{"my_agent_v1": 1}}, // single target: deterministic
	}))
	ctx := context.Background()
	env.store.UpsertAgent(ctx, &models.Agent{AgentID: "my_agent_v1", Name: "v1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	resp, err := postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{"assistant_id": "my_agent"})
	if err != nil {
		t.Fatalf("POST /threads/t1/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	if run.AgentID != "my_agent_v1" {
		t.Errorf("expected run.agent_id to be the resolved real target, got %q", run.AgentID)
	}
	if run.Metadata["requested_alias"] != "my_agent" {
		t.Errorf("expected metadata.requested_alias=my_agent, got %+v", run.Metadata)
	}

	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job enqueued: %v", err)
	}
	if assignment.GraphID != "my_agent_v1" {
		t.Errorf("expected assignment dispatched to resolved target my_agent_v1, got %s", assignment.GraphID)
	}
}

// TestAlias_NonAliasedAgentUnaffected proves configuring an alias for
// one name doesn't change behavior for any other, unrelated agent_id.
func TestAlias_NonAliasedAgentUnaffected(t *testing.T) {
	env := newTestEnv(t)
	env.apiServer.SetAliasResolver(api.NewAliasResolver(map[string]config.AgentAliasEntry{
		"my_agent": {Targets: map[string]int{"my_agent_v1": 1}},
	}))
	ctx := context.Background()
	env.store.UpsertAgent(ctx, &models.Agent{AgentID: "unrelated_agent", Name: "u", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}})

	resp, err := postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{"assistant_id": "unrelated_agent"})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	if run.AgentID != "unrelated_agent" {
		t.Errorf("expected unaliased agent_id unchanged, got %q", run.AgentID)
	}
	if _, ok := run.Metadata["requested_alias"]; ok {
		t.Errorf("expected no requested_alias metadata for a non-aliased run, got %+v", run.Metadata)
	}
}
