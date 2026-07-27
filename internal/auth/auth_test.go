package auth_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/sharanharsoor/runkite/internal/auth"
)

// ============================================================================
// NoneProvider
// ============================================================================

func TestNoneProvider_AlwaysAnonymous(t *testing.T) {
	p := &auth.NoneProvider{}
	r := httptest.NewRequest("GET", "/threads", nil)
	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != "anonymous" {
		t.Fatalf("expected identity=anonymous, got %s", result.Identity)
	}
}

// ============================================================================
// APIKeyProvider
// ============================================================================

func newAPIKeyProvider() *auth.APIKeyProvider {
	return auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"key-abc123": {Name: "CI Bot", Permissions: []string{"read", "write"}},
		"key-def456": {Name: "Reader", Permissions: []string{"read"}},
	})
}

func TestAPIKey_ValidBearerToken(t *testing.T) {
	p := newAPIKeyProvider()
	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer key-abc123")

	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != "CI Bot" {
		t.Fatalf("expected identity=CI Bot, got %s", result.Identity)
	}
	if len(result.Permissions) != 2 {
		t.Fatalf("expected 2 permissions, got %v", result.Permissions)
	}
}

func TestAPIKey_ValidXAPIKeyHeader(t *testing.T) {
	p := newAPIKeyProvider()
	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("X-API-Key", "key-def456")

	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != "Reader" {
		t.Fatalf("expected identity=Reader, got %s", result.Identity)
	}
}

func TestAPIKey_MissingKey(t *testing.T) {
	p := newAPIKeyProvider()
	r := httptest.NewRequest("GET", "/threads", nil)

	_, err := p.Authenticate(r)
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestAPIKey_WrongKey(t *testing.T) {
	p := newAPIKeyProvider()
	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer wrong-key")

	_, err := p.Authenticate(r)
	if err == nil {
		t.Fatal("expected error for wrong key")
	}
}

// ============================================================================
// Middleware
// ============================================================================

func TestMiddleware_NilProviderPassesThrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})

	handler := auth.Middleware(nil, nil, nil, next)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/threads", nil))

	if !called {
		t.Fatal("nil provider should pass through")
	}
}

func TestMiddleware_SkipsHealth(t *testing.T) {
	// Use an API key provider so auth would normally block unauthenticated requests
	p := newAPIKeyProvider()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})

	handler := auth.Middleware(p, nil, nil, next)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))

	if !called {
		t.Fatal("/health should skip auth")
	}
}

func TestMiddleware_SkipsInternal(t *testing.T) {
	p := newAPIKeyProvider()
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})

	handler := auth.Middleware(p, nil, nil, next)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/runs/123/status", nil))

	if !called {
		t.Fatal("/internal/* should skip auth")
	}
}

func TestMiddleware_EnforcesOnThreads(t *testing.T) {
	p := newAPIKeyProvider()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	handler := auth.Middleware(p, nil, nil, next)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/threads/abc", nil))

	if rec.Code != 401 {
		t.Fatalf("expected 401 for unauthenticated /threads, got %d", rec.Code)
	}

	// Verify JSON error body
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body["message"] == "" {
		t.Fatal("expected JSON error message")
	}
}

func TestMiddleware_AttachesAuthResultToContext(t *testing.T) {
	p := newAPIKeyProvider()
	var gotResult *auth.AuthResult
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotResult = auth.FromContext(r.Context())
		w.WriteHeader(200)
	})

	handler := auth.Middleware(p, nil, nil, next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads", nil)
	req.Header.Set("Authorization", "Bearer key-abc123")
	handler.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotResult == nil {
		t.Fatal("expected AuthResult in context")
	}
	if gotResult.Identity != "CI Bot" {
		t.Fatalf("expected identity=CI Bot, got %s", gotResult.Identity)
	}
}

func TestMiddleware_Returns401WithJSONBody(t *testing.T) {
	p := newAPIKeyProvider()
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/runs", nil))

	if rec.Code != 401 {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected Content-Type application/json, got %s", ct)
	}
}

// ============================================================================
// Authorization (permissions enforcement)
// ============================================================================

func TestAuthz_ReadOnlyKeyRejectedOnWrite(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"reader-key": {Name: "Reader", Permissions: []string{"read"}},
	})
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/threads", nil)
	req.Header.Set("Authorization", "Bearer reader-key")
	handler.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Fatalf("expected 403 for read-only key doing POST, got %d", rec.Code)
	}
}

