package payloadshrink

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisAPI is the go-redis subset Store needs.
type RedisAPI interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	IncrBy(ctx context.Context, key string, value int64) *redis.IntCmd
	DecrBy(ctx context.Context, key string, decrement int64) *redis.IntCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
}

type redisStore struct {
	rdb RedisAPI
}

func NewRedisStore(rdb RedisAPI) Store {
	return &redisStore{rdb: rdb}
}

func (s *redisStore) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := s.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *redisStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.rdb.Set(ctx, key, value, ttl).Err()
}

func (s *redisStore) IncrBy(ctx context.Context, key string, n int64) (int64, error) {
	return s.rdb.IncrBy(ctx, key, n).Result()
}

func (s *redisStore) DecrBy(ctx context.Context, key string, n int64) (int64, error) {
	return s.rdb.DecrBy(ctx, key, n).Result()
}

func (s *redisStore) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return s.rdb.Expire(ctx, key, ttl).Err()
}
