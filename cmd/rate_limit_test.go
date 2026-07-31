package main

import (
	"testing"

	goredis "github.com/redis/go-redis/v9"
)

func TestInitRateLimiter_Unconfigured(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{"graphs": {"echo": "graph.py:graph"}}`)
	l := initRateLimiter(path, nil)
	if l.Enabled() {
		t.Fatal("expected disabled limiter when rate_limit absent")
	}
}

func TestInitRateLimiter_MemoryBackendExplicit(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"rate_limit": {"backend": "memory", "global": {"rps": 1, "burst": 1}}
	}`)
	// Even with a non-nil redis client pointer, explicit memory wins.
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })

	l := initRateLimiter(path, rdb)
	if !l.Enabled() {
		t.Fatal("expected enabled")
	}
	if l.BackendName() != "memory" {
		t.Fatalf("BackendName = %q, want memory", l.BackendName())
	}
}

func TestInitRateLimiter_AutoSelectsRedisWhenClientPresent(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"rate_limit": {"global": {"rps": 1, "burst": 1}}
	}`)
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = rdb.Close() })

	l := initRateLimiter(path, rdb)
	if l.BackendName() != "redis" {
		t.Fatalf("BackendName = %q, want redis (auto when REDIS client present)", l.BackendName())
	}
}

func TestInitRateLimiter_AutoMemoryWhenNoRedis(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"rate_limit": {"global": {"rps": 1, "burst": 1}}
	}`)
	l := initRateLimiter(path, nil)
	if l.BackendName() != "memory" {
		t.Fatalf("BackendName = %q, want memory", l.BackendName())
	}
}
