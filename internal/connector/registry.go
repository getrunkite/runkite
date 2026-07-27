package connector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// ErrNotFound is returned when a connector name is not registered.
var ErrNotFound = errors.New("connector not found")

// Registry manages connector configurations and session creation.
type Registry struct {
	connectors map[string]*Connector
	mu         sync.RWMutex
}

// Connector is a single registered external-service connector.
type Connector struct {
	Name   string
	Config ConnectorConfig
	// cached token + expiry for client_credentials (shared across requests)
	cachedToken *CachedToken
	tokenMu     sync.Mutex
	// breaker guards the actual network call in oauth2_* token fetches
	// (master plan: "Circuit breakers: per-connector circuit breakers with
	// configurable thresholds"). api_key/bearer never touch it -- they
	// don't make network calls.
	breaker *CircuitBreaker
}

// CachedToken holds an active credential set with its expiry.
type CachedToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	InstanceURL  string // for Salesforce-style connectors
}

// SessionResponse is the runner-facing credential payload.
type SessionResponse struct {
	Credentials map[string]string `json:"credentials"`
	ExpiresAt   string            `json:"expires_at"`
	MCP         *MCPSession       `json:"mcp,omitempty"`
}

// MCPSession describes a ready-to-use MCP connection.
type MCPSession struct {
	URL   string   `json:"url"`
	Tools []string `json:"tools,omitempty"` // filtered by allow/deny
}

// NewRegistry creates a Registry from a map of connector configs.
// Environment variables in auth fields are expanded at registration time.
func NewRegistry(configs map[string]ConnectorConfig) *Registry {
	r := &Registry{connectors: make(map[string]*Connector, len(configs))}
	for name, cfg := range configs {
		expandAuthConfig(&cfg.Auth)
		r.connectors[name] = &Connector{Name: name, Config: cfg, breaker: NewCircuitBreaker(cfg.CircuitBreaker)}
	}
	return r
}

// Get returns a connector by name or ErrNotFound.
func (r *Registry) Get(name string) (*Connector, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.connectors[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return c, nil
}

// List returns registered connector names in sorted order.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.connectors))
	for name := range r.connectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GetSession performs the auth flow for a connector and returns ready-to-use credentials.
func (r *Registry) GetSession(ctx context.Context, name string, userCtx map[string]interface{}) (*SessionResponse, error) {
	c, err := r.Get(name)
	if err != nil {
		return nil, err
	}

	token, err := c.getToken(ctx, userCtx)
	if err != nil {
		return nil, fmt.Errorf("connector %s: %w", name, err)
	}

	resp := &SessionResponse{
		Credentials: map[string]string{
			"access_token": token.AccessToken,
		},
		ExpiresAt: token.ExpiresAt.Format(time.RFC3339),
	}
	if token.InstanceURL != "" {
		resp.Credentials["instance_url"] = token.InstanceURL
	}
	if token.RefreshToken != "" {
		resp.Credentials["refresh_token"] = token.RefreshToken
	}

	// Attach MCP info if configured
	if c.Config.MCP != nil {
		resp.MCP = &MCPSession{URL: c.Config.MCP.URL}
		if c.Config.Tools != nil {
			resp.MCP.Tools = r.allowedTools(c)
		}
	}

	return resp, nil
}

// BreakerState reports a connector's circuit breaker state ("closed",
// "open", "half_open"), for status/debugging endpoints. Returns "" for an
// unknown connector name.
func (r *Registry) BreakerState(name string) string {
	r.mu.RLock()
	c, ok := r.connectors[name]
	r.mu.RUnlock()
	if !ok {
		return ""
	}
	return c.breaker.State()
}

// IsToolAllowed checks if a specific tool is permitted for a connector.
func (r *Registry) IsToolAllowed(name string, tool string) bool {
	r.mu.RLock()
	c, ok := r.connectors[name]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return isAllowed(c.Config.Tools, tool)
}

// getToken dispatches to the appropriate auth flow.
func (c *Connector) getToken(ctx context.Context, userCtx map[string]interface{}) (*CachedToken, error) {
	switch c.Config.Auth.Type {
	case "api_key":
		return &CachedToken{
			AccessToken: c.Config.Auth.APIKey,
			ExpiresAt:   time.Now().Add(365 * 24 * time.Hour), // effectively never expires
		}, nil

	case "bearer":
		return &CachedToken{
			AccessToken: c.Config.Auth.BearerToken,
			ExpiresAt:   time.Now().Add(365 * 24 * time.Hour),
		}, nil

	case "oauth2_client_credentials":
		return c.getClientCredentialsToken(ctx)

	case "oauth2_token_exchange":
		ssoToken, _ := userCtx["sso_token"].(string)
		if ssoToken == "" {
			return nil, errors.New("sso_token required for token_exchange")
		}
		// Every token_exchange call is a real network call (no cache --
		// each exchange is tied to a specific user's sso_token), so the
		// breaker guards it directly, unlike client_credentials which only
		// needs guarding on its cache-miss path (see getClientCredentialsToken).
		if !c.breaker.Allow() {
			return nil, &ErrCircuitOpen{Connector: c.Name}
		}
		token, err := oauth2TokenExchange(ctx, c.Config.Auth, ssoToken)
		if err != nil {
			c.breaker.RecordFailure()
			return nil, err
		}
		c.breaker.RecordSuccess()
		return token, nil

	default:
		return nil, fmt.Errorf("unsupported auth type: %s", c.Config.Auth.Type)
	}
}

// getClientCredentialsToken returns a cached token or fetches a new one.
func (c *Connector) getClientCredentialsToken(ctx context.Context) (*CachedToken, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Return cached token if still valid (with 60s buffer) -- the breaker
	// only guards the network call below, not this fast path, so a
	// previously-fetched token keeps working fine even while the breaker
	// is open on a since-broken endpoint.
	if c.cachedToken != nil && time.Now().Add(60*time.Second).Before(c.cachedToken.ExpiresAt) {
		return c.cachedToken, nil
	}

	if !c.breaker.Allow() {
		return nil, &ErrCircuitOpen{Connector: c.Name}
	}
	token, err := oauth2ClientCredentials(ctx, c.Config.Auth)
	if err != nil {
		c.breaker.RecordFailure()
		return nil, err
	}
	c.breaker.RecordSuccess()
	c.cachedToken = token
	return token, nil
}

// allowedTools returns the filtered list of MCP tools for a connector.
func (r *Registry) allowedTools(c *Connector) []string {
	if c.Config.Tools == nil {
		return nil
	}
	// If allow list is set, return it (it's the positive filter)
	if len(c.Config.Tools.Allow) > 0 {
		return c.Config.Tools.Allow
	}
	return nil
}

// isAllowed checks a tool name against allow/deny filters.
func isAllowed(filter *ToolFilter, tool string) bool {
	if filter == nil {
		return true // no filter = all allowed
	}
	// Deny list takes precedence
	for _, d := range filter.Deny {
		if d == tool {
			return false
		}
	}
	// If allow list exists, tool must be in it
	if len(filter.Allow) > 0 {
		for _, a := range filter.Allow {
			if a == tool {
				return true
			}
		}
		return false
	}
	return true
}
