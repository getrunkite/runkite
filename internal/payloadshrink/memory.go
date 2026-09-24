package payloadshrink

import (
	"context"
	"errors"
	"sync"
	"time"
)

// MemoryStore is an in-process stash for tests. TTL is ignored.
type MemoryStore struct {
	mu     sync.Mutex
	values map[string][]byte
	ints   map[string]int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		values: map[string][]byte{},
		ints:   map[string]int64{},
	}
}

func (m *MemoryStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[key]
	if !ok {
		return nil, ErrNotFound
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

func (m *MemoryStore) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(value))
	copy(cp, value)
	m.values[key] = cp
	return nil
}

func (m *MemoryStore) IncrBy(_ context.Context, key string, n int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ints[key] += n
	return m.ints[key], nil
}

func (m *MemoryStore) DecrBy(_ context.Context, key string, n int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ints[key] -= n
	return m.ints[key], nil
}

func (m *MemoryStore) Expire(context.Context, string, time.Duration) error {
	return nil
}

// FailStore returns Err on every method so a Redis blip cannot fail the
// already-successful downstream tools/call.
type FailStore struct {
	Err error
}

func (f FailStore) Get(context.Context, string) ([]byte, error) { return nil, f.err() }
func (f FailStore) Set(context.Context, string, []byte, time.Duration) error {
	return f.err()
}
func (f FailStore) IncrBy(context.Context, string, int64) (int64, error) {
	return 0, f.err()
}
func (f FailStore) DecrBy(context.Context, string, int64) (int64, error) {
	return 0, f.err()
}
func (f FailStore) Expire(context.Context, string, time.Duration) error { return f.err() }

func (f FailStore) err() error {
	if f.Err != nil {
		return f.Err
	}
	return errors.New("redis down")
}
