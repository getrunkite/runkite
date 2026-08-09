package connector

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// HeaderConnectorSession is the runner-facing capability token minted by
// GetSession for MCP connectors and required on POST .../mcp.
const HeaderConnectorSession = "X-Runkite-Connector-Session"

// ConnectorSessionTTL is the absolute lifetime of a capability token.
// Not sliding — a stolen token dies after this window.
const ConnectorSessionTTL = 15 * time.Minute

// StaticCredentialSessionTTL is the advisory expires_at for api_key/bearer
// GetSession responses. Remint hint only — does not revoke the underlying secret.
const StaticCredentialSessionTTL = time.Hour

// ConnectorSession is one short-lived MCP capability for a run.
type ConnectorSession struct {
	Token      string
	Connector  string
	RunID      string
	Generation int64
	TenantID   string
	AgentID    string
	Expires    time.Time
}

// ConnectorSessionStore mints and looks up MCP capability tokens.
type ConnectorSessionStore interface {
	Create(connector, runID string, generation int64, tenantID, agentID string) (*ConnectorSession, error)
	Get(token string) *ConnectorSession
	Delete(token string)
	TTL() time.Duration
}

// MemoryConnectorSessionStore is the process-local store (no Redis).
type MemoryConnectorSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*ConnectorSession
	ttl      time.Duration
}

// NewMemoryConnectorSessionStore creates an empty memory store.
// ttl <= 0 uses ConnectorSessionTTL.
func NewMemoryConnectorSessionStore(ttl time.Duration) *MemoryConnectorSessionStore {
	if ttl <= 0 {
		ttl = ConnectorSessionTTL
	}
	s := &MemoryConnectorSessionStore{
		sessions: make(map[string]*ConnectorSession),
		ttl:      ttl,
	}
	go s.sweepLoop()
	return s
}

func (s *MemoryConnectorSessionStore) TTL() time.Duration { return s.ttl }

func (s *MemoryConnectorSessionStore) sweepLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		now := time.Now()
		for id, sess := range s.sessions {
			if now.After(sess.Expires) {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

func (s *MemoryConnectorSessionStore) Create(connectorName, runID string, generation int64, tenantID, agentID string) (*ConnectorSession, error) {
	tok, err := randomSessionToken(32)
	if err != nil {
		return nil, err
	}
	sess := &ConnectorSession{
		Token:      tok,
		Connector:  connectorName,
		RunID:      runID,
		Generation: generation,
		TenantID:   tenantID,
		AgentID:    agentID,
		Expires:    time.Now().Add(s.ttl),
	}
	s.mu.Lock()
	s.sessions[tok] = sess
	s.mu.Unlock()
	return sess, nil
}

func (s *MemoryConnectorSessionStore) Get(token string) *ConnectorSession {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[token]
	if !ok {
		return nil
	}
	if time.Now().After(sess.Expires) {
		delete(s.sessions, token)
		return nil
	}
	// Absolute TTL — do not slide.
	copied := *sess
	return &copied
}

func (s *MemoryConnectorSessionStore) Delete(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

func randomSessionToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
