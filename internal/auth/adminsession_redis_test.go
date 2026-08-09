package auth_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/getrunkite/runkite/internal/auth"
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
		_ = rdb.Close()
		t.Skipf("REDIS_URL set but unreachable: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func flushAdminSessionKeys(t *testing.T, rdb *redis.Client) {
	t.Helper()
	ctx := context.Background()
	iter := rdb.Scan(ctx, 0, "rk:admin:sess:*", 100).Iterator()
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

func TestRedisAdminSession_CreateGetDeleteShared(t *testing.T) {
	rdb := redisClientOrSkip(t)
	flushAdminSessionKeys(t, rdb)

	a := auth.NewRedisAdminSessionStore(rdb, time.Hour)
	b := auth.NewRedisAdminSessionStore(rdb, time.Hour)

	sess, err := a.Create(&auth.AuthResult{
		Identity:    "admin@example.com",
		Permissions: []string{"admin"},
		TenantID:    "acme",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sess.ID == "" || sess.CSRF == "" {
		t.Fatalf("expected id and csrf, got %+v", sess)
	}

	got := b.Get(sess.ID)
	if got == nil {
		t.Fatal("replica B Get: nil")
	}
	if got.CSRF != sess.CSRF || got.Result.Identity != "admin@example.com" || got.Result.TenantID != "acme" {
		t.Fatalf("Get mismatch: %+v", got)
	}
	if len(got.Result.Permissions) != 1 || got.Result.Permissions[0] != "admin" {
		t.Fatalf("permissions: %+v", got.Result.Permissions)
	}

	// Sliding TTL: key should still exist after Get
	ttl := rdb.TTL(context.Background(), "rk:admin:sess:"+sess.ID).Val()
	if ttl < 30*time.Minute {
		t.Fatalf("expected refreshed TTL near 1h, got %v", ttl)
	}

	b.Delete(sess.ID)
	if a.Get(sess.ID) != nil {
		t.Fatal("expected nil after Delete on other store")
	}
}

func TestRedisAdminSession_GetUnknown(t *testing.T) {
	rdb := redisClientOrSkip(t)
	flushAdminSessionKeys(t, rdb)
	store := auth.NewRedisAdminSessionStore(rdb, time.Hour)
	if store.Get("missing-session-id") != nil {
		t.Fatal("expected nil for unknown id")
	}
}

func TestRedisAdminSession_FailClosedOnDeadRedis(t *testing.T) {
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = dead.Close() })
	store := auth.NewRedisAdminSessionStore(dead, time.Hour)

	if _, err := store.Create(&auth.AuthResult{Identity: "x"}); err == nil {
		t.Fatal("Create expected error against dead Redis")
	}
	if store.Get("any") != nil {
		t.Fatal("Get should fail closed (nil) on Redis error")
	}
	// Delete is idempotent / best-effort; must not panic
	store.Delete("any")
}

func TestAdminSessionStore_TTL(t *testing.T) {
	store := auth.NewAdminSessionStore(2 * time.Hour)
	if store.TTL() != 2*time.Hour {
		t.Fatalf("TTL = %v", store.TTL())
	}
	def := auth.NewAdminSessionStore(0)
	if def.TTL() != auth.AdminSessionTTL {
		t.Fatalf("default TTL = %v", def.TTL())
	}
}
