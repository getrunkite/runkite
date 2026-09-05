package auth

import (
	"net"
	"net/http"
	"sync"

	"golang.org/x/time/rate"
)

// Admin login brute-force ceiling: 5 attempts per minute per client IP,
// burst of 5 so a human retrying a mistyped key is not locked out, but
// a script cannot hammer POST /admin-api/session. Independent of the
// optional API rate_limit config -- login is a credential oracle even
// when the rest of the API is unlimited.
const (
	adminLoginRPS   = 5.0 / 60.0
	adminLoginBurst = 5
)

type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{buckets: make(map[string]*rate.Limiter)}
}

func (l *loginLimiter) allow(ip string) bool {
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= 10000 {
			l.buckets = make(map[string]*rate.Limiter)
		}
		lim = rate.NewLimiter(rate.Limit(adminLoginRPS), adminLoginBurst)
		l.buckets[ip] = lim
	}
	return lim.Allow()
}

var defaultLoginLimiter = newLoginLimiter()

// clientIP intentionally ignores X-Forwarded-For: this project has no
// trusted-proxy configuration, so that header is entirely
// attacker-controlled on a directly-exposed deployment. Trusting it would
// let a brute-forcer bypass the limiter below by sending a fresh spoofed
// value on every request while hitting the same connection. RemoteAddr is
// the actual TCP peer and cannot be spoofed by the client. Deployments
// behind a reverse proxy will rate-limit per proxy IP rather than per
// real client until this gets real trusted-proxy support -- coarser, but
// never a no-op.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func loginRetryAfter() string {
	return "12"
}

func (h *AdminSessionHandlers) loginAllowed(r *http.Request) bool {
	if h == nil {
		return true
	}
	if h.AdminProvider == nil && h.Provider == nil {
		return true
	}
	lim := h.loginLimit
	if lim == nil {
		lim = defaultLoginLimiter
	}
	return lim.allow(clientIP(r))
}
