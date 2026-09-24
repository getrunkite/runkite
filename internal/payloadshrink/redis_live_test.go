package payloadshrink

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// The unit and HTTP-layer tests all exercise MemoryStore, never the real
// redisStore adapter -- in particular redis.Nil's translation to
// ErrNotFound (Get) is exactly the kind of detail that type-checks fine
// against the Store interface but can silently diverge from a mock.
// This runs the same shrink+retrieve+budget shape against a live Redis.
func TestRedisStore_LiveGetSetIncrExpire(t *testing.T) {
	url := os.Getenv("PAYLOADSHRINK_REDIS_URL")
	if url == "" {
		t.Skip("PAYLOADSHRINK_REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { rdb.Close() })
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Fatalf("redis not reachable: %v", err)
	}

	store := NewRedisStore(rdb)
	key := "rk:payload:v:live-test-run:1:deadbeefdeadbeef"
	t.Cleanup(func() { rdb.Del(ctx, key) })

	// Miss must be ErrNotFound, not a raw redis.Nil leak.
	if _, err := store.Get(ctx, key); err != ErrNotFound {
		t.Fatalf("want ErrNotFound on miss, got %v", err)
	}

	// Round-trip with a real TTL.
	orig := []byte(`{"content":[{"type":"text","text":"live redis payload"}],"isError":false}`)
	if err := store.Set(ctx, key, orig, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, key)
	if err != nil || string(got) != string(orig) {
		t.Fatalf("round trip: %v %s", err, got)
	}
	ttl := rdb.TTL(ctx, key).Val()
	if ttl <= 0 || ttl > time.Minute {
		t.Fatalf("TTL not applied: %v", ttl)
	}

	// Budget IncrBy/DecrBy against a real INCRBY/DECRBY, not a map.
	bkey := "rk:payload:b:live-test-run:1"
	t.Cleanup(func() { rdb.Del(ctx, bkey) })
	n, err := store.IncrBy(ctx, bkey, 500)
	if err != nil || n != 500 {
		t.Fatalf("IncrBy: %v %d", err, n)
	}
	n, err = store.DecrBy(ctx, bkey, 500)
	if err != nil || n != 0 {
		t.Fatalf("DecrBy: %v %d", err, n)
	}

	// Full Apply/Retrieve shape against the live client, not the fake.
	// MaxBytes must leave enough room for the shrink to actually net a
	// size reduction: the fixed retrieve-hint text alone is ~130 bytes,
	// so a tiny cap here would correctly decline to shrink (stub >=
	// original) rather than prove anything about the live Redis path.
	s := testSettings()
	body := bigRPC(strings.Repeat("live-redis-payload-chunk ", 100))
	stub := Apply(ctx, store, s, "live-apply-run", 7, body)
	if len(stub) >= len(body) {
		t.Fatalf("expected shrink, got same size")
	}
	ref := ParseRef([]byte(`{"ref":"` + extractRef(previewText(mustResult(t, stub), 10_000)) + `"}`))
	t.Cleanup(func() { rdb.Del(ctx, valueKey("live-apply-run", 7, ref)) })
	t.Cleanup(func() { rdb.Del(ctx, budgetKey("live-apply-run", 7)) })
	inner, err := Retrieve(ctx, store, s, "live-apply-run", 7, ref)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	_, origInner, ok := innerResult(body)
	if !ok || string(inner) != string(origInner) {
		t.Fatalf("retrieve mismatch against live redis")
	}
}

func mustResult(t *testing.T, stubBody []byte) []byte {
	t.Helper()
	_, inner, ok := innerResult(stubBody)
	if !ok {
		t.Fatal("stub has no inner result")
	}
	return inner
}
