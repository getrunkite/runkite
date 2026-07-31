package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

// WebhookConfig configures the webhook auth provider.
type WebhookConfig struct {
	URL             string `json:"url"`
	TimeoutMs       int    `json:"timeout_ms"`
	CacheTTLSeconds int    `json:"cache_ttl_seconds"`
	// CacheMaxEntries bounds the auth result cache's size -- see
	// WebhookProvider's own doc comment for why an unbounded map here
	// was a real, not theoretical, memory-growth risk. 0 (the default,
	// no config needed for the common case) applies defaultCacheMaxEntries.
	CacheMaxEntries int `json:"cache_max_entries,omitempty"`
}

const defaultCacheMaxEntries = 10_000

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
	result *AuthResult
	err    error
}

// WebhookProvider delegates auth to an external HTTP sidecar.
type WebhookProvider struct {
	url    string
	client *http.Client
	// cache is nil when CacheTTLSeconds <= 0 (caching disabled --
	// same "nil means off" convention as everywhere else in this
	// codebase, e.g. hooks.Dispatcher's own nil-receiver methods).
	//
	// A bounded, TTL-evicting LRU rather than a plain map: cacheKeyFor
	// hashes credential material together with request method+path
	// (see its own doc comment for why), which means the cache grows
	// one entry per unique (caller, method, URL path) combination --
	// for a REST API whose paths embed resource IDs (e.g.
	// /threads/{threadID}/runs/{runID}), that is effectively one entry
	// per distinct thread/run a caller has ever touched, not one entry
	// per caller. A plain map with only a read-time expiry check (the
	// original implementation here) never actually removed expired
	// entries, so this grew without bound for the lifetime of a
	// long-running control plane -- a real memory leak under normal
	// traffic, not a hypothetical one. expirable.LRU evicts the truly
	// oldest entry once CacheMaxEntries is hit AND independently expires
	// entries past ttl, closing both the unbounded-size and the
	// never-swept-stale-entries versions of the same underlying problem.
	cache *lru.LRU[string, cacheEntry]
}

// NewWebhookProvider creates a webhook auth provider.
func NewWebhookProvider(cfg WebhookConfig) *WebhookProvider {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	ttl := time.Duration(cfg.CacheTTLSeconds) * time.Second

	p := &WebhookProvider{
		url:    cfg.URL,
		client: &http.Client{Timeout: timeout},
	}
	if ttl > 0 {
		maxEntries := cfg.CacheMaxEntries
		if maxEntries <= 0 {
			maxEntries = defaultCacheMaxEntries
		}
		p.cache = lru.NewLRU[string, cacheEntry](maxEntries, nil, ttl)
	}
	return p
}

func (p *WebhookProvider) Authenticate(r *http.Request) (*AuthResult, error) {
	if p.cache == nil {
		return p.callWebhook(r)
	}

	cacheKey := p.cacheKeyFor(r)
	if entry, ok := p.cache.Get(cacheKey); ok {
		return entry.result, entry.err
	}

	result, err := p.callWebhook(r)
	// Cache the result (both success and failure) -- unchanged behavior
	// from before this fix, just via the bounded cache now.
	p.cache.Add(cacheKey, cacheEntry{result: result, err: err})
	return result, err
}

// CacheLen returns the current number of cached auth results, mainly for
// tests that need to confirm the size bound actually holds. 0 if caching
// is disabled (CacheTTLSeconds <= 0).
func (p *WebhookProvider) CacheLen() int {
	if p.cache == nil {
		return 0
	}
	return p.cache.Len()
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
