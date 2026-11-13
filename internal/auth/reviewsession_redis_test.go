package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestReviewRedisNamespaceAttenuationAndAbsoluteExpiry(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL is not configured")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	admin := NewRedisAdminSessionStore(rdb, time.Hour)
	review := NewRedisReviewSessionStore(rdb, time.Hour)
	creator := review.(ReviewSessionCreator)
	result := &AuthResult{Identity: "fixture-reviewer", TenantID: "fixture-tenant", Permissions: []string{"admin"}, DisplayName: "not-retained", Extra: map[string]interface{}{"fixture": "not-retained"}}
	sess, err := creator.CreateReview(result)
	if err != nil {
		t.Fatal(err)
	}
	adminSession, err := admin.Create(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = rdb.Del(ctx, reviewSessionRedisPrefix+sess.ID, adminSessionRedisPrefix+adminSession.ID).Err()
	})
	if admin.Get(sess.ID) != nil || review.Get(adminSession.ID) != nil {
		t.Fatal("Admin and review keyspaces overlap")
	}
	if _, err := admin.(ReviewSessionCreator).CreateReview(result); err == nil {
		t.Fatal("Admin-mode Redis minted review session")
	}
	if _, err := review.Create(result); err == nil {
		t.Fatal("review-mode Redis minted Admin session")
	}
	raw, err := rdb.Get(ctx, reviewSessionRedisPrefix+sess.ID).Result()
	if err != nil {
		t.Fatal(err)
	}
	var blob redisSessionBlob
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		t.Fatal(err)
	}
	if blob.Purpose != SessionPurposeReview || blob.AbsoluteExpires == 0 || blob.Result.Extra != nil || blob.Result.DisplayName != "" || len(blob.Result.Permissions) != 2 {
		t.Fatalf("unattenuated Redis blob: %+v", blob)
	}
	if time.Until(time.UnixMilli(blob.AbsoluteExpires)) > ReviewSessionTTL {
		t.Fatal("review absolute cap exceeded")
	}
	for range 8 {
		got := review.Get(sess.ID)
		if got == nil || got.AbsoluteExpires.UnixMilli() != blob.AbsoluteExpires || got.Expires.After(got.AbsoluteExpires) {
			t.Fatalf("activity extended fixed session: %+v", got)
		}
		remaining := time.Until(time.UnixMilli(blob.AbsoluteExpires))
		ttl := rdb.PTTL(ctx, reviewSessionRedisPrefix+sess.ID).Val()
		if ttl > remaining+10*time.Millisecond {
			t.Fatalf("Redis TTL slid beyond absolute expiry: ttl=%s remaining=%s", ttl, remaining)
		}
	}
	// Keep the Redis key live to prove the Lua check, not key expiration,
	// enforces the absolute bound atomically when activity continues.
	blob.AbsoluteExpires = time.Now().Add(-time.Second).UnixMilli()
	blob.Expires = time.Now().Add(time.Hour).Unix()
	rawBytes, err := json.Marshal(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := rdb.Set(ctx, reviewSessionRedisPrefix+sess.ID, rawBytes, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if review.Get(sess.ID) != nil {
		t.Fatal("expired absolute review session remained valid")
	}
	if count := rdb.Exists(ctx, reviewSessionRedisPrefix+sess.ID).Val(); count != 0 {
		t.Fatal("expired session was not atomically removed")
	}
	if admin.Get(adminSession.ID) == nil {
		t.Fatal("review expiry affected independent Admin session")
	}
}

func TestReviewRedisLeavesLegacyAdminBlobCompatible(t *testing.T) {
	url := os.Getenv("REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL is not configured")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()
	ctx := context.Background()
	admin := NewRedisAdminSessionStore(rdb, time.Hour)
	review := NewRedisReviewSessionStore(rdb, time.Hour)
	id, err := randomToken(16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rdb.Del(ctx, adminSessionRedisPrefix+id).Err() })
	raw := `{"csrf":"fixture-csrf","result":{"identity":"fixture-admin","permissions":["admin"]},"expires":` + fmt.Sprint(time.Now().Add(time.Hour).Unix()) + `}`
	if err := rdb.Set(ctx, adminSessionRedisPrefix+id, raw, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	sess := admin.Get(id)
	if sess == nil || sess.Purpose != "" || !sess.AbsoluteExpires.IsZero() || sess.Result.Identity != "fixture-admin" {
		t.Fatalf("legacy Admin blob changed: %+v", sess)
	}
	if review.Get(id) != nil {
		t.Fatal("legacy Admin credential entered review namespace")
	}
}
