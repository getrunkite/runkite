package customroutes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestNormalizeMount(t *testing.T) {
	got, err := NormalizeMount("")
	if err != nil || got != DefaultMount {
		t.Fatalf("empty → default: got %q err=%v", got, err)
	}
	got, err = NormalizeMount("/sales-assistant/")
	if err != nil || got != "/sales-assistant" {
		t.Fatalf("trim trailing slash: got %q err=%v", got, err)
	}
	if _, err := NormalizeMount("/"); err == nil {
		t.Fatal("root mount should be rejected")
	}
	if _, err := NormalizeMount("/threads"); err == nil {
		t.Fatal("reserved /threads should be rejected")
	}
	if _, err := NormalizeMount("no-slash"); err == nil {
		t.Fatal("missing leading slash should be rejected")
	}
}

func TestProxy_StripsMountAndInjectsIdentity(t *testing.T) {
	var (
		gotPath  string
		gotID    string
		gotTen   string
		gotPerm  string
		gotUser  string
		gotSpoof string
	)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotID = r.Header.Get(HeaderIdentity)
		gotTen = r.Header.Get(HeaderTenantID)
		gotPerm = r.Header.Get(HeaderPermissions)
		gotUser = r.Header.Get(HeaderUserJSON)
		gotSpoof = r.Header.Get(HeaderIdentity) // after injection
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	target, _ := url.Parse(backend.URL)
	proxy, err := NewProxy(target, "/sales-assistant")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sales-assistant/v1/profile/me", nil)
	// Client tries to spoof identity — must be overwritten.
	req.Header.Set(HeaderIdentity, "attacker")
	req.Header.Set(HeaderTenantID, "evil-tenant")
	ar := &auth.AuthResult{
		Identity:    "alice",
		Permissions: []string{"read", "write"},
		TenantID:    "acme",
		DisplayName: "Alice",
	}
	ctx := auth.WithContext(req.Context(), ar)
	ctx = tenant.WithContext(ctx, "acme")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if gotPath != "/v1/profile/me" {
		t.Errorf("path: want /v1/profile/me, got %q", gotPath)
	}
	if gotID != "alice" {
		t.Errorf("identity: want alice, got %q (spoof check: %q)", gotID, gotSpoof)
	}
	if gotTen != "acme" {
		t.Errorf("tenant: want acme, got %q", gotTen)
	}
	if gotPerm != "read,write" {
		t.Errorf("permissions: got %q", gotPerm)
	}
	var user map[string]any
	if err := json.Unmarshal([]byte(gotUser), &user); err != nil {
		t.Fatalf("user json: %v (%q)", err, gotUser)
	}
	if user["identity"] != "alice" || user["tenant_id"] != "acme" {
		t.Errorf("user json: %+v", user)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "ok" {
		t.Errorf("body %q", body)
	}
}

func TestProxy_DefaultMount(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
	}))
	defer backend.Close()
	target, _ := url.Parse(backend.URL)
	proxy, err := NewProxy(target, "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/custom/ping", nil)
	req = req.WithContext(auth.WithContext(req.Context(), &auth.AuthResult{Identity: "bob"}))
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if gotPath != "/ping" {
		t.Errorf("want /ping, got %q", gotPath)
	}
}

func TestMatches(t *testing.T) {
	if !Matches("/custom", "/custom") || !Matches("/custom", "/custom/x") {
		t.Fatal("expected match")
	}
	if Matches("/custom", "/customize") {
		t.Fatal("/customize must not match /custom")
	}
}
