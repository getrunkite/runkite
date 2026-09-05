package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

func TestTryAcquireReclaimLeader_NilRedisAlwaysWins(t *testing.T) {
	ok, err := tryAcquireReclaimLeader(context.Background(), nil, "a", time.Second)
	if err != nil || !ok {
		t.Fatalf("nil redis must allow reclaim (got ok=%v err=%v)", ok, err)
	}
}

func TestInitReclaimMaxAge_Default(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{"graphs": {"echo": "graph.py:graph"}}`)
	if got := initReclaimMaxAge(path); got != defaultReclaimMaxAge {
		t.Fatalf("got %v, want default %v", got, defaultReclaimMaxAge)
	}
}

func TestInitReclaimMaxAge_Configured(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"reclaim": {"max_age": "12s"}
	}`)
	if got := initReclaimMaxAge(path); got != 12*time.Second {
		t.Fatalf("got %v, want 12s", got)
	}
}

func TestInitReclaimMaxAge_EnvOverridesConfig(t *testing.T) {
	t.Setenv("RUNKITE_RECLAIM_MAX_AGE", "9s")
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"reclaim": {"max_age": "12s"}
	}`)
	if got := initReclaimMaxAge(path); got != 9*time.Second {
		t.Fatalf("got %v, want env override 9s", got)
	}
}

func TestInitReclaimMaxAge_InvalidConfigKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"reclaim": {"max_age": "not-a-duration"}
	}`)
	if got := initReclaimMaxAge(path); got != defaultReclaimMaxAge {
		t.Fatalf("got %v, want default %v after invalid config", got, defaultReclaimMaxAge)
	}
}

func TestInitReclaimMaxAge_InvalidEnvKeepsConfig(t *testing.T) {
	t.Setenv("RUNKITE_RECLAIM_MAX_AGE", "nope")
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"reclaim": {"max_age": "10s"}
	}`)
	if got := initReclaimMaxAge(path); got != 10*time.Second {
		t.Fatalf("got %v, want config 10s after invalid env", got)
	}
}

func TestInitReclaimMaxRetries_Default(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{"graphs": {"echo": "graph.py:graph"}}`)
	if got := initReclaimMaxRetries(path); got != defaultReclaimMaxRetries {
		t.Fatalf("got %d, want default %d", got, defaultReclaimMaxRetries)
	}
}

func TestInitReclaimMaxRetries_Configured(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"reclaim": {"max_retries": 5}
	}`)
	if got := initReclaimMaxRetries(path); got != 5 {
		t.Fatalf("got %d, want 5", got)
	}
}

func TestInitReclaimMaxRetries_ZeroMeansUnlimited(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"reclaim": {"max_retries": 0}
	}`)
	if got := initReclaimMaxRetries(path); got != 0 {
		t.Fatalf("got %d, want 0 (unlimited)", got)
	}
}

func TestInitReclaimMaxRetries_InvalidConfigNegativeKeepsDefault(t *testing.T) {
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"reclaim": {"max_retries": -1}
	}`)
	if got := initReclaimMaxRetries(path); got != defaultReclaimMaxRetries {
		t.Fatalf("got %d, want default %d after negative config", got, defaultReclaimMaxRetries)
	}
}

func TestInitReclaimMaxRetries_InvalidEnvKeepsConfig(t *testing.T) {
	t.Setenv("RUNKITE_RECLAIM_MAX_RETRIES", "-1")
	dir := t.TempDir()
	path := writeLangGraphJSON(t, dir, `{
		"graphs": {"echo": "graph.py:graph"},
		"reclaim": {"max_retries": 5}
	}`)
	if got := initReclaimMaxRetries(path); got != 5 {
		t.Fatalf("got %d, want config 5 after invalid env", got)
	}
}

func TestTryAcquireReclaimLeader_OnlyOneWinner(t *testing.T) {
	rdb := redisClientOrSkip(t)
	ctx := context.Background()
	_ = rdb.Del(ctx, reclaimLeaderKey)

	const n = 8
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := tryAcquireReclaimLeader(ctx, rdb, fmt.Sprintf("replica-%d", i), reclaimLeaderTTL)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 reclaim-leader winner across %d replicas, got %d", n, wins)
	}
}

// TestTryAcquireReclaimLeader_HolderRenewsWithoutGap is the regression
// for the blind-SetNX cadence bug: with TTL (3s) > tick (2s), a holder
// that only SetNX'd would lose its next tick while the key still had
// ~1s left, leaving the lock unheld for ~1s every cycle (live-confirmed
// against two real serve replicas). Renew must succeed for the same
// holder, and a different holder must still lose while the lease is live.
func TestTryAcquireReclaimLeader_HolderRenewsWithoutGap(t *testing.T) {
	rdb := redisClientOrSkip(t)
	ctx := context.Background()
	_ = rdb.Del(ctx, reclaimLeaderKey)

	ok, err := tryAcquireReclaimLeader(ctx, rdb, "leader-a", reclaimLeaderTTL)
	if err != nil || !ok {
		t.Fatalf("initial acquire: ok=%v err=%v", ok, err)
	}
	ttl1, err := rdb.PTTL(ctx, reclaimLeaderKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl1 < 2*time.Second {
		t.Fatalf("expected fresh TTL near 3s, got %v", ttl1)
	}

	// Simulate the next 2s tick arriving while the key still has ~1s+ left
	// (same situation that broke blind SetNX).
	time.Sleep(100 * time.Millisecond)
	ok, err = tryAcquireReclaimLeader(ctx, rdb, "leader-a", reclaimLeaderTTL)
	if err != nil || !ok {
		t.Fatalf("same holder must renew: ok=%v err=%v", ok, err)
	}
	ttl2, err := rdb.PTTL(ctx, reclaimLeaderKey).Result()
	if err != nil {
		t.Fatal(err)
	}
	// Renewed PX resets toward full TTL -- must be clearly above the
	// pre-renew remaining time (which would be ~ttl1-100ms without renew).
	if ttl2 < 2*time.Second {
		t.Fatalf("renew did not reset TTL (got %v); lock would still gap", ttl2)
	}

	ok, err = tryAcquireReclaimLeader(ctx, rdb, "leader-b", reclaimLeaderTTL)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("different holder must not steal a live lease")
	}

	val, err := rdb.Get(ctx, reclaimLeaderKey).Result()
	if err != nil || val != "leader-a" {
		t.Fatalf("holder = %q err=%v, want leader-a", val, err)
	}
}

func redisClientOrSkip(t *testing.T) *goredis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not set")
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := goredis.NewClient(opts)
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
	return rdb
}
