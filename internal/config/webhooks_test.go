package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLangGraphJSON_Webhooks proves the "webhooks" section parses into
// WebhookEntry (master plan: "Webhook delivery ... on run completion,
// failure, interrupt", generalized to all hook event types).
func TestLoadLangGraphJSON_Webhooks(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"webhooks": [
			{"url": "https://example.com/hook1", "secret": "whsec_abc", "events": ["run_complete", "error"]},
			{"url": "https://example.com/hook2"}
		]
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatalf("LoadLangGraphJSON: %v", err)
	}
	if len(cfg.Webhooks) != 2 {
		t.Fatalf("expected 2 webhooks, got %d", len(cfg.Webhooks))
	}
	w1 := cfg.Webhooks[0]
	if w1.URL != "https://example.com/hook1" || w1.Secret != "whsec_abc" {
		t.Errorf("hook1 fields wrong: %+v", w1)
	}
	if len(w1.Events) != 2 || w1.Events[0] != "run_complete" || w1.Events[1] != "error" {
		t.Errorf("hook1 events wrong: %+v", w1.Events)
	}
	w2 := cfg.Webhooks[1]
	if w2.URL != "https://example.com/hook2" || len(w2.Events) != 0 {
		t.Errorf("hook2 (subscribe-all) fields wrong: %+v", w2)
	}
}

func TestLoadLangGraphJSON_NoWebhooksIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(`{"graphs": {"echo": "graph.py:graph"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Webhooks) != 0 {
		t.Errorf("expected no webhooks when section absent, got %+v", cfg.Webhooks)
	}
}
