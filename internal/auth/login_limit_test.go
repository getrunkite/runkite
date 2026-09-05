package auth

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminSession_LoginRateLimited(t *testing.T) {
	primary := NewAPIKeyProvider(map[string]APIKeyEntry{
		"admin-key": {Name: "Admin", Permissions: []string{"admin"}},
	})
	h := &AdminSessionHandlers{
		Store:      NewAdminSessionStore(0),
		Provider:   primary,
		loginLimit: newLoginLimiter(),
	}

	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin-api/session", strings.NewReader(`{"credential":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "203.0.113.9:54321"
		h.Create(rec, req)
		return rec
	}

	for i := 0; i < adminLoginBurst; i++ {
		rec := post()
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401, got %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
	rec := post()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 after burst, got %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After on 429")
	}
}

func TestAdminSession_LoginRateLimitIgnoresSpoofedXFF(t *testing.T) {
	h := &AdminSessionHandlers{
		Store:      NewAdminSessionStore(0),
		Provider:   NewAPIKeyProvider(map[string]APIKeyEntry{"admin-key": {Name: "Admin", Permissions: []string{"admin"}}}),
		loginLimit: newLoginLimiter(),
	}
	// Same real attacker connection (RemoteAddr) on every request; only
	// the client-controlled X-Forwarded-For header changes. If the
	// limiter trusted that header, this would never trip the 429.
	blocked := false
	for i := 0; i < adminLoginBurst+3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin-api/session", strings.NewReader(`{"credential":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i))
		req.RemoteAddr = "203.0.113.9:54321"
		h.Create(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			blocked = true
			break
		}
	}
	if !blocked {
		t.Fatal("expected the limiter to trip on RemoteAddr regardless of spoofed X-Forwarded-For")
	}
}

func TestAdminSession_LoginNotLimitedWhenAuthOff(t *testing.T) {
	h := &AdminSessionHandlers{
		Store:      NewAdminSessionStore(0),
		loginLimit: newLoginLimiter(),
	}
	for i := 0; i < adminLoginBurst+3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin-api/session", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		h.Create(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("open login %d: want 200, got %d %s", i+1, rec.Code, rec.Body.String())
		}
	}
}
