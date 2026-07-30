package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLangGraphJSON_RateLimit proves the "rate_limit" section parses
// into RateLimitEntry, covering per-user, per-agent, and per-tenant limits
// configurable via config -- including that per_tenant parses
// (even though internal/ratelimit currently treats it as a no-op) so config
// written today keeps working once multi-tenancy lands.
func TestLoadLangGraphJSON_RateLimit(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"rate_limit": {
			"global": {"rps": 100, "burst": 200},
			"per_user": {"rps": 10, "burst": 20},
			"per_agent": {"rps": 5, "burst": 10},
			"per_tenant": {"rps": 50, "burst": 100}
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
	if cfg.RateLimit == nil {
		t.Fatal("expected RateLimit to be parsed, got nil")
	}
	if cfg.RateLimit.Global == nil || cfg.RateLimit.Global.RPS != 100 || cfg.RateLimit.Global.Burst != 200 {
		t.Errorf("Global = %+v, want {RPS:100 Burst:200}", cfg.RateLimit.Global)
	}
	if cfg.RateLimit.PerUser == nil || cfg.RateLimit.PerUser.RPS != 10 || cfg.RateLimit.PerUser.Burst != 20 {
		t.Errorf("PerUser = %+v, want {RPS:10 Burst:20}", cfg.RateLimit.PerUser)
	}
	if cfg.RateLimit.PerAgent == nil || cfg.RateLimit.PerAgent.RPS != 5 || cfg.RateLimit.PerAgent.Burst != 10 {
		t.Errorf("PerAgent = %+v, want {RPS:5 Burst:10}", cfg.RateLimit.PerAgent)
	}
	if cfg.RateLimit.PerTenant == nil || cfg.RateLimit.PerTenant.RPS != 50 {
		t.Errorf("PerTenant = %+v, want {RPS:50 Burst:100}", cfg.RateLimit.PerTenant)
	}
}

// TestLoadLangGraphJSON_NoRateLimitIsNil proves absence of the section
// (the common case -- rate limiting is opt-in) doesn't error and leaves
// RateLimit nil, so initRateLimiter's disabled-by-default path is reachable.
func TestLoadLangGraphJSON_NoRateLimitIsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(`{"graphs": {"echo": "graph.py:graph"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RateLimit != nil {
		t.Errorf("expected nil RateLimit when section absent, got %+v", cfg.RateLimit)
	}
}
