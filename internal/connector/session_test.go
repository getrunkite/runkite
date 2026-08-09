package connector_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/getrunkite/runkite/internal/connector"
)

func TestMemoryConnectorSession_CreateGetDelete(t *testing.T) {
	a := connector.NewMemoryConnectorSessionStore(time.Hour)
	b := connector.NewMemoryConnectorSessionStore(time.Hour) // separate — not shared

	sess, err := a.Create("sf", "run-1", 2, "acme", "sales")
	if err != nil {
		t.Fatal(err)
	}
	got := a.Get(sess.Token)
	if got == nil || got.Connector != "sf" || got.RunID != "run-1" || got.Generation != 2 {
		t.Fatalf("Get = %+v", got)
	}
	if b.Get(sess.Token) != nil {
		t.Fatal("memory stores must not share state")
	}
	a.Delete(sess.Token)
	if a.Get(sess.Token) != nil {
		t.Fatal("expected nil after Delete")
	}
}

func TestMemoryConnectorSession_Expired(t *testing.T) {
	s := connector.NewMemoryConnectorSessionStore(time.Millisecond)
	sess, err := s.Create("sf", "r", 1, "t", "a")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if s.Get(sess.Token) != nil {
		t.Fatal("expected expired session nil")
	}
}

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
		_ = rdb.Close()
		t.Skipf("REDIS_URL set but unreachable: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestRedisConnectorSession_SharedAndFailClosed(t *testing.T) {
	rdb := redisClientOrSkip(t)
	ctx := context.Background()
	iter := rdb.Scan(ctx, 0, "rk:conn:sess:*", 100).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if len(keys) > 0 {
		_ = rdb.Del(ctx, keys...).Err()
	}

	a := connector.NewRedisConnectorSessionStore(rdb, time.Hour)
	b := connector.NewRedisConnectorSessionStore(rdb, time.Hour)
	sess, err := a.Create("sf", "run-1", 3, "acme", "sales")
	if err != nil {
		t.Fatal(err)
	}
	got := b.Get(sess.Token)
	if got == nil || got.Connector != "sf" || got.Generation != 3 {
		t.Fatalf("shared Get = %+v", got)
	}
	b.Delete(sess.Token)
	if a.Get(sess.Token) != nil {
		t.Fatal("expected nil after Delete on other store")
	}

	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = dead.Close() })
	fail := connector.NewRedisConnectorSessionStore(dead, time.Hour)
	if _, err := fail.Create("sf", "r", 1, "t", "a"); err == nil {
		t.Fatal("Create expected error against dead Redis")
	}
	if fail.Get("any") != nil {
		t.Fatal("Get should fail closed")
	}
}
