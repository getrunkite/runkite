package api_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
)

// TestCreateRun_StoresRunManifestInMetadataAndAssignment proves the frozen
// dispatch snapshot is written to run metadata and copied onto the queued
// RunAssignment so runners, Admin, and audit all see the same intent.
func TestCreateRun_StoresRunManifestInMetadataAndAssignment(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if err := env.store.UpsertAgent(ctx, &models.Agent{
		AgentID: "manifest-agent",
		Name:    "manifest-agent",
		Metadata: map[string]interface{}{
			"runner_kind":       "typescript-langgraphjs",
			"connector_needs":   []string{"salesforce"},
			"allowed_tools":     []interface{}{"lookup"},
			"allowed_tools_set": true,
		},
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := postJSON(env.srv.URL+"/threads", map[string]interface{}{})
	if err != nil {
		t.Fatal(err)
	}
	var thread map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&thread)
	resp.Body.Close()
	threadID := thread["thread_id"].(string)

	runResp, err := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "manifest-agent",
		"input":    map[string]interface{}{"q": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var created models.Run
	if err := json.NewDecoder(runResp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	runResp.Body.Close()
	if created.RunID == "" {
		t.Fatal("expected run_id in create response")
	}

	stored, err := env.store.GetRun(ctx, created.RunID)
	if err != nil {
		t.Fatal(err)
	}
	rawMeta, ok := stored.Metadata["run_manifest"]
	if !ok {
		t.Fatal("run metadata missing run_manifest")
	}
	metaBytes, _ := json.Marshal(rawMeta)
	var manifest models.RunManifest
	if err := json.Unmarshal(metaBytes, &manifest); err != nil {
		t.Fatalf("run_manifest metadata: %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("schema_version=%d", manifest.SchemaVersion)
	}
	if manifest.AgentID != "manifest-agent" || manifest.AgentVersion != 1 {
		t.Fatalf("agent fields: id=%q version=%d", manifest.AgentID, manifest.AgentVersion)
	}
	if manifest.RunnerKind != "typescript-langgraphjs" {
		t.Fatalf("runner_kind=%q", manifest.RunnerKind)
	}
	if len(manifest.ConnectorNeeds) != 1 || manifest.ConnectorNeeds[0] != "salesforce" {
		t.Fatalf("connector_needs=%v", manifest.ConnectorNeeds)
	}
	if manifest.AllowedTools == nil || len(*manifest.AllowedTools) != 1 {
		t.Fatalf("allowed_tools=%#v", manifest.AllowedTools)
	}
	if manifest.Depth != 0 {
		t.Fatalf("depth=%d, want 0", manifest.Depth)
	}

	assignment, err := env.queue.Dequeue(ctx, "typescript-langgraphjs", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected queued assignment: %v", err)
	}
	if len(assignment.RunManifest) == 0 {
		t.Fatal("assignment missing run_manifest")
	}
	var assignmentManifest models.RunManifest
	if err := json.Unmarshal(assignment.RunManifest, &assignmentManifest); err != nil {
		t.Fatal(err)
	}
	if assignmentManifest.AgentID != manifest.AgentID ||
		assignmentManifest.RunnerKind != manifest.RunnerKind ||
		assignmentManifest.AgentVersion != manifest.AgentVersion {
		t.Fatalf("assignment manifest mismatch: stored=%+v assignment=%+v", manifest, assignmentManifest)
	}
}
