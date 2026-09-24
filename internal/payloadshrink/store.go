package payloadshrink

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is a cache miss (expired TTL, wrong ref, or never stored).
var ErrNotFound = errors.New("payload not found")

// Store is the Redis (or test) backend for original MCP result bytes.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	IncrBy(ctx context.Context, key string, n int64) (int64, error)
	DecrBy(ctx context.Context, key string, n int64) (int64, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
}
