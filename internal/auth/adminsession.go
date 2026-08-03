package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// Admin UI browser sessions (httpOnly cookie + CSRF). Closes the
// XSS-readable Bearer token gap documented in docs/limitations.md:
// the API key/JWT is verified once at login and never stored in
// JavaScript; subsequent /admin-api/* calls authenticate via the
// session cookie. Machine clients keep using Authorization: Bearer
// (no cookie, no CSRF).
//
// Store is process-local with a TTL sweep -- same class of trade-off as
// MCP session ownership. Multi-replica Admin UI needs sticky routing to
// the replica that minted the session (or a shared store later).

const (
	CookieAdminSession = "runkite_admin_session"
	HeaderCSRF         = "X-CSRF-Token"
	AdminSessionTTL    = 12 * time.Hour
)

// AdminSession is one browser login.
type AdminSession struct {
	ID      string
	CSRF    string
	Result  AuthResult
	Expires time.Time
}

// AdminSessionStore holds active Admin UI sessions.
type AdminSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*AdminSession
	ttl      time.Duration
}

// NewAdminSessionStore creates an empty store. ttl <= 0 uses AdminSessionTTL.
func NewAdminSessionStore(ttl time.Duration) *AdminSessionStore {
	if ttl <= 0 {
		ttl = AdminSessionTTL
	}
	s := &AdminSessionStore{
		sessions: make(map[string]*AdminSession),
		ttl:      ttl,
	}
	go s.sweepLoop()
	return s
}

func (s *AdminSessionStore) sweepLoop() {
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

// Create mints a session for result and returns the new session
// (caller sets cookies from ID/CSRF).
func (s *AdminSessionStore) Create(result *AuthResult) (*AdminSession, error) {
	id, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
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
	sess := &AdminSession{
		ID:      id,
		CSRF:    csrf,
		Result:  copied,
		Expires: time.Now().Add(s.ttl),
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return sess, nil
}

// Get returns a live session by id, sliding its expiry. nil if missing/expired.
func (s *AdminSessionStore) Get(id string) *AdminSession {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	if time.Now().After(sess.Expires) {
		delete(s.sessions, id)
		return nil
	}
	sess.Expires = time.Now().Add(s.ttl)
	return sess
}

// Delete removes a session (logout). Idempotent.
func (s *AdminSessionStore) Delete(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// SessionIDFromRequest reads the admin session cookie.
func SessionIDFromRequest(r *http.Request) string {
	c, err := r.Cookie(CookieAdminSession)
	if err != nil || c == nil {
		return ""
	}
	return c.Value
}

// SetSessionCookie writes the httpOnly session cookie.
func SetSessionCookie(w http.ResponseWriter, r *http.Request, sessionID string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = AdminSessionTTL
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieAdminSession,
		Value:    sessionID,
		Path:     "/admin-api",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestLooksHTTPS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieAdminSession,
		Value:    "",
		Path:     "/admin-api",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestLooksHTTPS(r),
		MaxAge:   -1,
	})
}

func requestLooksHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}

func randomToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func hasAuthHeader(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != ""
}

func isMutatingMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}
