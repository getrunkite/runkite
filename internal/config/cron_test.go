package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLangGraphJSON_Cron(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"cron": {
			"daily-report": {
				"agent_id": "report_agent",
				"expression": "0 9 * * *",
				"timezone": "America/New_York",
				"input": {"type": "daily"}
			},
			"disabled-job": {
				"agent_id": "other_agent",
				"expression": "* * * * *",
				"enabled": false
			}
		}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatalf("LoadLangGraphJSON: %v", err)
	}
	if len(cfg.Cron) != 2 {
		t.Fatalf("expected 2 cron entries, got %d", len(cfg.Cron))
	}
	daily := cfg.Cron["daily-report"]
	if daily.AgentID != "report_agent" || daily.Expression != "0 9 * * *" || daily.Timezone != "America/New_York" {
		t.Errorf("daily-report fields wrong: %+v", daily)
	}
	if daily.Input["type"] != "daily" {
		t.Errorf("daily-report input wrong: %+v", daily.Input)
	}
	if daily.Enabled != nil {
		t.Errorf("expected nil Enabled (defaults to true) when omitted, got %v", *daily.Enabled)
	}

	disabled := cfg.Cron["disabled-job"]
	if disabled.Enabled == nil || *disabled.Enabled != false {
		t.Errorf("expected disabled-job.enabled=false, got %+v", disabled.Enabled)
	}
}

func TestLoadLangGraphJSON_NoCronIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(`{"graphs": {"echo": "graph.py:graph"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Cron) != 0 {
		t.Errorf("expected no cron entries when section absent, got %+v", cfg.Cron)
	}
}
