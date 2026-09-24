package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePayloadShrink_OmitIsDisabled(t *testing.T) {
	s, err := ParsePayloadShrink(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Enabled {
		t.Fatal("omit must be disabled")
	}
	if s.MaxBytes != 32768 || s.PreviewBytes != 2048 || s.CacheTTL != 15*time.Minute {
		t.Fatalf("%+v", s)
	}
}

func TestLoadLangGraphJSON_PayloadShrink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"payload_shrink": {
			"enabled": true,
			"max_bytes": 4096,
			"preview_bytes": 100,
			"cache_ttl": "5m",
			"max_store_bytes": 8192
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PayloadShrink == nil || !cfg.PayloadShrink.Enabled {
		t.Fatal("expected payload_shrink")
	}
	s, err := ParsePayloadShrink(cfg.PayloadShrink)
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxBytes != 4096 || s.PreviewBytes != 100 || s.CacheTTL != 5*time.Minute || s.MaxStoreBytes != 8192 {
		t.Fatalf("%+v", s)
	}
}

func TestParsePayloadShrink_Invalid(t *testing.T) {
	tooSmall := 512
	_, err := ParsePayloadShrink(&PayloadShrinkEntry{Enabled: true, MaxBytes: &tooSmall})
	if err == nil {
		t.Fatal("expected max_bytes error")
	}
}