func TestAuthz_ReadOnlyKeyAllowedOnGet(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"reader-key": {Name: "Reader", Permissions: []string{"read"}},
	})
	called := false
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads/abc", nil)
	req.Header.Set("Authorization", "Bearer reader-key")
	handler.ServeHTTP(rec, req)

	if !called || rec.Code != 200 {
		t.Fatalf("expected read-only key to succeed on GET, called=%v code=%d", called, rec.Code)
	}
}

func TestAuthz_WriteKeyAllowedOnWrite(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"writer-key": {Name: "Writer", Permissions: []string{"write"}},
	})
	called := false
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/threads/abc", nil)
	req.Header.Set("Authorization", "Bearer writer-key")
	handler.ServeHTTP(rec, req)

	if !called || rec.Code != 200 {
		t.Fatalf("expected write key to succeed on DELETE, called=%v code=%d", called, rec.Code)
	}
}

func TestAuthz_WriteKeyImpliesRead(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"writer-key": {Name: "Writer", Permissions: []string{"write"}},
	})
	called := false
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/threads/abc", nil)
	req.Header.Set("Authorization", "Bearer writer-key")
	handler.ServeHTTP(rec, req)

	if !called || rec.Code != 200 {
		t.Fatalf("expected write key to succeed on GET (write implies read), called=%v code=%d", called, rec.Code)
	}
}

func TestAuthz_AdminPermissionBypassesEverything(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"admin-key": {Name: "Admin", Permissions: []string{"admin"}},
	})
	called := false
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/threads/abc", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	handler.ServeHTTP(rec, req)

	if !called || rec.Code != 200 {
		t.Fatalf("expected admin permission to bypass method check, called=%v code=%d", called, rec.Code)
	}
}

// TestAuthz_EmptyPermissionsIsUnrestricted guards the backward-compat
// contract: authenticating with a key/token that has no explicit
// permissions list configured must keep today's all-access behavior. Only
// an explicitly limited permissions list restricts access. This matters
// because every existing test and every currently-documented example
// config authenticates without necessarily setting permissions -- adding
// enforcement must not silently lock those out.
func TestAuthz_EmptyPermissionsIsUnrestricted(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"no-perms-key": {Name: "NoPerms"}, // Permissions left nil
	})
	called := false
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/threads/abc", nil)
	req.Header.Set("Authorization", "Bearer no-perms-key")
	handler.ServeHTTP(rec, req)

	if !called || rec.Code != 200 {
		t.Fatalf("expected key with no configured permissions to remain unrestricted, called=%v code=%d", called, rec.Code)
	}
}

func TestAuthz_AdminPathRejectsReadPermission(t *testing.T) {
	// The Admin API/UI is a stricter tier: "read" is enough for the
	// regular client-facing GETs, but not for /admin-api/* -- viewing the
	// dashboard is itself an admin action.
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"reader-key": {Name: "Reader", Permissions: []string{"read"}},
	})
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin-api/overview", nil)
	req.Header.Set("Authorization", "Bearer reader-key")
	handler.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Fatalf("expected 403 for a read-only key on /admin-api/*, got %d", rec.Code)
	}
}

func TestAuthz_AdminPathRejectsWritePermission(t *testing.T) {
	// "write" implies "read" everywhere else, but must NOT imply "admin".
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"writer-key": {Name: "Writer", Permissions: []string{"write"}},
	})
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin-api/agents", nil)
	req.Header.Set("Authorization", "Bearer writer-key")
	handler.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Fatalf("expected 403 for a write-only key on /admin-api/*, got %d", rec.Code)
	}
}

func TestAuthz_AdminPathAllowsAdminPermission(t *testing.T) {
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"admin-key": {Name: "Admin", Permissions: []string{"admin"}},
	})
	called := false
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin-api/overview", nil)
	req.Header.Set("Authorization", "Bearer admin-key")
	handler.ServeHTTP(rec, req)

	if !called || rec.Code != 200 {
		t.Fatalf("expected an admin-permission key to reach /admin-api/*, called=%v code=%d", called, rec.Code)
	}
}

func TestAuthz_AdminPathUnrestrictedWithEmptyPermissions(t *testing.T) {
	// Same backward-compatible convention as everywhere else: empty
	// permissions means unrestricted, not "no admin access".
	p := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"no-perms-key": {Name: "NoPerms"},
	})
	called := false
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin-api/overview", nil)
	req.Header.Set("Authorization", "Bearer no-perms-key")
	handler.ServeHTTP(rec, req)

	if !called || rec.Code != 200 {
		t.Fatalf("expected an empty-permissions key to reach /admin-api/*, called=%v code=%d", called, rec.Code)
	}
}

