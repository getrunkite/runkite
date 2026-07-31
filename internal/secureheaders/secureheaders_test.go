package secureheaders

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMiddleware_SetsExpectedHeaders(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	h := Middleware(next)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != `{"ok":true}` {
		t.Fatalf("body clobbered: %q", body)
	}

	want := map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "strict-origin-when-cross-origin",
		"Content-Security-Policy": contentSecurityPolicy,
		"Permissions-Policy":      "geolocation=(), microphone=(), camera=()",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be set by this process (TLS often terminates at the edge)")
	}
}

func TestMiddleware_AppliesToAdminPath(t *testing.T) {
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("expected CSP on /admin/")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("expected X-Frame-Options on /admin/")
	}
}
