package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
)

func TestAdminSession_LoginCookieAndCSRF(t *testing.T) {
	primary := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"admin-key": {Name: "Admin", Permissions: []string{"admin"}},
	})
	store := auth.NewAdminSessionStore(0)
	handlers := &auth.AdminSessionHandlers{
		Store:    store,
		Provider: primary,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin-api/session", handlers.Create)
	mux.HandleFunc("GET /admin-api/session", handlers.Status)
	mux.HandleFunc("DELETE /admin-api/session", handlers.Delete)
	mux.HandleFunc("GET /admin-api/overview", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("POST /admin-api/runs/r1/cancel", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"cancelled":true}`))
	})

	handler := auth.MiddlewareWithOpts(primary, nil, nil, auth.MiddlewareOpts{AdminSessions: store}, mux)

	// Login
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin-api/session", strings.NewReader(`{"credential":"admin-key"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d body %s", rec.Code, rec.Body.String())
	}
	var loginBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	csrf, _ := loginBody["csrf_token"].(string)
	if csrf == "" {
		t.Fatal("expected csrf_token in login response")
	}
	cookie := sessionCookie(rec)
	if cookie == "" {
		t.Fatal("expected Set-Cookie session")
	}

	// GET with cookie, no Bearer
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/admin-api/overview", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieAdminSession, Value: cookie})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie GET overview: %d %s", rec.Code, rec.Body.String())
	}

	// Mutating without CSRF → 403
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/admin-api/runs/r1/cancel", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieAdminSession, Value: cookie})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected CSRF 403, got %d", rec.Code)
	}

	// Mutating with CSRF → 200
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/admin-api/runs/r1/cancel", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieAdminSession, Value: cookie})
	req.Header.Set(auth.HeaderCSRF, csrf)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("CSRF POST: %d %s", rec.Code, rec.Body.String())
	}

	// Bearer still works without cookie/CSRF (machine API)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/admin-api/runs/r1/cancel", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Bearer POST: %d", rec.Code)
	}

	// Logout
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("DELETE", "/admin-api/session", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieAdminSession, Value: cookie})
	req.Header.Set(auth.HeaderCSRF, csrf)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rec.Code)
	}

	// Cookie no longer valid
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/admin-api/overview", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieAdminSession, Value: cookie})
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 after logout, got %d", rec.Code)
	}
}

func TestAdminSession_StatusOpenDeployment(t *testing.T) {
	store := auth.NewAdminSessionStore(0)
	handlers := &auth.AdminSessionHandlers{Store: store}
	rec := httptest.NewRecorder()
	handlers.Status(rec, httptest.NewRequest("GET", "/admin-api/session", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["authenticated"] != true || body["auth_required"] != false {
		t.Fatalf("open deployment status: %+v", body)
	}
}

func TestAdminSession_RejectsNonAdmin(t *testing.T) {
	primary := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"reader": {Name: "Reader", Permissions: []string{"read"}},
	})
	handlers := &auth.AdminSessionHandlers{
		Store:    auth.NewAdminSessionStore(0),
		Provider: primary,
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin-api/session", strings.NewReader(`{"credential":"reader"}`))
	req.Header.Set("Content-Type", "application/json")
	handlers.Create(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin login, got %d %s", rec.Code, rec.Body.String())
	}
}

func sessionCookie(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieAdminSession {
			return c.Value
		}
	}
	return ""
}
