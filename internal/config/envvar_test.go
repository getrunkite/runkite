package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadLangGraphJSON_EnvVarSubstitution proves the real-world need this
// exists for: a secret (here, an admin API key) never has to live in the
// checked-in langgraph.json -- deploy tooling sets the env var, the file
// stays a safe-to-commit template.
func TestLoadLangGraphJSON_EnvVarSubstitution(t *testing.T) {
	t.Setenv("TEST_ADMIN_KEY", "secret-key-123")

	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"auth": {
			"type": "jwt",
			"jwks_url": "https://example.com/certs",
			"admin_keys": {"${TEST_ADMIN_KEY}": "ops"}
		}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if name, ok := cfg.Auth.AdminKeys["secret-key-123"]; !ok || name != "ops" {
		t.Fatalf("expected env var substituted into admin_keys, got %+v", cfg.Auth.AdminKeys)
	}
}

func TestLoadLangGraphJSON_EnvVarDefaultUsedWhenUnset(t *testing.T) {
	os.Unsetenv("TEST_UNSET_VAR_XYZ")

	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"auth": {"type": "jwt", "jwks_url": "${TEST_UNSET_VAR_XYZ:-https://fallback.example.com/certs}"}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.JWKSURL != "https://fallback.example.com/certs" {
		t.Fatalf("expected default value used, got %q", cfg.Auth.JWKSURL)
	}
}

func TestLoadLangGraphJSON_EnvVarDefaultOverriddenWhenSet(t *testing.T) {
	t.Setenv("TEST_JWKS_OVERRIDE", "https://real.example.com/certs")

	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"auth": {"type": "jwt", "jwks_url": "${TEST_JWKS_OVERRIDE:-https://fallback.example.com/certs}"}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.JWKSURL != "https://real.example.com/certs" {
		t.Fatalf("expected env value to override default, got %q", cfg.Auth.JWKSURL)
	}
}

// TestLoadLangGraphJSON_MissingEnvVarWithNoDefaultFails is the safety
// property that matters most here: a missing secret should fail loudly
// at startup, not silently substitute an empty string into something
// like jwks_url or an admin key.
func TestLoadLangGraphJSON_MissingEnvVarWithNoDefaultFails(t *testing.T) {
	os.Unsetenv("TEST_REQUIRED_BUT_MISSING")

	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"auth": {"type": "jwt", "jwks_url": "${TEST_REQUIRED_BUT_MISSING}"}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadLangGraphJSON(path); err == nil {
		t.Fatal("expected an error for an undefined env var with no default")
	}
}

// TestLoadLangGraphJSON_EmptyEnvVarWithNoDefaultFails covers the
// LookupEnv footgun: a var set to "" is "present" to the OS but must
// still fail like unset -- otherwise admin_keys gets a "" key.
func TestLoadLangGraphJSON_EmptyEnvVarWithNoDefaultFails(t *testing.T) {
	t.Setenv("TEST_EMPTY_ADMIN_KEY", "")

	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"auth": {
			"type": "jwt",
			"jwks_url": "https://example.com/certs",
			"admin_keys": {"${TEST_EMPTY_ADMIN_KEY}": "ops"}
		}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadLangGraphJSON(path); err == nil {
		t.Fatal("expected an error for an empty env var with no default")
	}
}

// TestLoadLangGraphJSON_NoPlaceholdersUnaffected proves this is purely
// additive: a config with no ${...} syntax at all loads byte-identically
// to before this feature existed.
func TestLoadLangGraphJSON_NoPlaceholdersUnaffected(t *testing.T) {
	dir := t.TempDir()
	content := `{"graphs": {"echo": "graph.py:graph"}, "auth": {"type": "api_key", "keys": {"literal-key": {"name": "bot"}}}}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Auth.Keys["literal-key"]; !ok {
		t.Fatalf("expected literal key untouched, got %+v", cfg.Auth.Keys)
	}
}

func TestLoadConnectorConfigs_EnvVarSubstitution(t *testing.T) {
	t.Setenv("TEST_CONNECTOR_TOKEN", "tok-abc-123")

	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "connector.yaml")
	content := "auth:\n  type: api_key\n  api_key: \"${TEST_CONNECTOR_TOKEN}\"\n"
	if err := os.WriteFile(yamlPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	entries := map[string]ConnectorEntry{"svc": {ConfigRef: "connector.yaml"}}
	configs, err := LoadConnectorConfigs(entries, dir)
	if err != nil {
		t.Fatal(err)
	}
	if configs["svc"].Auth.APIKey != "tok-abc-123" {
		t.Fatalf("expected env var substituted into connector auth, got %+v", configs["svc"].Auth)
	}
}