func TestAuthz_NoProviderIsUnaffected(t *testing.T) {
	// No auth configured at all (local dev mode) -- authorization must not
	// kick in regardless of method.
	called := false
	handler := auth.Middleware(nil, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("DELETE", "/threads/abc", nil))

	if !called || rec.Code != 200 {
		t.Fatalf("expected no-auth mode to remain fully unrestricted, called=%v code=%d", called, rec.Code)
	}
}

func TestAuthz_JWTPermissionsEnforced(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksSrv := testJWKS(t, &key.PublicKey)
	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":         "readonly-user",
		"permissions": []string{"read"},
		"exp":         time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/runs", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	handler.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Fatalf("expected 403 for read-only JWT doing POST, got %d", rec.Code)
	}
}

// ============================================================================
// Runner auth on /internal/* HTTP routes
// ============================================================================

// TestMiddleware_BareAdminPathIsPublic is a regression test for a real
// bug found clicking through to the Admin UI with auth configured: the
// bypass only matched paths with a trailing slash ("/admin/..."), so
// typing the URL by hand as "localhost:PORT/admin" (the natural thing to
// type, no trailing slash) got a raw 401 JSON body instead of ever
// reaching the login screen -- the mux's own "/admin/" pattern would
// normally redirect a trailing-slash-less request, but this middleware
// runs before the mux ever sees it.
func TestMiddleware_BareAdminPathIsPublic(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	handler := auth.Middleware(newAPIKeyProvider(), nil, nil, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/admin", nil))
	if !called || rec.Code != 200 {
		t.Fatalf("expected bare /admin (no trailing slash) to be public, called=%v code=%d body=%s", called, rec.Code, rec.Body.String())
	}
}

// TestMiddleware_PublicPathShapes covers the pre-mux path-shape class:
// auth wraps the handler, so trailing-slash / HEAD variants never reach
// TestMiddleware_AdminProviderGrantsAccessIndependentOfPrimary proves the
// real-world motivation for a separate admin credential: the primary
// provider here is a JWT provider that would reject any bearer string
// that isn't a real signed token (an operator's static admin key
// included) -- but a valid admin key still gets in, because it's checked
// via adminProvider before the primary provider ever runs.
func TestMiddleware_AdminProviderGrantsAccessIndependentOfPrimary(t *testing.T) {
	primary := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"user-key": {Name: "normal-user", Permissions: nil},
	})
	adminProvider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"break-glass-admin-key": {Name: "ops", Permissions: []string{"admin"}},
	})

	var gotIdentity string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = auth.FromContext(r.Context()).Identity
		w.WriteHeader(200)
	})
	handler := auth.Middleware(primary, adminProvider, nil, next)

	r := httptest.NewRequest("GET", "/admin-api/overview", nil)
	r.Header.Set("Authorization", "Bearer break-glass-admin-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if rec.Code != 200 {
		t.Fatalf("expected admin key to grant /admin-api/* access, got %d body=%s", rec.Code, rec.Body.String())
	}
	if gotIdentity != "ops" {
		t.Fatalf("expected admin provider's identity attached to context, got %q", gotIdentity)
	}
}

// TestMiddleware_AdminKeysFailClosedWithNoPrimaryProvider proves the fix
// for a real misconfiguration footgun: an operator sets only
// auth.admin_keys with no primary auth.type, expecting /admin-api/* to
// be protected. A missing/invalid admin credential on an admin path now
// fails closed (401) instead of falling through to client-facing
// local-dev "trust everyone" -- configuring a credential must never
// leave the dashboard LESS protected than configuring none. The
// client-facing surface is deliberately untouched by this: admin_keys
// was never meant to affect it, so /threads with no primary provider at
// all stays in ordinary local/dev mode, same as if admin_keys were
// never set.
func TestMiddleware_AdminKeysFailClosedWithNoPrimaryProvider(t *testing.T) {
	adminProvider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"break-glass-admin-key": {Name: "ops", Permissions: []string{"admin"}},
	})
	handler := auth.Middleware(nil, adminProvider, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/admin-api/overview", nil))
	if rec.Code != 401 {
		t.Fatalf("/admin-api/overview: expected 401 with no credential and no primary provider, got %d", rec.Code)
	}

	// The valid admin key still works.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/admin-api/overview", nil)
	req.Header.Set("Authorization", "Bearer break-glass-admin-key")
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/admin-api/overview: expected 200 with a valid admin key, got %d", rec.Code)
	}

	// Client-facing surface is untouched: admin_keys has no bearing on
	// it, so it stays in ordinary local/dev (no primary provider = open).
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/threads", nil))
	if rec.Code != 200 {
		t.Fatalf("/threads: client-facing local-dev mode must be unaffected by admin_keys, got %d", rec.Code)
	}

	// Contrast: the same admin key set WITH a primary provider still
	// falls through to it correctly on an invalid/missing admin
	// credential (TestMiddleware_AdminProviderFallsThroughToPrimary
	// covers the successful-fallthrough case in detail; this just
	// re-confirms the 401 shape here).
	primary := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"user-key": {Name: "alice", Permissions: []string{"admin"}},
	})
	gated := auth.Middleware(primary, adminProvider, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	rec = httptest.NewRecorder()
	gated.ServeHTTP(rec, httptest.NewRequest("GET", "/admin-api/overview", nil))
	if rec.Code != 401 {
		t.Fatalf("expected 401 with a primary provider present and no credential, got %d", rec.Code)
	}
}

