package auth_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestMiddleware_InternalPathHonorsTenantHeader(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = tenant.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := auth.Middleware(nil, nil, nil, next)

	req := httptest.NewRequest("GET", "/internal/store/items", nil)
	req.Header.Set(auth.HeaderTenantID, "acme")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got != "acme" {
		t.Fatalf("tenant = %q, want acme", got)
	}
}

func TestMiddleware_InternalPathDefaultsTenantWithoutHeader(t *testing.T) {
	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = tenant.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := auth.Middleware(nil, nil, nil, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/store/items", nil))
	if got != tenant.DefaultTenant {
		t.Fatalf("tenant = %q, want %q", got, tenant.DefaultTenant)
	}
}

func TestMiddleware_InternalPathTenantAllowList(t *testing.T) {
	withEnv(t, map[string]string{
		"RUNNER_TOKEN_PYTHON_LANGGRAPH":   "secret",
		"RUNNER_TENANTS_PYTHON_LANGGRAPH": "acme",
	})
	rt := auth.LoadRunnerTokensFromEnv()

	var got string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = tenant.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := auth.Middleware(nil, nil, rt, next)

	t.Run("allowed", func(t *testing.T) {
		got = ""
		req := httptest.NewRequest("GET", "/internal/store/items", nil)
		req.Header.Set(auth.HeaderRunnerKind, "python-langgraph")
		req.Header.Set(auth.HeaderRunnerToken, "secret")
		req.Header.Set(auth.HeaderTenantID, "acme")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
		}
		if got != "acme" {
			t.Fatalf("tenant = %q, want acme", got)
		}
	})

	t.Run("denied", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/internal/store/items", nil)
		req.Header.Set(auth.HeaderRunnerKind, "python-langgraph")
		req.Header.Set(auth.HeaderRunnerToken, "secret")
		req.Header.Set(auth.HeaderTenantID, "evil")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", rec.Code)
		}
	})

	t.Run("missing_header_denied_when_default_not_listed", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/internal/store/items", nil)
		req.Header.Set(auth.HeaderRunnerKind, "python-langgraph")
		req.Header.Set(auth.HeaderRunnerToken, "secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status %d, want 403", rec.Code)
		}
	})
}
