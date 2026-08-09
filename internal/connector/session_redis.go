package connector

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// SessionRedisClient is the subset of go-redis used by the Redis store.
type SessionRedisClient interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value interface{}, expiration time.Duration) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

const connectorSessionRedisPrefix = "rk:conn:sess:"

const connectorSessionRedisTimeout = 200 * time.Millisecond

type redisSessionBlob struct {
	Connector  string `json:"connector"`
	RunID      string `json:"run_id"`
	Generation int64  `json:"generation"`
	TenantID   string `json:"tenant_id"`
	AgentID    string `json:"agent_id"`
	Expires    int64  `json:"expires"`
}

type redisConnectorSessionStore struct {
	rdb SessionRedisClient
	ttl time.Duration
}

// NewRedisConnectorSessionStore shares MCP capability tokens via Redis
// (auto when REDIS_URL is set). ttl <= 0 uses ConnectorSessionTTL.
func NewRedisConnectorSessionStore(rdb SessionRedisClient, ttl time.Duration) ConnectorSessionStore {
	if ttl <= 0 {
		ttl = ConnectorSessionTTL
	}
	return &redisConnectorSessionStore{rdb: rdb, ttl: ttl}
}

func (s *redisConnectorSessionStore) TTL() time.Duration { return s.ttl }

func (s *redisConnectorSessionStore) key(token string) string {
	return connectorSessionRedisPrefix + token
}

func (s *redisConnectorSessionStore) Create(connectorName, runID string, generation int64, tenantID, agentID string) (*ConnectorSession, error) {
	tok, err := randomSessionToken(32)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(s.ttl)
	blob := redisSessionBlob{
		Connector:  connectorName,
		RunID:      runID,
		Generation: generation,
		TenantID:   tenantID,
		AgentID:    agentID,
		Expires:    expires.Unix(),
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectorSessionRedisTimeout)
	defer cancel()
	if err := s.rdb.Set(ctx, s.key(tok), raw, s.ttl).Err(); err != nil {
		return nil, err
	}
	return &ConnectorSession{
		Token:      tok,
		Connector:  connectorName,
		RunID:      runID,
		Generation: generation,
		TenantID:   tenantID,
		AgentID:    agentID,
		Expires:    expires,
	}, nil
}

func (s *redisConnectorSessionStore) Get(token string) *ConnectorSession {
	if token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectorSessionRedisTimeout)
	defer cancel()
	raw, err := s.rdb.Get(ctx, s.key(token)).Result()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		slog.Warn("connector session redis get failed (fail closed)", "err", err)
		return nil
	}
	var blob redisSessionBlob
	if err := json.Unmarshal([]byte(raw), &blob); err != nil {
		slog.Warn("connector session redis decode failed (fail closed)", "err", err)
		return nil
	}
	if time.Now().Unix() > blob.Expires {
		_, _ = s.rdb.Del(ctx, s.key(token)).Result()
		return nil
	}
	return &ConnectorSession{
		Token:      token,
		Connector:  blob.Connector,
		RunID:      blob.RunID,
		Generation: blob.Generation,
		TenantID:   blob.TenantID,
		AgentID:    blob.AgentID,
		Expires:    time.Unix(blob.Expires, 0),
	}
}

func (s *redisConnectorSessionStore) Delete(token string) {
	if token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectorSessionRedisTimeout)
	defer cancel()
	if err := s.rdb.Del(ctx, s.key(token)).Err(); err != nil {
		slog.Warn("connector session redis delete failed", "err", err)
	}
}
