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

func TestTryAcquireReclaimLeader_OnlyOneWinner(t *testing.T) {
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
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis ping: %v", err)
	}
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
				t.Errorf("SetNX: %v", err)
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
