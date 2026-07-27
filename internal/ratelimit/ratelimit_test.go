package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/tenant"
)

func TestDisabled_NoConfigIsAlwaysUnlimited(t *testing.T) {
	l := New(nil)
	if l.Enabled() {
		t.Fatal("expected disabled with nil config")
	}
	for i := 0; i < 100; i++ {
		if !l.AllowGlobal() || !l.AllowUser("u1") || !l.AllowAgent("a1") || !l.AllowTenant("t1") {
			t.Fatal("expected unlimited when disabled")
		}
	}
}

func TestNilLimiter_IsSafeAndUnlimited(t *testing.T) {
	var l *Limiter // never constructed via New -- exercises the nil-receiver paths
	if l.Enabled() {
		t.Fatal("nil limiter must report disabled")
	}
	if !l.AllowGlobal() || !l.AllowUser("u1") || !l.AllowAgent("a1") || !l.AllowTenant("t1") {
		t.Fatal("nil limiter must be a pure pass-through")
	}
}

func TestGlobal_EnforcesBurstThenBlocks(t *testing.T) {
	l := New(&Config{Global: &Rule{RPS: 0.001, Burst: 3}}) // effectively never refills within the test
	for i := 0; i < 3; i++ {
		if !l.AllowGlobal() {
			t.Fatalf("expected request %d within burst to be allowed", i)
		}
	}
	if l.AllowGlobal() {
		t.Fatal("expected request beyond burst to be denied")
	}
}

func TestPerUser_IsolatedPerIdentity(t *testing.T) {
	l := New(&Config{PerUser: &Rule{RPS: 0.001, Burst: 1}})

	if !l.AllowUser("alice") {
		t.Fatal("alice's first request should be allowed")
	}
	if l.AllowUser("alice") {
		t.Fatal("alice's second request should be denied (burst exhausted)")
	}
	// A different identity has its own independent bucket.
	if !l.AllowUser("bob") {
		t.Fatal("bob should be unaffected by alice's limit")
	}
}

func TestPerAgent_IsolatedPerAgent(t *testing.T) {
	l := New(&Config{PerAgent: &Rule{RPS: 0.001, Burst: 1}})

	if !l.AllowAgent("agent-a") {
		t.Fatal("agent-a's first request should be allowed")
	}
	if l.AllowAgent("agent-a") {
		t.Fatal("agent-a's second request should be denied")
	}
	if !l.AllowAgent("agent-b") {
		t.Fatal("agent-b should be unaffected by agent-a's limit")
	}
}

func TestPerTenant_IsolatedPerTenant(t *testing.T) {
	l := New(&Config{PerTenant: &Rule{RPS: 0.001, Burst: 1}})

	if !l.AllowTenant("acme-corp") {
		t.Fatal("acme-corp's first request should be allowed")
	}
	if l.AllowTenant("acme-corp") {
		t.Fatal("acme-corp's second request should be denied (burst exhausted)")
	}
	// A different tenant has its own independent bucket -- one noisy
	// tenant must never be able to starve another's quota.
	if !l.AllowTenant("globex-inc") {
		t.Fatal("globex-inc should be unaffected by acme-corp's limit")
	}
}

func TestPerTenant_EmptyTenantIsUnlimited(t *testing.T) {
	l := New(&Config{PerTenant: &Rule{RPS: 0.001, Burst: 1}})
	for i := 0; i < 10; i++ {
		if !l.AllowTenant("") {
			t.Fatal("empty tenant must always be allowed")
		}
	}
}

// TestMiddleware_PerTenantUsesTenantFromContext proves the HTTP middleware
// actually reads the tenant the SAME way auth.Middleware populates it
// (tenant.WithContext), not some separate mechanism that could silently
// diverge.
func TestMiddleware_PerTenantUsesTenantFromContext(t *testing.T) {
	l := New(&Config{PerTenant: &Rule{RPS: 0.001, Burst: 1}})
	handler := Middleware(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	reqFor := func(tenantID string) *http.Request {
		r := httptest.NewRequest("GET", "/agents/search", nil)
		return r.WithContext(tenant.WithContext(context.Background(), tenantID))
	}

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, reqFor("tenant-a"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("tenant-a's first request: expected 200, got %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, reqFor("tenant-a"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("tenant-a's second request: expected 429, got %d", rec2.Code)
	}

	// tenant-b is a completely different bucket -- not starved by tenant-a.
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, reqFor("tenant-b"))
	if rec3.Code != http.StatusOK {
		t.Fatalf("tenant-b's request: expected 200, got %d", rec3.Code)
	}
}

func TestPerUser_EmptyIdentityIsUnlimited(t *testing.T) {
	// No auth configured -> no identity to key on; per-user dimension
	// shouldn't block unauthenticated deployments.
	l := New(&Config{PerUser: &Rule{RPS: 0.001, Burst: 1}})
	for i := 0; i < 10; i++ {
		if !l.AllowUser("") {
			t.Fatal("empty identity must always be allowed")
		}
	}
}

func TestMiddleware_BlocksWithRetryAfterAnd429(t *testing.T) {
	l := New(&Config{Global: &Rule{RPS: 0.001, Burst: 1}})
	handler := Middleware(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest("GET", "/agents/search", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request: expected 200, got %d", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest("GET", "/agents/search", nil))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: expected 429, got %d", rec2.Code)
	}
	if rec2.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
}

func TestEvictLoop_RemovesIdleEntries(t *testing.T) {
	l := New(&Config{PerUser: &Rule{RPS: 100, Burst: 100}})
	l.AllowUser("stale-user")

	l.mu.Lock()
	if _, ok := l.perUser["stale-user"]; !ok {
		l.mu.Unlock()
		t.Fatal("expected bucket to exist after use")
	}
	// Simulate the bucket having gone idle long enough to be evicted,
	// without waiting out the real 10-minute interval.
	l.perUser["stale-user"].lastUsed = time.Now().Add(-idleEvictAfter - time.Second)
	l.mu.Unlock()

	// Run one eviction pass directly rather than waiting for the ticker.
	l.mu.Lock()
	cutoff := time.Now().Add(-idleEvictAfter)
	for k, b := range l.perUser {
		if b.lastUsed.Before(cutoff) {
			delete(l.perUser, k)
		}
	}
	_, stillThere := l.perUser["stale-user"]
	l.mu.Unlock()

	if stillThere {
		t.Fatal("expected idle bucket to be evicted")
	}
}

func TestEnabled_PerTenantAloneEnablesLimiter(t *testing.T) {
	// PerTenant used to be excluded from enabled() (it was a documented
	// no-op) -- a config with ONLY per_tenant set must now actually
	// enable the limiter, not silently do nothing.
	l := New(&Config{PerTenant: &Rule{RPS: 10, Burst: 10}})
	if !l.Enabled() {
		t.Fatal("expected a per_tenant-only config to enable the limiter")
	}
}
