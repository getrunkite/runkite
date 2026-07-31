// Package cors implements CORS header handling for the Agent Protocol
// HTTP surface: a browser-based frontend on a different
// origin -- the common case, e.g. a Vite/React dev server on its own
// port -- cannot reach the control plane at all without this; the
// browser blocks the request client-side before it ever reaches
// application logic, which looks nothing like an auth or network error.
// Opt-in via langgraph.json's "cors" section, same convention as every
// other platform extension -- disabled (zero overhead, no headers added)
// unless configured, correct default for server-to-server or
// same-origin deployments.
package cors

import (
	"net/http"
	"strings"
)

// Config configures allowed origins. A nil Config (or nil/empty
// AllowOrigins) means CORS is disabled -- Middleware becomes a no-op
// pass-through, not "deny everything".
type Config struct {
	AllowOrigins []string
}

// Enabled reports whether any origin is configured.
func (c *Config) Enabled() bool {
	return c != nil && len(c.AllowOrigins) > 0
}

// allows reports whether origin is permitted, and whether that match
// came from an explicit, literal origin string rather than a "*"
// wildcard entry -- the two need different credential handling, see
// Middleware.
func (c *Config) allows(origin string) (allowed bool, explicit bool) {
	if origin == "" {
		return false, false
	}
	for _, entry := range c.AllowOrigins {
		if entry == origin {
			return true, true
		}
	}
	for _, entry := range c.AllowOrigins {
		if entry == "*" {
			return true, false
		}
	}
	return false, false
}

// Middleware adds CORS headers and answers preflight OPTIONS requests
// directly, before the request reaches auth or any handler -- a
// preflight carries no Authorization header by design (that's the whole
// point of a preflight: the browser checks permission before sending the
// real, possibly-credentialed request), so it must never be routed
// through auth.Middleware or it always fails.
func Middleware(cfg *Config, next http.Handler) http.Handler {
	if !cfg.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if allowed, explicit := cfg.allows(origin); allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			// Credentials only for an explicitly-listed origin, never
			// for a "*" wildcard match. Fixed: this used to reflect the
			// request's own Origin header verbatim and unconditionally
			// set Allow-Credentials for ANY origin once "*" was
			// configured -- which is *worse* than a literal
			// "Access-Control-Allow-Origin: *" (browsers refuse that
			// exact combination with credentials), since reflecting the
			// origin instead of sending the literal "*" string sidesteps
			// that browser-side protection entirely. "*" still means
			// "allow any origin" for a non-credentialed (public,
			// cookie-free) API -- it no longer also means "and trust
			// every one of them with credentials."
			if explicit {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}

		if r.Method == http.MethodOptions {
			reqHeaders := r.Header.Get("Access-Control-Request-Headers")
			if reqHeaders == "" {
				reqHeaders = "Authorization, Content-Type"
			}
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ParseAllowOrigins trims whitespace from each configured origin --
// forgiving of a trailing comma/space typo in langgraph.json, since a
// silently-mismatched origin string produces the same confusing
// "blocked by CORS, no error message" symptom as leaving it unconfigured.
func ParseAllowOrigins(raw []string) []string {
	out := make([]string, 0, len(raw))
	for _, o := range raw {
		if trimmed := strings.TrimSpace(o); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