// TestMiddleware_AdminProviderFallsThroughToPrimary proves the admin
// credential is additive, not a replacement: a request that doesn't match
// any admin key still gets the pre-existing behavior (primary provider,
// requiring it to carry "admin" itself) instead of an outright reject.
func TestMiddleware_AdminProviderFallsThroughToPrimary(t *testing.T) {
	primary := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"primary-admin-key": {Name: "root", Permissions: []string{"admin"}},
	})
	adminProvider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"break-glass-admin-key": {Name: "ops", Permissions: []string{"admin"}},
	})

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	handler := auth.Middleware(primary, adminProvider, nil, next)

	r := httptest.NewRequest("GET", "/admin-api/overview", nil)
	r.Header.Set("Authorization", "Bearer primary-admin-key") // not an admin-provider key
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if !called || rec.Code != 200 {
		t.Fatalf("expected fallthrough to primary provider's own admin permission, called=%v code=%d", called, rec.Code)
	}
}

// ServeMux redirects and must be public here, while /admin-api must NOT
// inherit the /admin bypass (different trust boundary).
func TestMiddleware_PublicPathShapes(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	handler := auth.Middleware(newAPIKeyProvider(), nil, nil, next)

	public := []struct{ method, path string }{
		{"GET", "/health"},
		{"GET", "/health/"},
		{"HEAD", "/health"},
		{"GET", "/admin"},
		{"GET", "/admin/"},
		{"GET", "/admin/index.html"},
		{"HEAD", "/admin/"},
	}
	for _, tc := range public {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != 200 {
			t.Errorf("%s %s: expected public 200, got %d body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}

	// /admin-api is NOT the SPA shell -- must still require a credential.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/admin-api/overview", nil))
	if rec.Code != 401 {
		t.Fatalf("GET /admin-api/overview must require auth, got %d", rec.Code)
	}
}

func TestMiddleware_BareInternalUsesRunnerAuthNotClientAuth(t *testing.T) {
	// Bare /internal (no trailing slash) must not fall through to the
	// client JWT/API-key check -- same pre-mux shape trap as /admin.
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	handler := auth.Middleware(newAPIKeyProvider(), nil, nil, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/internal", nil))
	if !called || rec.Code != 200 {
		t.Fatalf("expected bare /internal to use runner-auth path (open in local mode), called=%v code=%d body=%s", called, rec.Code, rec.Body.String())
	}
}

func TestMiddleware_InternalRoutesOpenInLocalMode(t *testing.T) {
	// No runner tokens configured (local mode) -- /internal/* stays open,
	// same as before runner auth existed.
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	handler := auth.Middleware(newAPIKeyProvider(), nil, nil, next)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/runs/123/status", nil))
	if !called || rec.Code != 200 {
		t.Fatalf("expected /internal/* open in local mode, called=%v code=%d", called, rec.Code)
	}
}

func TestMiddleware_InternalRoutesRequireRunnerTokenInProductionMode(t *testing.T) {
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "secret-tok"})
	rt := auth.LoadRunnerTokensFromEnv()

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(200)
	})
	handler := auth.Middleware(newAPIKeyProvider(), nil, rt, next)

	// No runner credentials -- must be rejected, even though this is /internal/*.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/internal/connectors/salesforce/session", nil))
	if called {
		t.Fatal("must not reach handler without runner credentials in production mode")
	}
	if rec.Code != 401 {
		t.Fatalf("expected 401 for missing runner credentials, got %d", rec.Code)
	}

	// Wrong token -- rejected.
	called = false
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/internal/connectors/salesforce/session", nil)
	req.Header.Set(auth.HeaderRunnerKind, "python-langgraph")
	req.Header.Set(auth.HeaderRunnerToken, "wrong-token")
	handler.ServeHTTP(rec, req)
	if called || rec.Code != 401 {
		t.Fatalf("expected 401 for wrong runner token, called=%v code=%d", called, rec.Code)
	}

	// Correct token -- allowed through.
	called = false
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/internal/connectors/salesforce/session", nil)
	req.Header.Set(auth.HeaderRunnerKind, "python-langgraph")
	req.Header.Set(auth.HeaderRunnerToken, "secret-tok")
	handler.ServeHTTP(rec, req)
	if !called || rec.Code != 200 {
		t.Fatalf("expected valid runner token to reach handler, called=%v code=%d", called, rec.Code)
	}
}

