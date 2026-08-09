package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisClient is the subset of go-redis used by redisAdminSessionStore.
type RedisClient interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	Eval(ctx context.Context, script string, keys []string, args ...interface{}) *redis.Cmd
}

const adminSessionRedisPrefix = "rk:admin:sess:"

// redisEvalTimeout bounds a single session Redis round-trip. On timeout
// or error, Get fails closed (nil session) rather than inventing auth.
const adminSessionRedisTimeout = 200 * time.Millisecond

// getAndSlideScript atomically loads a session blob, refreshes Redis TTL,
// and rewrites the embedded expires unix timestamp (sliding 12h window).
//
// KEYS[1] = session key
// ARGV[1] = now unix seconds
// ARGV[2] = ttl seconds
// returns the updated JSON string, or false if missing/expired
const getAndSlideScript = `
local v = redis.call('GET', KEYS[1])
if not v then
  return false
end
local sess = cjson.decode(v)
local now = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
if not sess.expires or sess.expires < now then
  redis.call('DEL', KEYS[1])
  return false
end
sess.expires = now + ttl
local encoded = cjson.encode(sess)
redis.call('SET', KEYS[1], encoded, 'EX', ttl)
return encoded
`

type redisSessionBlob struct {
	CSRF    string     `json:"csrf"`
	Result  AuthResult `json:"result"`
	Expires int64      `json:"expires"`
}

type redisAdminSessionStore struct {
	rdb RedisClient
	ttl time.Duration
}

// NewRedisAdminSessionStore shares Admin UI sessions via Redis (auto when
// REDIS_URL is set). ttl <= 0 uses AdminSessionTTL.
func NewRedisAdminSessionStore(rdb RedisClient, ttl time.Duration) SessionStore {
	if ttl <= 0 {
		ttl = AdminSessionTTL
	}
	return &redisAdminSessionStore{rdb: rdb, ttl: ttl}
}

func (s *redisAdminSessionStore) TTL() time.Duration { return s.ttl }

func (s *redisAdminSessionStore) key(id string) string {
	return adminSessionRedisPrefix + id
}

func (s *redisAdminSessionStore) Create(result *AuthResult) (*AdminSession, error) {
	id, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	copied := copyAuthResult(result)
	expires := time.Now().Add(s.ttl)
	blob := redisSessionBlob{
		CSRF:    csrf,
		Result:  copied,
		Expires: expires.Unix(),
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminSessionRedisTimeout)
	defer cancel()
	if err := s.rdb.Set(ctx, s.key(id), raw, s.ttl).Err(); err != nil {
		return nil, err
	}
	return &AdminSession{
		ID:      id,
		CSRF:    csrf,
		Result:  copied,
		Expires: expires,
	}, nil
}

func (s *redisAdminSessionStore) Get(id string) *AdminSession {
	if id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminSessionRedisTimeout)
	defer cancel()
	now := time.Now().Unix()
	ttlSec := int64(s.ttl.Seconds())
	if ttlSec < 1 {
		ttlSec = 1
	}
	res, err := s.rdb.Eval(ctx, getAndSlideScript, []string{s.key(id)}, now, ttlSec).Result()
	if err == redis.Nil {
		// Lua false / missing key — not an infra failure.
		return nil
	}
	if err != nil {
		slog.Warn("admin session redis get failed (fail closed)", "err", err)
		return nil
	}
	switch v := res.(type) {
	case string:
		return decodeRedisSession(id, v)
	case []byte:
		return decodeRedisSession(id, string(v))
	default:
		// unexpected type → fail closed
		return nil
	}
}

func (s *redisAdminSessionStore) Delete(id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), adminSessionRedisTimeout)
	defer cancel()
	if err := s.rdb.Del(ctx, s.key(id)).Err(); err != nil {
		slog.Warn("admin session redis delete failed", "err", err)
	}
}

func decodeRedisSession(id, raw string) *AdminSession {
	var blob redisSessionBlob
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		slog.Warn("admin session redis decode failed (fail closed)", "err", err)
		return nil
	}
	return &AdminSession{
		ID:      id,
		CSRF:    blob.CSRF,
		Result:  blob.Result,
		Expires: time.Unix(blob.Expires, 0),
	}
}

func copyAuthResult(result *AuthResult) AuthResult {
	if result == nil {
		return AuthResult{}
	}
	copied := *result
	if result.Extra != nil {
		extra := make(map[string]interface{}, len(result.Extra))
		for k, v := range result.Extra {
			extra[k] = v
		}
		copied.Extra = extra
	}
	if result.Permissions != nil {
		perms := make([]string, len(result.Permissions))
		copy(perms, result.Permissions)
		copied.Permissions = perms
	}
	return copied
}
