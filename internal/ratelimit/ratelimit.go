// Package ratelimit implements a "Rate limiting: per-user, per-agent,
// per-tenant, configurable via config" platform extension.
//
// Token-bucket limiting via golang.org/x/time/rate (already a transitive
// dependency of this project via grpc -- no new dependency needed). Disabled
// entirely (zero overhead) unless a rate_limit section is configured.
package ratelimit

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

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
// PerTenant is keyed by tenant.FromContext -- which resolves to
// tenant.DefaultTenant when multi-tenancy isn't configured, meaning a
// per_tenant rule on a single-tenant deployment behaves identically to a
// global rule (one bucket, "default"). It only becomes a real per-tenant
// limit once auth is configured to supply real tenant IDs (see the
// Multi-tenancy section of the README).
type Config struct {
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

// idleEvictAfter bounds memory growth: a per-user or per-agent bucket not
// touched for this long is evicted. Distinct callers seen once (or agents
// deleted long ago) don't accumulate in memory forever.
const idleEvictAfter = 10 * time.Minute

type bucket struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

// Limiter enforces a Config's rules using per-scope token buckets.
type Limiter struct {
	cfg    *Config
	global *rate.Limiter

	mu        sync.Mutex
	perUser   map[string]*bucket
	perAgent  map[string]*bucket
	perTenant map[string]*bucket
}

// New builds a Limiter from cfg. cfg may be nil (limiter is then always a
// pass-through, matching "disabled by default").
func New(cfg *Config) *Limiter {
	l := &Limiter{cfg: cfg, perUser: map[string]*bucket{}, perAgent: map[string]*bucket{}, perTenant: map[string]*bucket{}}
	if cfg != nil && cfg.Global != nil {
		l.global = rate.NewLimiter(rate.Limit(cfg.Global.RPS), cfg.Global.Burst)
	}
	if cfg.enabled() {
		go l.evictLoop()
	}
	return l
}

// Enabled reports whether any dimension is configured. Callers use this to
// skip all rate-limit bookkeeping entirely when it's nil/unconfigured.
func (l *Limiter) Enabled() bool {
	return l != nil && l.cfg.enabled()
}

// AllowGlobal checks (and, if allowed, consumes) the global bucket.
func (l *Limiter) AllowGlobal() bool {
	if l == nil || l.global == nil {
		return true
	}
	return l.global.Allow()
}

// AllowUser checks the per-user bucket for identity. An empty identity (no
// auth provider configured, so there's no user to key on) is unlimited for
// this dimension -- global still applies.
func (l *Limiter) AllowUser(identity string) bool {
	if l == nil || l.cfg == nil || l.cfg.PerUser == nil || identity == "" {
		return true
	}
	return l.allow(l.perUser, identity, l.cfg.PerUser)
}

// AllowAgent checks the per-agent bucket for agentID.
func (l *Limiter) AllowAgent(agentID string) bool {
	if l == nil || l.cfg == nil || l.cfg.PerAgent == nil || agentID == "" {
		return true
	}
	return l.allow(l.perAgent, agentID, l.cfg.PerAgent)
}

// AllowTenant checks the per-tenant bucket for tenantID (see
// tenant.FromContext -- always non-empty, "default" when unconfigured).
func (l *Limiter) AllowTenant(tenantID string) bool {
	if l == nil || l.cfg == nil || l.cfg.PerTenant == nil || tenantID == "" {
		return true
	}
	return l.allow(l.perTenant, tenantID, l.cfg.PerTenant)
}

func (l *Limiter) allow(m map[string]*bucket, key string, rule *Rule) bool {
	l.mu.Lock()
	b, ok := m[key]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(rate.Limit(rule.RPS), rule.Burst)}
		m[key] = b
	}
	b.lastUsed = time.Now()
	l.mu.Unlock()
	return b.limiter.Allow()
}

func (l *Limiter) evictLoop() {
	ticker := time.NewTicker(idleEvictAfter)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-idleEvictAfter)
		l.mu.Lock()
		for k, b := range l.perUser {
			if b.lastUsed.Before(cutoff) {
				delete(l.perUser, k)
			}
		}
		for k, b := range l.perAgent {
			if b.lastUsed.Before(cutoff) {
				delete(l.perAgent, k)
			}
		}
		for k, b := range l.perTenant {
			if b.lastUsed.Before(cutoff) {
				delete(l.perTenant, k)
			}
		}
		l.mu.Unlock()
	}
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
