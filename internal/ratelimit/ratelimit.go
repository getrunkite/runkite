// Package ratelimit implements per-user, per-agent, and per-tenant rate
// limiting, configurable via langgraph.json's "rate_limit" section.
//
// Two backends:
//   - memory (default): process-local token buckets via golang.org/x/time/rate
//   - redis: shared Lua token buckets on REDIS_URL, so N control-plane
//     replicas share one ceiling instead of each enforcing its own copy
//
// Disabled entirely (zero overhead) unless a rate_limit section is configured.
package ratelimit

import (
	"net/http"

	"github.com/sharanharsoor/runkite/internal/auth"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// Rule configures one token bucket: RPS is the sustained rate, Burst is the
// peak capacity (how many requests can arrive back-to-back before limiting
// kicks in).
type Rule struct {
	RPS   float64
	Burst int
}

// Config is rate-limiting configuration, loaded from langgraph.json's
// "rate_limit" section (see internal/config.RateLimitEntry). Any subset of
// dimensions may be configured; unconfigured dimensions are unlimited.
//
// Backend selects the store: "memory" (process-local, default), "redis"
// (shared across replicas via REDIS_URL), or "" (auto: redis when a
// client is supplied at construction, otherwise memory).
//
// PerTenant is keyed by tenant.FromContext -- which resolves to
// tenant.DefaultTenant when multi-tenancy isn't configured, meaning a
// per_tenant rule on a single-tenant deployment behaves identically to a
// global rule (one bucket, "default"). It only becomes a real per-tenant
// limit once auth is configured to supply real tenant IDs (see the
// Multi-tenancy section of the README).
type Config struct {
	Backend   string // "memory", "redis", or "" (auto)
	Global    *Rule
	PerUser   *Rule
	PerAgent  *Rule
	PerTenant *Rule
}

func (c *Config) enabled() bool {
	return c != nil && (c.Global != nil || c.PerUser != nil || c.PerAgent != nil || c.PerTenant != nil)
}

// ErrRateLimited is returned by Limiter checks and by createRun when a
// configured limit is exceeded. internal/api's handleStoreError maps this
// to HTTP 429.
type ErrRateLimited struct {
	Scope string // "global", "user", "agent", "tenant"
}

func (e *ErrRateLimited) Error() string {
	return "rate limit exceeded (" + e.Scope + ")"
}

// backend is the storage behind Allow* checks. memoryBackend is
// process-local; redisBackend shares one ceiling across replicas.
type backend interface {
	Allow(key string, rule Rule) bool
}

// Limiter enforces a Config's rules using per-scope token buckets.
type Limiter struct {
	cfg *Config
	be  backend
}

// New builds a process-local (memory) Limiter from cfg. cfg may be nil
// (limiter is then always a pass-through, matching "disabled by default").
func New(cfg *Config) *Limiter {
	l := &Limiter{cfg: cfg}
	if cfg.enabled() {
		l.be = newMemoryBackend()
	}
	return l
}

// NewRedis builds a Limiter whose buckets live in Redis (shared across
// every replica using the same REDIS_URL). rdb must be non-nil and already
// pinged by the caller. cfg must be enabled (at least one dimension set).
func NewRedis(cfg *Config, rdb RedisClient) *Limiter {
	l := &Limiter{cfg: cfg}
	if cfg.enabled() {
		l.be = newRedisBackend(rdb)
	}
	return l
}

// Enabled reports whether any dimension is configured. Callers use this to
// skip all rate-limit bookkeeping entirely when it's nil/unconfigured.
func (l *Limiter) Enabled() bool {
	return l != nil && l.cfg.enabled()
}

// BackendName reports which store is in use ("memory", "redis", or ""
// when disabled). Useful for startup logs.
func (l *Limiter) BackendName() string {
	if l == nil || !l.cfg.enabled() {
		return ""
	}
	switch l.be.(type) {
	case *redisBackend:
		return "redis"
	default:
		return "memory"
	}
}

// AllowGlobal checks (and, if allowed, consumes) the global bucket.
func (l *Limiter) AllowGlobal() bool {
	if l == nil || l.cfg == nil || l.cfg.Global == nil || l.be == nil {
		return true
	}
	return l.be.Allow("global", *l.cfg.Global)
}

// AllowUser checks the per-user bucket for identity. An empty identity (no
// auth provider configured, so there's no user to key on) is unlimited for
// this dimension -- global still applies.
func (l *Limiter) AllowUser(identity string) bool {
	if l == nil || l.cfg == nil || l.cfg.PerUser == nil || l.be == nil || identity == "" {
		return true
	}
	return l.be.Allow("user:"+identity, *l.cfg.PerUser)
}

// AllowAgent checks the per-agent bucket for agentID.
func (l *Limiter) AllowAgent(agentID string) bool {
	if l == nil || l.cfg == nil || l.cfg.PerAgent == nil || l.be == nil || agentID == "" {
		return true
	}
	return l.be.Allow("agent:"+agentID, *l.cfg.PerAgent)
}

// AllowTenant checks the per-tenant bucket for tenantID (see
// tenant.FromContext -- always non-empty, "default" when unconfigured).
func (l *Limiter) AllowTenant(tenantID string) bool {
	if l == nil || l.cfg == nil || l.cfg.PerTenant == nil || l.be == nil || tenantID == "" {
		return true
	}
	return l.be.Allow("tenant:"+tenantID, *l.cfg.PerTenant)
}

// Middleware enforces the global, per-user, and per-tenant dimensions.
// Per-agent enforcement happens separately, directly in createRun
// (internal/api), where agent_id is already parsed from the request body
// -- sniffing it generically here would mean buffering and restoring the
// request body on every route just for the few that create runs.
//
// Must be mounted inside auth.Middleware (i.e. auth runs first) so
// auth.FromContext has an identity, and tenant.FromContext has a tenant,
// to key their respective buckets on.
func Middleware(limiter *Limiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if !limiter.AllowGlobal() {
			writeRateLimited(w, "global")
			return
		}
		identity := ""
		if result := auth.FromContext(r.Context()); result != nil {
			identity = result.Identity
		}
		if !limiter.AllowUser(identity) {
			writeRateLimited(w, "user")
			return
		}
		if !limiter.AllowTenant(tenant.FromContext(r.Context())) {
			writeRateLimited(w, "tenant")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeRateLimited(w http.ResponseWriter, scope string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	w.Write([]byte(`{"message":"rate limit exceeded (` + scope + `)"}`))
}
