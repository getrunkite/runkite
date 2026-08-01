package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/getrunkite/runkite/internal/connector"
)

// TestLoadConnectorConfigs_RealYAML guards against a regression where
// config_ref files were only ever parsed as JSON (json.Unmarshal), despite
// being named/documented as YAML support and having yaml struct tags on
// ConnectorConfig. Real YAML syntax (comments, unquoted scalars) would fail
// to parse with a JSON-only decoder.
func TestLoadConnectorConfigs_RealYAML(t *testing.T) {
	dir := t.TempDir()
	yamlContent := `# Salesforce connector config
auth:
  type: oauth2_client_credentials
  token_url: https://example.com/token
  client_id: abc123
  scopes:
    - api
    - refresh_token
mcp:
  url: https://mcp.example.com
`
	if err := os.WriteFile(filepath.Join(dir, "salesforce.yaml"), []byte(yamlContent), 0644); err != nil {
		t.Fatal(err)
	}

	cfgs, err := LoadConnectorConfigs(map[string]ConnectorEntry{
		"salesforce": {ConfigRef: "salesforce.yaml"},
	}, dir)
	if err != nil {
		t.Fatalf("LoadConnectorConfigs failed on real YAML file: %v", err)
	}

	sf, ok := cfgs["salesforce"]
	if !ok {
		t.Fatal("salesforce connector not loaded")
	}
	if sf.Auth.TokenURL != "https://example.com/token" {
		t.Errorf("expected token_url parsed, got %+v", sf.Auth)
	}
	if len(sf.Auth.Scopes) != 2 || sf.Auth.Scopes[0] != "api" {
		t.Errorf("expected 2 scopes parsed, got %+v", sf.Auth.Scopes)
	}
	if sf.MCP == nil || sf.MCP.URL != "https://mcp.example.com" {
		t.Errorf("expected mcp.url parsed, got %+v", sf.MCP)
	}
}

// TestLoadConnectorConfigs_PlainJSONBackwardCompat ensures config_ref files
// written as plain JSON (valid before YAML support existed) still work,
// since YAML is a superset of JSON syntax.
func TestLoadConnectorConfigs_PlainJSONBackwardCompat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plain.json"),
		[]byte(`{"auth": {"type": "api_key", "api_key": "sk-test-123"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfgs, err := LoadConnectorConfigs(map[string]ConnectorEntry{
		"stripe": {ConfigRef: "plain.json"},
	}, dir)
	if err != nil {
		t.Fatalf("LoadConnectorConfigs failed on plain JSON file: %v", err)
	}
	if cfgs["stripe"].Auth.APIKey != "sk-test-123" {
		t.Errorf("expected api_key parsed from JSON, got %+v", cfgs["stripe"].Auth)
	}
}

// TestLoadConnectorConfigs_InlineCircuitBreaker is a regression test for a
// real bug found via live end-to-end testing: ConnectorEntry.CircuitBreaker
// was never added to the struct, so an inline (non-config_ref) connector's
// circuit_breaker section was silently dropped -- the breaker always used
// defaults regardless of what langgraph.json configured. config_ref-based
// connectors were unaffected (they unmarshal straight into ConnectorConfig,
// which always had the field).
func TestLoadConnectorConfigs_InlineCircuitBreaker(t *testing.T) {
	dir := t.TempDir()
	cfgs, err := LoadConnectorConfigs(map[string]ConnectorEntry{
		"flaky": {
			Auth: &connector.AuthConfig{Type: "api_key", APIKey: "sk-test"},
			CircuitBreaker: &connector.CircuitBreakerConfig{
				FailureThreshold: 2,
				CooldownSeconds:  30,
			},
		},
	}, dir)
	if err != nil {
		t.Fatalf("LoadConnectorConfigs: %v", err)
	}
	cb := cfgs["flaky"].CircuitBreaker
	if cb == nil {
		t.Fatal("expected inline circuit_breaker config to be preserved, got nil")
	}
	if cb.FailureThreshold != 2 || cb.CooldownSeconds != 30 {
		t.Errorf("circuit breaker fields wrong: %+v", cb)
	}
}
