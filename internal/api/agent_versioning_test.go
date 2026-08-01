package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
)

// TestAgentVersioning_HistoryTracksEveryRealChange proves ListAgentVersions
// records one snapshot per actual content change, not per UpsertAgent
// call (an unchanged re-registration -- the common control-plane-restart
// case -- must not inflate history).
func TestAgentVersioning_HistoryTracksEveryRealChange(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	agent := &models.Agent{AgentID: "a1", Name: "v1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}
	if err := env.store.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("upsert v1: %v", err)
	}
	// Unchanged re-registration -- must not create a new version.
	if err := env.store.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("upsert v1 again (unchanged): %v", err)
	}
	agent.Name = "v2"
	if err := env.store.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}
	agent.Description = "now with a description"
	if err := env.store.UpsertAgent(ctx, agent); err != nil {
		t.Fatalf("upsert v3: %v", err)
	}

	resp, err := http.Get(env.srv.URL + "/agents/a1/versions")
	if err != nil {
		t.Fatalf("GET versions: %v", err)
	}
	expectStatus(t, resp, 200)
	var versions []models.AgentVersion
	json.Unmarshal(readBody(t, resp), &versions)

	if len(versions) != 3 {
		t.Fatalf("expected exactly 3 version snapshots (not 4 -- the unchanged re-registration must not count), got %d: %+v", len(versions), versions)
	}
	if versions[0].Version != 3 || versions[0].Description != "now with a description" {
		t.Errorf("expected newest-first ordering with v3 first, got %+v", versions[0])
	}
	if versions[2].Version != 1 || versions[2].Name != "v1" {
		t.Errorf("expected v1 last, got %+v", versions[2])
	}
}

// TestAgentVersioning_RollbackCreatesNewVersionWithOldContent proves
// rollback is append-only: rolling back to v1's content from v3 creates
// v4 (not a rewrite of the current row back to "version 1"), and v4's
// CONTENT matches v1's.
func TestAgentVersioning_RollbackCreatesNewVersionWithOldContent(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	agent := &models.Agent{AgentID: "a1", Name: "v1-name", Description: "v1-desc", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}
	env.store.UpsertAgent(ctx, agent)
	agent.Name, agent.Description = "v2-name", "v2-desc"
	env.store.UpsertAgent(ctx, agent)
	agent.Name, agent.Description = "v3-name", "v3-desc"
	env.store.UpsertAgent(ctx, agent)

	resp, err := postJSON(env.srv.URL+"/agents/a1/versions/1/rollback", nil)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	expectStatus(t, resp, 200)
	var rolledBack models.Agent
	json.Unmarshal(readBody(t, resp), &rolledBack)

	if rolledBack.Version != 4 {
		t.Errorf("expected rollback to create version 4 (append-only, not rewriting v1), got %d", rolledBack.Version)
	}
	if rolledBack.Name != "v1-name" || rolledBack.Description != "v1-desc" {
		t.Errorf("expected rolled-back content to match v1's snapshot, got name=%q desc=%q", rolledBack.Name, rolledBack.Description)
	}

	// v1's own history row must be untouched (still says "v1-name"),
	// proving rollback didn't rewrite or delete anything.
	v1Resp, _ := http.Get(env.srv.URL + "/agents/a1/versions")
	var versions []models.AgentVersion
	json.Unmarshal(readBody(t, v1Resp), &versions)
	if len(versions) != 4 {
		t.Fatalf("expected 4 total versions after rollback (1,2,3, and the new 4), got %d", len(versions))
	}
	foundOriginalV1 := false
	for _, v := range versions {
		if v.Version == 1 && v.Name == "v1-name" {
			foundOriginalV1 = true
		}
	}
	if !foundOriginalV1 {
		t.Error("expected original v1 snapshot to survive unmodified")
	}

	// GET /agents/a1 should now reflect the rolled-back content as current.
	current, _ := env.store.GetAgent(ctx, "a1")
	if current.Version != 4 || current.Name != "v1-name" {
		t.Errorf("expected current agent to be v4 with v1's content, got %+v", current)
	}
}

func TestAgentVersioning_RollbackToUnknownVersion404(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", nil)

	resp, _ := postJSON(env.srv.URL+"/agents/a1/versions/99/rollback", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown version, got %d", resp.StatusCode)
	}
}

// TestAgentVersioning_AssistantViewReportsRealVersion is a regression
// test for a real (not hypothetical) pre-existing bug found while
// touching this file: the /assistants/* SDK-compat view hardcoded
// version=1 for every agent regardless of its actual version.
func TestAgentVersioning_AssistantViewReportsRealVersion(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	agent := &models.Agent{AgentID: "a1", Name: "v1", Metadata: map[string]interface{}{}, Capabilities: map[string]interface{}{}}
	env.store.UpsertAgent(ctx, agent)
	agent.Name = "v2"
	env.store.UpsertAgent(ctx, agent)

	resp, err := http.Get(env.srv.URL + "/assistants/a1")
	if err != nil {
		t.Fatalf("GET /assistants/a1: %v", err)
	}
	expectStatus(t, resp, 200)
	var view map[string]interface{}
	json.Unmarshal(readBody(t, resp), &view)
	if v, _ := view["version"].(float64); int(v) != 2 {
		t.Errorf("expected assistant view to report the real version 2, got %v", view["version"])
	}
}
