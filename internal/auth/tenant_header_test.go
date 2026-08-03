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
