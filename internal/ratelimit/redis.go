package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisClient is the subset of go-redis used by redisBackend -- small so
// tests can stub Eval without a real server when needed.
type RedisClient interface {
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd
}

// redisKeyPrefix matches the rk:* convention used by internal/transport/redis
// so ops tooling (SCAN, FlushAll helpers) stays consistent.
const redisKeyPrefix = "rk:rl:"

// redisEvalTimeout bounds how long a single Allow waits on Redis. A hung
// Redis must not stall every HTTP request; on timeout/error we fail open
// (allow) rather than turning an infra blip into a 429 storm.
const redisEvalTimeout = 100 * time.Millisecond

// tokenBucketScript is an atomic token-bucket Allow: refill based on
// elapsed time since last touch, consume 1 token if available, persist
// tokens+timestamp, and EXPIRE the key so idle buckets don't grow forever
// (Redis-side equivalent of memoryBackend's idle eviction).
//
// KEYS[1] = bucket key
// ARGV[1] = rps (float)
// ARGV[2] = burst (int)
// ARGV[3] = now unix seconds (float)
// ARGV[4] = key TTL seconds
// returns 1 if allowed, 0 if denied
const tokenBucketScript = `
local rps = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local ttl = tonumber(ARGV[4])

local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])

if tokens == nil then
  tokens = burst
  ts = now
end

local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = math.min(burst, tokens + (elapsed * rps))

local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end

redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', KEYS[1], ttl)
return allowed
`

type redisBackend struct {
	rdb RedisClient
}

func newRedisBackend(rdb RedisClient) *redisBackend {
	return &redisBackend{rdb: rdb}
}

func (b *redisBackend) Allow(key string, rule Rule) bool {
	if rule.Burst <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), redisEvalTimeout)
	defer cancel()

	// TTL: keep idle keys around for the same window memory eviction uses,
	// with a floor so a very low rps doesn't expire mid-burst refill math.
	ttlSec := int(idleEvictAfter / time.Second)
	if ttlSec < 60 {
		ttlSec = 60
	}

	res, err := b.rdb.Eval(ctx, tokenBucketScript,
		[]string{redisKeyPrefix + key},
		rule.RPS, rule.Burst, float64(time.Now().UnixNano())/1e9, ttlSec,
	).Int()
	if err != nil {
		// Fail open: a Redis blip must not deny every request (or stall
		// them). Briefly reintroduces "no shared limit" for this check;
		// transport/readyz already surfaces Redis unavailability separately.
		slog.Error("rate_limit: redis eval failed, allowing request", "key", key, "error", err)
		return true
	}
	return res == 1
}