func TestMiddleware_ClientAuthStillEnforcedWhenRunnerAuthEnabled(t *testing.T) {
	// Enabling runner auth must not accidentally weaken or bypass the
	// separate client-facing auth provider on normal routes.
	withEnv(t, map[string]string{"RUNNER_TOKEN_PYTHON_LANGGRAPH": "secret-tok"})
	rt := auth.LoadRunnerTokensFromEnv()

	handler := auth.Middleware(newAPIKeyProvider(), nil, rt, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/threads/abc", nil))
	if rec.Code != 401 {
		t.Fatalf("client auth should still be enforced on /threads, got %d", rec.Code)
	}
}

// ============================================================================
// WebhookProvider
// ============================================================================

func TestWebhook_AllowResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"allow": true,
			"user": map[string]interface{}{
				"identity":    "alice",
				"permissions": []string{"read", "write"},
			},
		})
	}))
	defer srv.Close()

	p := auth.NewWebhookProvider(auth.WebhookConfig{
		URL:       srv.URL,
		TimeoutMs: 5000,
	})

	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer some-token")
	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != "alice" {
		t.Fatalf("expected identity=alice, got %s", result.Identity)
	}
}

// TestWebhook_ExtraFieldsSurviveIntoAuthResult proves a custom auth
// sidecar's richer identity fields (email, an internal user ID, etc.) --
// beyond the fixed identity/permissions/tenant_id/display_name set --
// aren't silently dropped. These feed the Factory Graph runtime.user
// passthrough (see internal/transport.UserContext).
func TestWebhook_ExtraFieldsSurviveIntoAuthResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"allow": true,
			"user": map[string]interface{}{
				"identity":     "alice",
				"display_name": "Alice Example",
				"permissions":  []string{"read"},
				"tenant_id":    "acme-corp",
				"email":        "alice@example.com",
				"internal_id":  "u-12345",
			},
		})
	}))
	defer srv.Close()

	p := auth.NewWebhookProvider(auth.WebhookConfig{URL: srv.URL, TimeoutMs: 5000})
	r := httptest.NewRequest("GET", "/threads", nil)
	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.DisplayName != "Alice Example" {
		t.Errorf("expected display_name=Alice Example, got %q", result.DisplayName)
	}
	if result.Extra["email"] != "alice@example.com" {
		t.Errorf("expected extra.email=alice@example.com, got %+v", result.Extra)
	}
	if result.Extra["internal_id"] != "u-12345" {
		t.Errorf("expected extra.internal_id=u-12345, got %+v", result.Extra)
	}
	if _, stillPresent := result.Extra["identity"]; stillPresent {
		t.Errorf("identity should be surfaced as a first-class field, not duplicated in Extra, got %+v", result.Extra)
	}
}

func TestWebhook_DenyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"allow":   false,
			"status":  403,
			"message": "Insufficient permissions",
		})
	}))
	defer srv.Close()

	p := auth.NewWebhookProvider(auth.WebhookConfig{URL: srv.URL, TimeoutMs: 5000})

	r := httptest.NewRequest("GET", "/threads", nil)
	_, err := p.Authenticate(r)
	if err == nil {
		t.Fatal("expected error for deny response")
	}
	if _, ok := err.(*auth.ErrForbidden); !ok {
		t.Fatalf("expected ErrForbidden, got %T: %v", err, err)
	}
}

