package auth

import (
	"net/http"
	"strings"
)

// APIKeyEntry describes a single API key and its permissions.
type APIKeyEntry struct {
	Name        string   `json:"name"`
	Permissions []string `json:"permissions,omitempty"`
	// TenantID scopes this key's data for multi-tenancy.
	// Empty resolves to tenant.DefaultTenant -- multi-tenancy is opt-in.
	TenantID string `json:"tenant_id,omitempty"`
	// Extra carries arbitrary per-key metadata into AuthResult.Extra --
	// e.g. a downstream service credential or department tag a graph's
	// tools need at runtime. Same mechanism/purpose as JWT's
	// extra_claims and webhook's passthrough user fields (see jwt.go's
	// doc comments); here the deployer just writes the value directly
	// in langgraph.json since a static key has no token/claims of its
	// own to extract it from.
	Extra map[string]interface{} `json:"extra,omitempty"`
}

// APIKeyProvider authenticates requests using a static key map.
// Accepts keys via Authorization: Bearer <key> or X-API-Key: <key> header.
type APIKeyProvider struct {
	keys map[string]APIKeyEntry
}

// NewAPIKeyProvider creates an API key provider from a key→entry map.
func NewAPIKeyProvider(keys map[string]APIKeyEntry) *APIKeyProvider {
	return &APIKeyProvider{keys: keys}
}

func (p *APIKeyProvider) Authenticate(r *http.Request) (*AuthResult, error) {
	key := extractKey(r)
	if key == "" {
		return nil, &ErrUnauthorized{Message: "missing API key"}
	}

	entry, ok := p.keys[key]
	if !ok {
		return nil, &ErrUnauthorized{Message: "invalid API key"}
	}

	return &AuthResult{
		Identity:    entry.Name,
		Permissions: filterAppPermissions(entry.Permissions),
		TenantID:    entry.TenantID,
		Extra:       entry.Extra,
	}, nil
}

// extractKey tries Authorization: Bearer <key>, then X-API-Key header.
func extractKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}
