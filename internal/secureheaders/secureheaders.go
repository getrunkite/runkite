// Package secureheaders adds a small, always-on set of HTTP security
// headers to every response. Complements CORS (which only handles
// cross-origin access) -- these headers reduce clickjacking, MIME
// sniffing, and XSS blast radius for the embedded Admin UI and the rest
// of the Agent Protocol surface.
//
// Always enabled (unlike CORS/rate_limit): there is no legitimate reason
// to serve HTML/JSON without nosniff/frame protection, and the headers
// are harmless on pure JSON API responses.
package secureheaders

import "net/http"

// contentSecurityPolicy is tuned for the embedded Admin SPA at /admin/
// (Vite-built hashed assets + self-hosted fonts) while remaining safe on
// JSON API responses. 'unsafe-inline' is required for style-src because
// the Admin UI uses React inline style attributes (e.g. dynamic chart
// bar widths) -- script-src stays strict ('self' only, no inline JS).
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; font-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"

// Middleware sets security headers on every response, then delegates.
// Intended to wrap the whole HTTP surface outside auth (same band as
// CORS) so public paths like /admin/, /health, and /metrics get them too.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		// HSTS is deliberately omitted: TLS is optional on this process
		// (often terminated at a reverse proxy), and sending HSTS over
		// plaintext would be wrong. Operators terminating TLS at the edge
		// should set Strict-Transport-Security there.
		next.ServeHTTP(w, r)
	})
}
