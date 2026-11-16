package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

func TestConfiguredAgentCapabilitiesProfiles(t *testing.T) {
	for _, kind := range []string{"python-langgraph", "typescript-langgraphjs", "typescript-ai-sdk", "python-agent-framework", "python-openai-agents", "python-pydantic-ai", "python-google-adk"} {
		t.Run(kind, func(t *testing.T) {
			descriptor := configuredAgentCapabilities(nil, kind, "")["runkite_profile"].(map[string]interface{})
			if descriptor["framework_kind"] != kind || descriptor["schema_version"] != 1 || descriptor["provenance"] != "control_plane_configuration" {
				t.Fatalf("missing configured provenance: %#v", descriptor)
			}
			if kind == "python-langgraph" || kind == "typescript-langgraphjs" {
				if descriptor["native_resume"] != "langgraph_checkpoint_required" || descriptor["checkpoint_ref"] != "langgraph_checkpoint_required" || descriptor["continuity"] != "langgraph_checkpoint_state" || descriptor["local_tool_allowlist"] != "langgraph_stream_names" {
					t.Fatalf("LangGraph features lost their checkpoint/tool boundary: %#v", descriptor)
				}
			} else if descriptor["native_resume"] != "unsupported" || descriptor["checkpoint_ref"] != "unsupported" || descriptor["continuity"] != "successful_turn_state_http" || descriptor["local_tool_allowlist"] != "adapter_local_functions" {
				t.Fatalf("managed adapter features overstate native restore: %#v", descriptor)
			}
		})
	}
}

func TestConfiguredAgentCapabilitiesPreservesOpaqueKeysWithoutMutating(t *testing.T) {
	opaque := map[string]interface{}{"nested": []interface{}{nil, false, "value"}}
	oldDescriptor := map[string]interface{}{"vendor/extra": opaque, "native_resume": "stale"}
	previous := map[string]interface{}{"unknown.vendor": opaque, "runkite_profile": oldDescriptor}
	got := configuredAgentCapabilities(previous, "python-openai-agents", "")
	if !reflect.DeepEqual(got["unknown.vendor"], opaque) || !reflect.DeepEqual(got["runkite_profile"].(map[string]interface{})["vendor/extra"], opaque) {
		t.Fatalf("opaque keys were lost: %#v", got)
	}
	if oldDescriptor["native_resume"] != "stale" || len(previous) != 2 {
		t.Fatal("merge mutated the stored input")
	}
}

func TestConfiguredAgentCapabilitiesUnknownKindsAndPools(t *testing.T) {
	for _, cfg := range []struct{ kind, pool string }{{"custom-worker", ""}, {"python-crewai", ""}, {"pool-python-langgraph", ""}, {"python-langgraph", "isolated"}, {"python-openai-agents", "isolated"}} {
		t.Run(cfg.kind+"/"+cfg.pool, func(t *testing.T) {
			previous := configuredAgentCapabilities(nil, "python-langgraph", "")
			descriptor := configuredAgentCapabilities(previous, cfg.kind, cfg.pool)["runkite_profile"].(map[string]interface{})
			for _, key := range []string{"native_resume", "checkpoint_ref", "continuity", "local_tool_allowlist", "schema_reporting"} {
				if descriptor[key] != "unknown" {
					t.Fatalf("%s=%v for unqualified profile/pool", key, descriptor[key])
				}
			}
		})
	}
}

