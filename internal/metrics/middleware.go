package metrics

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// uuidRe matches standard UUIDs and short hex IDs in URL path segments.
var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// knownPrefixes lists path prefixes whose next segment is a dynamic ID.
// Order doesn't matter — we match on the segment before the ID.
var knownPrefixes = map[string]bool{
	"threads":    true,
	"runs":       true,
	"agents":     true,
	"assistants": true,
	"connectors": true,
}

// normalizePath replaces dynamic path segments (UUIDs and resource IDs) with
// {id} to prevent label cardinality explosion.
func normalizePath(raw string) string {
	// Fast path: replace any UUIDs first
	path := uuidRe.ReplaceAllString(raw, "{id}")
	if !strings.Contains(path, "{id}") {
		// No UUIDs found — check for non-UUID dynamic segments
		// (e.g. /agents/my_custom_agent)
		segments := strings.Split(strings.Trim(path, "/"), "/")
		for i := 0; i < len(segments)-1; i++ {
			if knownPrefixes[segments[i]] {
				segments[i+1] = "{id}"
			}
		}
		path = "/" + strings.Join(segments, "/")
	}
	return path
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.written {
		rw.statusCode = code
		rw.written = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.written {
		rw.statusCode = 200
		rw.written = true
	}
	return rw.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying ResponseWriter
// (required for Flusher, Hijacker, etc. via Go 1.20+ unwrap convention).
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

// Flush makes responseWriter satisfy http.Flusher directly. Embedding
// http.ResponseWriter as an interface field does NOT promote Flush() --
// that method isn't part of the http.ResponseWriter interface, even though
// the concrete value underneath almost always implements it. Every SSE
// handler in internal/api does a raw `w.(http.Flusher)` type assertion
// (not http.NewResponseController), so without this explicit method every
// streaming endpoint silently breaks the instant this middleware wraps it:
// the assertion fails, the handler returns before writing any event, and
// the client sees a 200 response with correct SSE headers and an empty
// body forever. Unwrap() alone (the "correct" Go 1.20+ answer) does not
// help callers that don't use http.ResponseController.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// HTTPMiddleware records request count and duration for every HTTP request
// except the /metrics endpoint itself.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: 200}
		next.ServeHTTP(rw, r)
		duration := time.Since(start).Seconds()

		path := normalizePath(r.URL.Path)
		HTTPRequestsTotal.WithLabelValues(r.Method, path, strconv.Itoa(rw.statusCode)).Inc()
		HTTPRequestDuration.WithLabelValues(r.Method, path).Observe(duration)
	})
}
