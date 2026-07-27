package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// WebhookConfig configures the webhook auth provider.
type WebhookConfig struct {
	URL             string `json:"url"`
	TimeoutMs       int    `json:"timeout_ms"`
	CacheTTLSeconds int    `json:"cache_ttl_seconds"`
}

// webhookResponse is the expected response from the auth webhook. User is
// decoded generically (not a fixed struct) so any extra fields a custom
// auth sidecar returns (email, an internal user ID, upstream tokens to
// forward, etc.) survive into AuthResult.Extra for the Factory Graph
// runtime.user passthrough, instead of being silently dropped by a fixed
// set of known keys.
type webhookResponse struct {
	Allow   bool                   `json:"allow"`
	Status  int                    `json:"status,omitempty"`
	Message string                 `json:"message,omitempty"`
	User    map[string]interface{} `json:"user,omitempty"`
}

func stringField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func stringSliceField(m map[string]interface{}, key string) []string {
	raw, ok := m[key].([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

type cacheEntry struct {
	result  *AuthResult
	err     error
	expires time.Time
}

// WebhookProvider delegates auth to an external HTTP sidecar.
type WebhookProvider struct {
	url     string
	client  *http.Client
	cacheMu sync.RWMutex
	cache   map[string]cacheEntry
	ttl     time.Duration
}

// NewWebhookProvider creates a webhook auth provider.
func NewWebhookProvider(cfg WebhookConfig) *WebhookProvider {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second

	return &WebhookProvider{
		url:    cfg.URL,
		client: &http.Client{Timeout: timeout},
		cache:  make(map[string]cacheEntry),
		ttl:    ttl,
	}
}

func (p *WebhookProvider) Authenticate(r *http.Request) (*AuthResult, error) {
	cacheKey := p.cacheKeyFor(r)

	// Check cache
	if p.ttl > 0 {
		p.cacheMu.RLock()
		if entry, ok := p.cache[cacheKey]; ok && time.Now().Before(entry.expires) {
			p.cacheMu.RUnlock()
			return entry.result, entry.err
		}
		p.cacheMu.RUnlock()
	}

	result, err := p.callWebhook(r)

	// Cache the result (both success and failure)
	if p.ttl > 0 {
		p.cacheMu.Lock()
		p.cache[cacheKey] = cacheEntry{
			result:  result,
			err:     err,
			expires: time.Now().Add(p.ttl),
		}
		p.cacheMu.Unlock()
	}

	return result, err
}

func (p *WebhookProvider) callWebhook(r *http.Request) (*AuthResult, error) {
	// Send request headers as JSON
	headers := make(map[string]string, len(r.Header))
	for k := range r.Header {
		headers[k] = r.Header.Get(k)
	}

	body, _ := json.Marshal(map[string]interface{}{
		"headers": headers,
		"method":  r.Method,
		"path":    r.URL.Path,
	})

	resp, err := p.client.Post(p.url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, &ErrUnauthorized{Message: fmt.Sprintf("auth webhook unreachable: %v", err)}
	}
	defer resp.Body.Close()

	var wResp webhookResponse
	if err := json.NewDecoder(resp.Body).Decode(&wResp); err != nil {
		return nil, &ErrUnauthorized{Message: "auth webhook returned invalid response"}
	}

	if !wResp.Allow {
		msg := "access denied"
		if wResp.Message != "" {
			msg = wResp.Message
		}
		status := wResp.Status
		if status == http.StatusForbidden {
			return nil, &ErrForbidden{Message: msg}
		}
		return nil, &ErrUnauthorized{Message: msg}
	}

	result := &AuthResult{Identity: "webhook-user"}
	if wResp.User != nil {
		if id := stringField(wResp.User, "identity"); id != "" {
			result.Identity = id
		}
		result.Permissions = filterAppPermissions(stringSliceField(wResp.User, "permissions"))
		result.TenantID = stringField(wResp.User, "tenant_id")
		result.DisplayName = stringField(wResp.User, "display_name")

		result.Extra = make(map[string]interface{}, len(wResp.User))
		for k, v := range wResp.User {
			switch k {
			case "identity", "permissions", "tenant_id", "display_name":
				// already surfaced as first-class AuthResult fields above
			default:
				result.Extra[k] = v
			}
		}
	}
	return result, nil
}

// cacheKeyFor hashes the credential material AND the request method/path.
// Authorization alone is not enough: API-key clients often send
// X-API-Key with an empty Authorization, which previously collapsed
// every such caller onto one cache entry (identity/permission mixup).
// Method+path are included because the webhook request body forwards
// them and sidecars may set allow=false based on route -- caching
// identity-only would let a prior allow bypass a later path deny.
func (p *WebhookProvider) cacheKeyFor(r *http.Request) string {
	h := sha256.New()
	h.Write([]byte(r.Header.Get("Authorization")))
	h.Write([]byte{0})
	h.Write([]byte(r.Header.Get("X-API-Key")))
	h.Write([]byte{0})
	h.Write([]byte(r.Method))
	h.Write([]byte{0})
	h.Write([]byte(r.URL.Path))
	return hex.EncodeToString(h.Sum(nil))
}