func TestWebhook_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]interface{}{"allow": true})
	}))
	defer srv.Close()

	p := auth.NewWebhookProvider(auth.WebhookConfig{
		URL:       srv.URL,
		TimeoutMs: 50, // 50ms timeout, webhook sleeps 200ms
	})

	r := httptest.NewRequest("GET", "/threads", nil)
	_, err := p.Authenticate(r)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWebhook_CacheHit(t *testing.T) {
	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"allow": true,
			"user":  map[string]interface{}{"identity": "cached-user"},
		})
	}))
	defer srv.Close()

	p := auth.NewWebhookProvider(auth.WebhookConfig{
		URL:             srv.URL,
		TimeoutMs:       5000,
		CacheTTLSeconds: 300,
	})

	// Same credential + method + path must hit the cache. Different
	// paths are separate keys (see TestWebhook_CacheKeyIncludesPath).
	r1 := httptest.NewRequest("GET", "/threads", nil)
	r1.Header.Set("Authorization", "Bearer same-token")
	if _, err := p.Authenticate(r1); err != nil {
		t.Fatal(err)
	}

	r2 := httptest.NewRequest("GET", "/threads", nil)
	r2.Header.Set("Authorization", "Bearer same-token")
	if _, err := p.Authenticate(r2); err != nil {
		t.Fatal(err)
	}

	if c := callCount.Load(); c != 1 {
		t.Fatalf("expected 1 webhook call (cache hit), got %d", c)
	}
}

// TestWebhook_CacheKeySeparatesAPIKeys is the regression for hashing
// only Authorization: two X-API-Key clients with empty Bearer used to
// share one cache entry and inherit each other's identity.
func TestWebhook_CacheKeySeparatesAPIKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		headers, _ := body["headers"].(map[string]interface{})
		key, _ := headers["X-Api-Key"].(string)
		if key == "" {
			key, _ = headers["X-API-Key"].(string)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"allow": true,
			"user":  map[string]interface{}{"identity": "id-for-" + key},
		})
	}))
	defer srv.Close()

	p := auth.NewWebhookProvider(auth.WebhookConfig{
		URL:             srv.URL,
		TimeoutMs:       5000,
		CacheTTLSeconds: 300,
	})

	r1 := httptest.NewRequest("GET", "/threads", nil)
	r1.Header.Set("X-API-Key", "key-a")
	res1, err := p.Authenticate(r1)
	if err != nil {
		t.Fatal(err)
	}

	r2 := httptest.NewRequest("GET", "/threads", nil)
	r2.Header.Set("X-API-Key", "key-b")
	res2, err := p.Authenticate(r2)
	if err != nil {
		t.Fatal(err)
	}

	if res1.Identity == res2.Identity {
		t.Fatalf("expected distinct identities for distinct API keys, both got %q", res1.Identity)
	}
}

// TestWebhook_CacheKeyIncludesPath proves a prior allow for one path
// cannot satisfy a deny on another (the webhook body forwards path).
func TestWebhook_CacheKeyIncludesPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		path, _ := body["path"].(string)
		if path == "/admin-api/overview" {
			json.NewEncoder(w).Encode(map[string]interface{}{"allow": false, "message": "no admin"})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"allow": true,
			"user":  map[string]interface{}{"identity": "ok-user"},
		})
	}))
	defer srv.Close()

	p := auth.NewWebhookProvider(auth.WebhookConfig{
		URL:             srv.URL,
		TimeoutMs:       5000,
		CacheTTLSeconds: 300,
	})

	okReq := httptest.NewRequest("GET", "/threads", nil)
	okReq.Header.Set("Authorization", "Bearer tok")
	if _, err := p.Authenticate(okReq); err != nil {
		t.Fatal(err)
	}

	denyReq := httptest.NewRequest("GET", "/admin-api/overview", nil)
	denyReq.Header.Set("Authorization", "Bearer tok")
	_, err := p.Authenticate(denyReq)
	if err == nil {
		t.Fatal("expected path-specific deny, cache reused prior allow")
	}
}

// ============================================================================
// JWTProvider — test with a real RSA key pair and httptest JWKS server
// ============================================================================

