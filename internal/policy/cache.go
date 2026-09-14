package policy

import (
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

// defaultDecisionCacheMaxEntries bounds the Decide cache once argument
// digests are part of the key. Same default as the auth webhook LRU:
// credential+path keys had the same high-cardinality shape. Expired
// entries also drop via TTL; a size cap is what stops distinct amounts
// from growing the map for the process lifetime.
const defaultDecisionCacheMaxEntries = 10_000

// decisionCache is a short-TTL LRU keyed on
// stage/tenant/agent/principal/connector/tool/args_digest (not
// run_id/generation) so webhook latency stays bounded without
// cross-contaminating principals or argument values. A BYO webhook PDP
// can allow Alice / deny Bob for the same tool; omitting Principal
// would leak Alice's allow. Omitting ArgsDigest would reuse a $50
// allow for a $250 call.
type decisionCache struct {
	lru *lru.LRU[string, PolicyDecision]
}

func newDecisionCache(ttl time.Duration) *decisionCache {
	if ttl <= 0 {
		return nil
	}
	return &decisionCache{
		lru: lru.NewLRU[string, PolicyDecision](defaultDecisionCacheMaxEntries, nil, ttl),
	}
}

func cacheKey(in PolicyInput) string {
	return in.Stage + "\x00" + in.TenantID + "\x00" + in.AgentID + "\x00" +
		in.Principal + "\x00" + in.Connector + "\x00" + in.Tool + "\x00" + in.ArgsDigest
}

func (c *decisionCache) get(in PolicyInput) (PolicyDecision, bool) {
	if c == nil || c.lru == nil {
		return PolicyDecision{}, false
	}
	return c.lru.Get(cacheKey(in))
}

func (c *decisionCache) put(in PolicyInput, dec PolicyDecision) {
	if c == nil || c.lru == nil {
		return
	}
	c.lru.Add(cacheKey(in), dec)
}

func (c *decisionCache) clear() {
	if c == nil || c.lru == nil {
		return
	}
	c.lru.Purge()
}

func (c *decisionCache) len() int {
	if c == nil || c.lru == nil {
		return 0
	}
	return c.lru.Len()
}
