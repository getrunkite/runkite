package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// idleEvictAfter bounds memory growth: a per-key bucket not touched for
// this long is evicted. Distinct callers seen once (or agents deleted
// long ago) don't accumulate in memory forever.
const idleEvictAfter = 10 * time.Minute

type bucket struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

// memoryBackend is the process-local token-bucket store. Correct for a
// single control-plane replica; under N replicas each has its own ceiling
// (up to N× the configured limit) -- use redisBackend for shared HA limits.
type memoryBackend struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

func newMemoryBackend() *memoryBackend {
	m := &memoryBackend{buckets: map[string]*bucket{}}
	go m.evictLoop()
	return m
}

func (m *memoryBackend) Allow(key string, rule Rule) bool {
	m.mu.Lock()
	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(rate.Limit(rule.RPS), rule.Burst)}
		m.buckets[key] = b
	}
	b.lastUsed = time.Now()
	m.mu.Unlock()
	return b.limiter.Allow()
}

func (m *memoryBackend) evictLoop() {
	ticker := time.NewTicker(idleEvictAfter)
	defer ticker.Stop()
	for range ticker.C {
		m.evictIdle(time.Now().Add(-idleEvictAfter))
	}
}

func (m *memoryBackend) evictIdle(cutoff time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, b := range m.buckets {
		if b.lastUsed.Before(cutoff) {
			delete(m.buckets, k)
		}
	}
}
