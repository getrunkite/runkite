package ratelimit

import (
	"context"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"
)

func redisClientOrSkip(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func flushRateLimitKeys(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx := context.Background()
	iter := rdb.Scan(ctx, 0, redisKeyPrefix+"*", 100).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(keys) > 0 {
		if err := rdb.Del(ctx, keys...).Err(); err != nil {
			t.Fatalf("del: %v", err)
		}
	}
}

func TestRedis_EnforcesBurstThenBlocks(t *testing.T) {
	rdb := redisClientOrSkip(t)
	flushRateLimitKeys(t, rdb)

	l := NewRedis(&Config{Global: &Rule{RPS: 0.001, Burst: 3}}, rdb)
	if l.BackendName() != "redis" {
		t.Fatalf("BackendName = %q, want redis", l.BackendName())
	}
	for i := 0; i < 3; i++ {
		if !l.AllowGlobal() {
			t.Fatalf("expected request %d within burst to be allowed", i)
		}
	}
	if l.AllowGlobal() {
		t.Fatal("expected request beyond burst to be denied")
	}
}

func TestRedis_SharedCeilingAcrossTwoLimiters(t *testing.T) {
	// The actual multi-instance proof: two Limiter instances (standing in
	// for two control-plane replicas) sharing one Redis must share one
	// burst ceiling, not get 2×Burst.
	rdb := redisClientOrSkip(t)
	flushRateLimitKeys(t, rdb)

	cfg := &Config{Global: &Rule{RPS: 0.001, Burst: 3}}
	a := NewRedis(cfg, rdb)
	b := NewRedis(cfg, rdb)

	allowed := 0
	for i := 0; i < 10; i++ {
		lim := a
		if i%2 == 1 {
			lim = b
		}
		if lim.AllowGlobal() {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("shared ceiling: allowed %d across two limiters, want exactly burst=3 (not 6)", allowed)
	}
}

func TestRedis_PerUserIsolated(t *testing.T) {
	rdb := redisClientOrSkip(t)
	flushRateLimitKeys(t, rdb)

	l := NewRedis(&Config{PerUser: &Rule{RPS: 0.001, Burst: 1}}, rdb)
	if !l.AllowUser("alice") {
		t.Fatal("alice's first request should be allowed")
	}
	if l.AllowUser("alice") {
		t.Fatal("alice's second request should be denied")
	}
	if !l.AllowUser("bob") {
		t.Fatal("bob should be unaffected by alice's limit")
	}
}

func TestRedis_FailOpenOnEvalError(t *testing.T) {
	// Closed client → Eval fails → must allow (fail open), not deny.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = rdb.Close()

	l := NewRedis(&Config{Global: &Rule{RPS: 1, Burst: 1}}, rdb)
	for i := 0; i < 5; i++ {
		if !l.AllowGlobal() {
			t.Fatal("expected fail-open (allow) when redis eval fails")
		}
	}
}
