package cors_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sharanharsoor/runkite/internal/cors"
)

func TestMiddleware_DisabledIsNoop(t *testing.T) {
	called := false
	handler := cors.Middleware(&cors.Config{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	handler.ServeHTTP(rec, req)

	if !called {
		t.Fatal("expected the wrapped handler to run when CORS is disabled")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("expected no CORS headers when disabled, got %q", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestMiddleware_NilConfigIsNoop(t *testing.T) {
	called := false
	handler := cors.Middleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/threads", nil))
	if !called {
		t.Fatal("expected a nil Config to behave as disabled, not panic or block")
	}
}

func TestMiddleware_AllowedOriginGetsHeader(t *testing.T) {
	handler := cors.Middleware(&cors.Config{AllowOrigins: []string{"http://localhost:5173"}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("expected Access-Control-Allow-Origin echoed back, got %q", got)
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("expected Allow-Credentials=true so cookie/Authorization-bearing requests work")
	}
}

func TestMiddleware_DisallowedOriginGetsNoHeader(t *testing.T) {
	handler := cors.Middleware(&cors.Config{AllowOrigins: []string{"http://localhost:5173"}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("expected no CORS header for a disallowed origin, got %q", got)
	}
}

func TestMiddleware_WildcardAllowsAnyOrigin(t *testing.T) {
	handler := cors.Middleware(&cors.Config{AllowOrigins: []string{"*"}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads", nil)
	req.Header.Set("Origin", "http://anything.example.com")
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://anything.example.com" {
		t.Errorf("expected wildcard config to echo back any origin, got %q", got)
	}
}

// TestMiddleware_WildcardNeverSendsCredentials closes a real footgun an
// external audit found: this middleware used to reflect the request's
// Origin header verbatim AND unconditionally set
// Access-Control-Allow-Credentials whenever "*" matched -- worse than a
// literal "Access-Control-Allow-Origin: *" (which browsers already
// refuse to honor together with credentials), since reflecting the
// origin instead of the literal "*" string bypasses that browser-side
// protection entirely. A "*" config still means "any origin may read
// this API" (fine for a public, cookie-free deployment) -- it must
// never also mean "and every one of them may do so with credentials."
func TestMiddleware_WildcardNeverSendsCredentials(t *testing.T) {
	handler := cors.Middleware(&cors.Config{AllowOrigins: []string{"*"}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads", nil)
	req.Header.Set("Origin", "http://evil.example.com")
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("expected no Allow-Credentials for a wildcard-matched origin, got %q -- this is the exact CORS footgun (any site + credentials)", got)
	}
}

// TestMiddleware_ExplicitOriginAlongsideWildcardStillGetsCredentials
// proves the fix doesn't regress the common "public read + credentialed
// admin origin" config: an operator can list "*" for broad reachability
// and still name specific origins that are trusted with credentials.
func TestMiddleware_ExplicitOriginAlongsideWildcardStillGetsCredentials(t *testing.T) {
	handler := cors.Middleware(&cors.Config{AllowOrigins: []string{"*", "http://localhost:5173"}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("expected Allow-Credentials=true for an explicitly-listed origin even when \"*\" is also configured, got %q", got)
	}
}

// TestMiddleware_PreflightAnsweredWithoutReachingHandler proves an
// OPTIONS preflight (which never carries an Authorization header by
// design) is answered directly by this middleware -- if it fell through
// to the wrapped handler (in production, auth.Middleware), it would be
// rejected as unauthenticated and the browser's real request would never
// be attempted at all.
func TestMiddleware_PreflightAnsweredWithoutReachingHandler(t *testing.T) {
	called := false
	handler := cors.Middleware(&cors.Config{AllowOrigins: []string{"http://localhost:5173"}},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.WriteHeader(200)
		}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("OPTIONS", "/threads", nil)
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("expected the preflight to be answered directly, never reaching the wrapped handler/auth")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for a preflight, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("expected Access-Control-Allow-Methods to be set on the preflight response")
	}
}

func TestParseAllowOrigins_TrimsWhitespace(t *testing.T) {
	got := cors.ParseAllowOrigins([]string{" http://localhost:5173 ", "", "  ", "http://foo.com"})
	if len(got) != 2 || got[0] != "http://localhost:5173" || got[1] != "http://foo.com" {
		t.Fatalf("expected trimmed, non-empty origins only, got %+v", got)
	}
}
