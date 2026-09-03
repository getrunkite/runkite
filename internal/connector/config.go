// Package connector implements the Connector/MCP Registry — pre-authenticated
// OAuth/MCP sessions for runners without implementing auth flows themselves.
package connector

import (
	"os"
	"regexp"
)

// ConnectorConfig defines an external service connector (auth, MCP, error mapping, tool filtering).
type ConnectorConfig struct {
	Auth           AuthConfig            `json:"auth" yaml:"auth"`
	MCP            *MCPConfig            `json:"mcp,omitempty" yaml:"mcp,omitempty"`
	Errors         map[string]string     `json:"errors,omitempty" yaml:"errors,omitempty"`
	Tools          *ToolFilter           `json:"tools,omitempty" yaml:"tools,omitempty"`
	CircuitBreaker *CircuitBreakerConfig `json:"circuit_breaker,omitempty" yaml:"circuit_breaker,omitempty"`
}

// CircuitBreakerConfig configures per-connector circuit breaking.
// Absent/zero-valued means DefaultCircuitBreakerConfig is used -- circuit
// breaking is always on, only its thresholds are tunable, since an
// unprotected connector taking down run-creation latency for every agent
// that needs it is never the right default.
type CircuitBreakerConfig struct {
	// FailureThreshold is how many consecutive token-fetch failures open
	// the breaker.
	FailureThreshold int `json:"failure_threshold,omitempty" yaml:"failure_threshold,omitempty"`
	// CooldownSeconds is how long the breaker stays open (rejecting calls
	// immediately, without attempting the network call) before allowing a
	// trial call through.
	CooldownSeconds int `json:"cooldown_seconds,omitempty" yaml:"cooldown_seconds,omitempty"`
}

// AuthConfig describes how to authenticate with the external service.
type AuthConfig struct {
	Type         string   `json:"type" yaml:"type"` // oauth2_token_exchange, oauth2_client_credentials, api_key, bearer
	Issuer       string   `json:"issuer,omitempty" yaml:"issuer,omitempty"`
	TokenURL     string   `json:"token_url,omitempty" yaml:"token_url,omitempty"`
	ClientID     string   `json:"client_id,omitempty" yaml:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty" yaml:"client_secret,omitempty"`
	Scopes       []string `json:"scopes,omitempty" yaml:"scopes,omitempty"`
	APIKey       string   `json:"api_key,omitempty" yaml:"api_key,omitempty"`
	BearerToken  string   `json:"bearer_token,omitempty" yaml:"bearer_token,omitempty"`
	// SecretRef resolves the primary credential at GetSession time
	// (env: / file: / vault:path#field). Do not also set api_key,
	// bearer_token, or client_secret for the same auth type — pick one.
	// The ref string itself is not ${ENV}-expanded.
	SecretRef string `json:"secret_ref,omitempty" yaml:"secret_ref,omitempty"`
}

// MCPConfig points to an MCP server endpoint.
type MCPConfig struct {
	URL string `json:"url" yaml:"url"`
}

// ToolFilter controls which MCP tools a connector exposes.
type ToolFilter struct {
	Allow []string `json:"allow,omitempty" yaml:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty" yaml:"deny,omitempty"`
}

// envVarPattern matches ${VAR_NAME} for environment variable expansion.
var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// ExpandEnv replaces all ${VAR_NAME} occurrences in s with os.Getenv("VAR_NAME").
func ExpandEnv(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		varName := envVarPattern.FindStringSubmatch(match)[1]
		return os.Getenv(varName)
	})
}

// expandAuthConfig expands environment variables in all string fields.
func expandAuthConfig(cfg *AuthConfig) {
	cfg.Issuer = ExpandEnv(cfg.Issuer)
	cfg.TokenURL = ExpandEnv(cfg.TokenURL)
	cfg.ClientID = ExpandEnv(cfg.ClientID)
	cfg.ClientSecret = ExpandEnv(cfg.ClientSecret)
	cfg.APIKey = ExpandEnv(cfg.APIKey)
	cfg.BearerToken = ExpandEnv(cfg.BearerToken)
}
