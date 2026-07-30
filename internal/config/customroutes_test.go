package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLangGraphJSON_CustomRoutes proves the "custom_routes" section
// parses -- in-runner and sidecar modes are both just a URL from the
// control plane's perspective.
func TestLoadLangGraphJSON_CustomRoutes(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"custom_routes": {"url": "http://127.0.0.1:8100"}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatalf("LoadLangGraphJSON: %v", err)
	}
	if cfg.CustomRoutes == nil || cfg.CustomRoutes.URL != "http://127.0.0.1:8100" {
		t.Errorf("expected custom_routes.url parsed, got %+v", cfg.CustomRoutes)
	}
}

func TestLoadLangGraphJSON_NoCustomRoutesIsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(`{"graphs": {"echo": "graph.py:graph"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CustomRoutes != nil {
		t.Errorf("expected nil CustomRoutes when section absent, got %+v", cfg.CustomRoutes)
	}
}
