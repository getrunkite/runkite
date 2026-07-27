package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLangGraphJSON_RunnerKindDefaultsToPythonLangGraph(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(`{"graphs": {"echo": "graph.py:graph"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerKind != "python-langgraph" {
		t.Errorf("expected default runner_kind python-langgraph, got %q", cfg.RunnerKind)
	}
}

func TestLoadLangGraphJSON_RunnerKindExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	content := `{"graphs": {"echo": "graph.ts:graph"}, "runner_kind": "typescript-langgraphjs"}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunnerKind != "typescript-langgraphjs" {
		t.Errorf("expected explicit runner_kind typescript-langgraphjs, got %q", cfg.RunnerKind)
	}
}
