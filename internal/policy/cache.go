package policy

import (
	"sync"
	"time"
)

// decisionCache is a short-TTL cache keyed on stage/tenant/agent/connector/tool
// (not run-specific fields) so webhook latency stays bounded.
type decisionCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]cacheEntry
}

type cacheEntry struct {
	dec PolicyDecision
	exp time.Time
}

func newDecisionCache(ttl time.Duration) *decisionCache {
	return &decisionCache{ttl: ttl, m: make(map[string]cacheEntry)}
}

func cacheKey(in PolicyInput) string {
	return in.Stage + "\x00" + in.TenantID + "\x00" + in.AgentID + "\x00" + in.Connector + "\x00" + in.Tool
}

func (c *decisionCache) get(in PolicyInput) (PolicyDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[cacheKey(in)]
	if !ok || time.Now().After(e.exp) {
		if ok {
			delete(c.m, cacheKey(in))
		}
		return PolicyDecision{}, false
	}
	return e.dec, true
}

func (c *decisionCache) put(in PolicyInput, dec PolicyDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[cacheKey(in)] = cacheEntry{dec: dec, exp: time.Now().Add(c.ttl)}
}