func TestBootstrapAgentCapabilitiesPreservesHistoryAndStableVersions(t *testing.T) {
	store := newCronTestStore(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "langgraph.json")
	writeConfig := func(kind string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"runner_kind":%q,"graphs":{"specialist":"./agent.py:graph"}}`, kind)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("python-langgraph")
	seed := &models.Agent{
		AgentID: "specialist", Name: "specialist",
		Description: fmt.Sprintf("Graph loaded from %s (./agent.py:graph)", path),
		Metadata:    map[string]interface{}{"source": path, "symbol": "graph", "runner_kind": "python-langgraph"},
		Capabilities: map[string]interface{}{
			"vendor":          map[string]interface{}{"nested": []interface{}{false, nil, "opaque", json.Number("9007199254740993"), json.Number("1.0000000000000001")}},
			"runkite_profile": map[string]interface{}{"vendor/extra": map[string]interface{}{"enabled": false}, "native_resume": "stale"},
		},
	}
	if err := store.UpsertAgent(ctx, seed); err != nil {
		t.Fatal(err)
	}
	bootstrapAgents(store, path)
	first, err := store.GetAgent(ctx, "specialist")
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 2 || !reflect.DeepEqual(first.Capabilities["vendor"], seed.Capabilities["vendor"]) {
		t.Fatalf("expected one additive definition version preserving opaque capabilities: %#v", first)
	}
	if !reflect.DeepEqual(first.Capabilities["runkite_profile"].(map[string]interface{})["vendor/extra"], map[string]interface{}{"enabled": false}) {
		t.Fatal("bootstrap lost an unknown key inside the descriptor")
	}
	bootstrapAgents(store, path)
	again, err := store.GetAgent(ctx, "specialist")
	if err != nil || again.Version != first.Version {
		t.Fatalf("restart churned version: agent=%#v err=%v", again, err)
	}
	versions, err := store.ListAgentVersions(ctx, "specialist")
	if err != nil || len(versions) != 2 || !reflect.DeepEqual(versions[1].Capabilities, seed.Capabilities) {
		t.Fatalf("immutable history changed: versions=%#v err=%v", versions, err)
	}
	writeConfig("python-openai-agents")
	bootstrapAgents(store, path)
	changed, err := store.GetAgent(ctx, "specialist")
	if err != nil || changed.Version != 3 || changed.Capabilities["runkite_profile"].(map[string]interface{})["native_resume"] != "unsupported" {
		t.Fatalf("profile change did not replace known fields: agent=%#v err=%v", changed, err)
	}
}

func TestBootstrapAgentCapabilitiesPoolKeepsFrameworkAndFeaturesUnknown(t *testing.T) {
	store := newCronTestStore(t)
	path := filepath.Join(t.TempDir(), "langgraph.json")
	if err := os.WriteFile(path, []byte(`{
		"runner_kind":"python-langgraph",
		"execution_pool":"isolated",
		"execution_pools":{"isolated":{"runner_kind":"python-langgraph","tenants":["default"],"agents":["specialist"]}},
		"graphs":{"specialist":"./agent.py:graph"}
	}`), 0600); err != nil {
		t.Fatal(err)
	}
	bootstrapAgents(store, path)
	agent, err := store.GetAgent(context.Background(), "specialist")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := agent.Capabilities["runkite_profile"].(map[string]interface{})
	if agent.Metadata["runner_kind"] != "pool-isolated" || descriptor["framework_kind"] != "python-langgraph" || descriptor["execution_boundary"] != "execution_pool" {
		t.Fatalf("dispatch kind replaced framework provenance: %#v", agent)
	}
	for _, key := range []string{"native_resume", "checkpoint_ref", "continuity", "local_tool_allowlist", "schema_reporting"} {
		if descriptor[key] != "unknown" {
			t.Fatalf("pool support inferred for %s: %#v", key, descriptor)
		}
	}
}

type capabilitiesReadFailure struct {
	state.Store
	writes int
}

func (s *capabilitiesReadFailure) GetAgent(context.Context, string) (*models.Agent, error) {
	return nil, errors.New("store unavailable")
}
func (s *capabilitiesReadFailure) UpsertAgent(context.Context, *models.Agent) error {
	s.writes++
	return nil
}

func TestBootstrapAgentCapabilitiesDoesNotEraseOnReadFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "langgraph.json")
	if err := os.WriteFile(path, []byte(`{"graphs":{"specialist":"./agent.py:graph"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	store := &capabilitiesReadFailure{}
	bootstrapAgents(store, path)
	if store.writes != 0 {
		t.Fatal("bootstrap replaced an unread definition")
	}
}