// testJWKS starts an httptest server that serves a JWKS with the given RSA public key.
func testJWKS(t *testing.T, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	// Build a minimal JWKS JSON
	jwks := map[string]interface{}{
		"keys": []map[string]interface{}{
			{
				"kty": "RSA",
				"kid": "test-key-1",
				"use": "sig",
				"alg": "RS256",
				"n":   base64URLEncode(pub.N.Bytes()),
				"e":   base64URLEncode(big.NewInt(int64(pub.E)).Bytes()),
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// base64URLEncode encodes bytes to base64url without padding.
func base64URLEncode(b []byte) string {
	const enc = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	result := make([]byte, 0, (len(b)*4+2)/3)
	for i := 0; i < len(b); i += 3 {
		var val uint32
		remaining := len(b) - i
		switch {
		case remaining >= 3:
			val = uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
			result = append(result, enc[val>>18], enc[(val>>12)&0x3f], enc[(val>>6)&0x3f], enc[val&0x3f])
		case remaining == 2:
			val = uint32(b[i])<<16 | uint32(b[i+1])<<8
			result = append(result, enc[val>>18], enc[(val>>12)&0x3f], enc[(val>>6)&0x3f])
		case remaining == 1:
			val = uint32(b[i]) << 16
			result = append(result, enc[val>>18], enc[(val>>12)&0x3f])
		}
	}
	return string(result)
}

func TestJWT_ValidToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":         "user-123",
		"permissions": []string{"read", "write"},
		"exp":         time.Now().Add(time.Hour).Unix(),
		"iat":         time.Now().Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != "user-123" {
		t.Fatalf("expected identity=user-123, got %s", result.Identity)
	}
	if len(result.Permissions) != 2 {
		t.Fatalf("expected 2 permissions, got %v", result.Permissions)
	}
}

// TestJWT_ExtraClaimsAndTokenForwarding proves the Factory Graph
// runtime.user gap found integrating a real production agent: tool code
// needing more than identity/permissions/tenant_id (e.g. an email claim,
// or the raw bearer token itself for downstream RFC 8693 token exchange)
// can get it via extra_claims/forward_token, without runkite needing to
// know that application's specific claim names in advance.
func TestJWT_ExtraClaimsAndTokenForwarding(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{
		JWKSURL:       jwksSrv.URL,
		ExtraClaims:   []string{"email", "orgUserId"},
		ForwardToken:  true,
		RawTokenField: "sso_token",
	})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":       "user-123",
		"name":      "Alice Example",
		"email":     "alice@example.com",
		"orgUserId": "uuid-999",
		"exp":       time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.DisplayName != "Alice Example" {
		t.Errorf("expected display_name from 'name' claim, got %q", result.DisplayName)
	}
	if result.Extra["email"] != "alice@example.com" {
		t.Errorf("expected extra.email populated, got %+v", result.Extra)
	}
	if result.Extra["orgUserId"] != "uuid-999" {
		t.Errorf("expected extra.orgUserId populated, got %+v", result.Extra)
	}
	if result.Extra["sso_token"] != signed {
		t.Errorf("expected the raw bearer token under the configured field name, got %+v", result.Extra["sso_token"])
	}
}

// TestJWT_ClaimAliasesAndForwardHeaders covers the second half of the
// SSO→tools gap: JWT claim names (orgUserId) and request headers
// (X-HRSystem-Token) often don't match the keys existing agent code
// already looks up (org_user_id / hr_system_token). Without rename maps,
// tools fall back to empty identity-derived values even when auth
// succeeded and ExtraClaims were configured.
func TestJWT_ClaimAliasesAndForwardHeaders(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{
		JWKSURL:     jwksSrv.URL,
		ExtraClaims: []string{"orgUserId", "email"},
		ClaimAliases: map[string]string{
			"orgUserId": "org_user_id",
		},
		ForwardHeaders: map[string]string{
			"X-HRSystem-Token": "hr_system_token",
			"X-Platform":       "platform",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":                "user-123",
		"preferred_username": "alice",
		"email":              "alice@example.com",
		"orgUserId":          "uuid-999",
		"exp":                time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	r.Header.Set("X-HRSystem-Token", "hr-system-abc")
	r.Header.Set("X-Platform", "web")

	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.DisplayName != "alice" {
		t.Errorf("expected display_name from preferred_username fallback, got %q", result.DisplayName)
	}
	if result.Extra["org_user_id"] != "uuid-999" {
		t.Errorf("expected aliased org_user_id, got %+v", result.Extra)
	}
	if _, keepJWTName := result.Extra["orgUserId"]; keepJWTName {
		t.Errorf("aliased claim should not also keep JWT name, got %+v", result.Extra)
	}
	if result.Extra["hr_system_token"] != "hr-system-abc" {
		t.Errorf("expected forwarded hr_system_token, got %+v", result.Extra)
	}
	if result.Extra["platform"] != "web" {
		t.Errorf("expected forwarded platform, got %+v", result.Extra)
	}
	if result.Extra["email"] != "alice@example.com" {
		t.Errorf("expected unaliased email claim, got %+v", result.Extra)
	}
}

// TestJWT_NoExtraClaimsConfiguredMeansNoExtra proves the opt-in default:
// a deployment that never sets extra_claims/forward_token gets exactly
// the pre-existing behavior (Extra is nil), not a new field silently
// carrying token/claim data nobody asked for.
func TestJWT_NoExtraClaimsConfiguredMeansNoExtra(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user-123", "email": "alice@example.com", "exp": time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, _ := token.SignedString(key)

	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if result.Extra != nil {
		t.Errorf("expected Extra to stay nil with no extra_claims/forward_token configured, got %+v", result.Extra)
	}
}

func TestJWT_ExpiredToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "user-123",
		"exp": time.Now().Add(-time.Hour).Unix(), // expired
		"iat": time.Now().Add(-2 * time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	_, err = p.Authenticate(r)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestJWT_MissingToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/threads", nil)
	_, err = p.Authenticate(r)
	if err == nil {
		t.Fatal("expected error for missing token")
	}
}

// TestJWT_ScopeClaim proves scope-as-permissions parsing works when a
// deployment explicitly opts in (ScopeAsPermissions: true) -- e.g. a
// simple OAuth2 client-credentials service that deliberately mints scope
// values matching this app's own permission strings.
func TestJWT_ScopeClaim(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL, ScopeAsPermissions: true})
	if err != nil {
		t.Fatal(err)
	}

	// Use "scope" claim instead of "permissions"
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   "user-456",
		"scope": "read write admin",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/threads", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Permissions) != 3 {
		t.Fatalf("expected 3 permissions from scope claim, got %v", result.Permissions)
	}
}

// TestJWT_ForeignPermissionsClaimIgnored is the same class of bug as
// scope→permissions, for the always-on "permissions" claim: Auth0 (and
// similar) mint API-scoped values like "read:messages", never bare
// "read"/"write". Keeping those as a restrictive allow-list 403s every
// POST from a valid SSO session. Unknown vocabulary must be filtered
// out so Permissions stays empty (= unrestricted) unless the token
// actually carries this app's RBAC strings.
func TestJWT_ForeignPermissionsClaimIgnored(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":         "auth0-user",
		"permissions": []string{"read:messages", "write:orders"},
		"exp":         time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, _ := token.SignedString(key)

	authReq := httptest.NewRequest("POST", "/threads", nil)
	authReq.Header.Set("Authorization", "Bearer "+signed)
	result, err := p.Authenticate(authReq)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Permissions) != 0 {
		t.Fatalf("expected foreign permissions vocabulary ignored, got %v", result.Permissions)
	}

	// End-to-end: foreign-only permissions must not 403 POST.
	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/threads", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected POST allowed when only foreign permissions present, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestJWT_AppPermissionsClaimStillEnforced proves filtering does not
// disable real runkite RBAC when the token carries read/write/admin.
func TestJWT_AppPermissionsClaimStillEnforced(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksSrv := testJWKS(t, &key.PublicKey)
	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":         "readonly-user",
		"permissions": []string{"read", "read:messages"},
		"exp":         time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, _ := token.SignedString(key)

	handler := auth.Middleware(p, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/threads", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("expected 403 for read-only (foreign perms stripped), got %d", rec.Code)
	}
}

// TestJWT_ScopeClaimIgnoredByDefault is the regression for a real bug
// found integrating a production SSO provider (Keycloak): its token's
// "scope" claim held standard OIDC values ("openid profile email"), not
// this app's read/write/admin vocabulary. With the old always-on scope
// fallback, that got parsed into a non-empty (but useless) Permissions
// list, which authorized() then treats as a real restrictive allow-list
// -- silently 403ing every write from every real SSO session, since
// "profile"/"email" never match "write". Left unset (the default),
// permissions must end up empty, which authorized() correctly treats as
// unrestricted -- the right behavior for an IdP that carries no app-level
// RBAC in the token at all.
func TestJWT_ScopeClaimIgnoredByDefault(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwksSrv := testJWKS(t, &key.PublicKey)

	p, err := auth.NewJWTProvider(auth.JWTConfig{JWKSURL: jwksSrv.URL})
	if err != nil {
		t.Fatal(err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   "user-456",
		"scope": "openid profile email",
		"exp":   time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key-1"
	signed, _ := token.SignedString(key)

	r := httptest.NewRequest("POST", "/threads", nil)
	r.Header.Set("Authorization", "Bearer "+signed)

	result, err := p.Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Permissions) != 0 {
		t.Fatalf("expected OIDC scope values to be ignored by default, got permissions=%v", result.Permissions)
	}
}

// ============================================================================
// Context helpers
// ============================================================================

func TestContext_RoundTrip(t *testing.T) {
	result := &auth.AuthResult{Identity: "test", Permissions: []string{"read"}}
	ctx := auth.WithContext(httptest.NewRequest("GET", "/", nil).Context(), result)
	got := auth.FromContext(ctx)
	if got == nil || got.Identity != "test" {
		t.Fatal("context round-trip failed")
	}
}

func TestContext_MissingReturnsNil(t *testing.T) {
	got := auth.FromContext(httptest.NewRequest("GET", "/", nil).Context())
	if got != nil {
		t.Fatal("expected nil from context without auth")
	}
}
