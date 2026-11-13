package auth

import (
	"crypto/rand"
	"crypto/subtle"
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
// Memory store is process-local with a TTL sweep. When REDIS_URL is set,
// serve wires a Redis-backed SessionStore instead so multi-replica Admin
// UI does not need sticky routing (MCP sessions remain process-local).

const (
	CookieAdminSession   = "runkite_admin_session"
	HeaderCSRF           = "X-CSRF-Token"
	AdminSessionTTL      = 12 * time.Hour
	CookieReviewSession  = "runkite_review_session"
	ReviewSessionTTL     = 15 * time.Minute
	SessionPurposeAdmin  = "admin"
	SessionPurposeReview = "review"
)

// SessionStore persists Admin UI browser sessions (memory or Redis).
type SessionStore interface {
	Create(result *AuthResult) (*AdminSession, error)
	Get(id string) *AdminSession
	Delete(id string) error
	TTL() time.Duration
}

// ReviewSessionCreator is optional. A custom store without this attenuation
// capability cannot create browser review sessions.
type ReviewSessionCreator interface {
	CreateReview(result *AuthResult) (*AdminSession, error)
}

// AdminSession is one browser login.
type AdminSession struct {
	ID              string
	CSRF            string
	Result          AuthResult
	Expires         time.Time
	Purpose         string
	AbsoluteExpires time.Time
}

// AdminSessionStore is the process-local SessionStore (no Redis).
type AdminSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*AdminSession
	ttl      time.Duration
	now      func() time.Time
	purpose  string
}

// NewAdminSessionStore creates an empty memory store. ttl <= 0 uses AdminSessionTTL.
func NewAdminSessionStore(ttl time.Duration) *AdminSessionStore {
	if ttl <= 0 {
		ttl = AdminSessionTTL
	}
	return newSessionStore(ttl, SessionPurposeAdmin)
}

// NewReviewSessionStore keeps review browser credentials separate from Admin.
func NewReviewSessionStore(ttl time.Duration) *AdminSessionStore {
	if ttl <= 0 {
		ttl = ReviewSessionTTL
	}
	return newSessionStore(ttl, SessionPurposeReview)
}

func newSessionStore(ttl time.Duration, purpose string) *AdminSessionStore {
	s := &AdminSessionStore{
		sessions: make(map[string]*AdminSession),
		ttl:      ttl,
		now:      time.Now,
		purpose:  purpose,
	}
	go s.sweepLoop()
	return s
}

// TTL returns the configured sliding session lifetime.
func (s *AdminSessionStore) TTL() time.Duration { return s.ttl }

func (s *AdminSessionStore) sweepLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		now := s.now()
		for id, sess := range s.sessions {
			if sessionExpired(sess, now) {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

// Create mints a session for result and returns the new session
// (caller sets cookies from ID/CSRF).
func (s *AdminSessionStore) Create(result *AuthResult) (*AdminSession, error) {
	if s.purpose != SessionPurposeAdmin {
		return nil, &ErrForbidden{Message: "admin sessions require an Admin store"}
	}
	return s.create(result, SessionPurposeAdmin, time.Time{})
}

func (s *AdminSessionStore) CreateReview(result *AuthResult) (*AdminSession, error) {
	if s.purpose != SessionPurposeReview {
		return nil, &ErrForbidden{Message: "review sessions require a review store"}
	}
	attenuated, ok := reviewAuthResult(result)
	if !ok {
		return nil, &ErrForbidden{Message: "review requires an explicit identity, tenant and role"}
	}
	return s.create(attenuated, SessionPurposeReview, s.now().Add(min(s.ttl, ReviewSessionTTL)))
}

func (s *AdminSessionStore) create(result *AuthResult, purpose string, absolute time.Time) (*AdminSession, error) {
	id, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return nil, err
	}
	copied := copyAuthResult(result)
	sess := &AdminSession{
		ID:              id,
		CSRF:            csrf,
		Result:          copied,
		Expires:         s.now().Add(s.ttl),
		Purpose:         purpose,
		AbsoluteExpires: absolute,
	}
	if !absolute.IsZero() && sess.Expires.After(absolute) {
		sess.Expires = absolute
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
	return copySession(sess), nil
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
	now := s.now()
	if sessionExpired(sess, now) {
		delete(s.sessions, id)
		return nil
	}
	sess.Expires = now.Add(s.ttl)
	if !sess.AbsoluteExpires.IsZero() && sess.Expires.After(sess.AbsoluteExpires) {
		sess.Expires = sess.AbsoluteExpires
	}
	return copySession(sess)
}

func sessionExpired(sess *AdminSession, now time.Time) bool {
	return sess == nil || !now.Before(sess.Expires) || (!sess.AbsoluteExpires.IsZero() && !now.Before(sess.AbsoluteExpires))
}

func copySession(sess *AdminSession) *AdminSession {
	copy := *sess
	copy.Result = copyAuthResult(&sess.Result)
	return &copy
}

// Delete removes a session (logout). Idempotent.
func (s *AdminSessionStore) Delete(id string) error {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
	return nil
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

// csrfTokenMatch is a constant-time compare matching the pprof/metrics
// and runner-token gates elsewhere in this codebase. Length mismatch
// fails closed (ConstantTimeCompare returns 0).
func csrfTokenMatch(got, want string) bool {
	return got != "" && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
