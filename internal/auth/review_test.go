package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/tenant"
)

func reviewProvider() Provider {
	return NewAPIKeyProvider(map[string]APIKeyEntry{
		"reader":        {Name: "reader", TenantID: "tenant-a", Permissions: []string{GovernanceReadPermission}},
		"approver":      {Name: "approver", TenantID: "tenant-a", Permissions: []string{GovernanceApprovePermission}},
		"other":         {Name: "other", TenantID: "tenant-b", Permissions: []string{GovernanceApprovePermission}},
		"primary-admin": {Name: "operator", TenantID: "tenant-a", Permissions: []string{"admin"}, Extra: map[string]interface{}{"fixture": "must-not-be-retained"}},
		"unscoped":      {Name: "unscoped", Permissions: []string{GovernanceApprovePermission}},
		"empty":         {Name: "empty", TenantID: "tenant-a"},
		"no-identity":   {TenantID: "tenant-a", Permissions: []string{GovernanceApprovePermission}},
	})
}

func TestReviewCapabilitiesRequireExplicitVerifiedScopeAndRole(t *testing.T) {
	for _, tc := range []struct {
		name, identity, tenant string
		permissions            []string
		read, approve          bool
	}{
		{"reader", "alice", "a", []string{GovernanceReadPermission}, true, false},
		{"approver", "alice", "a", []string{GovernanceApprovePermission}, true, true},
		{"admin", "alice", "a", []string{"admin"}, true, true},
		{"empty", "alice", "a", nil, false, false},
		{"write", "alice", "a", []string{"write"}, false, false},
		{"decide", "alice", "a", []string{GovernanceDecidePermission}, false, false},
		{"missing-tenant", "alice", "", []string{GovernanceApprovePermission}, false, false},
		{"missing-identity", "", "a", []string{GovernanceApprovePermission}, false, false},
		{"blank-tenant", "alice", " ", []string{GovernanceApprovePermission}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps, ok := ReviewCapabilitiesFromResult(&AuthResult{Identity: tc.identity, TenantID: tc.tenant, Permissions: tc.permissions})
			if ok != tc.read || caps.CanRead != tc.read || caps.CanApprove != tc.approve {
				t.Fatalf("unexpected capability: %+v %v", caps, ok)
			}
		})
	}
	if caps, ok := ReviewCapabilitiesFromResult(nil); ok || caps.CanRead {
		t.Fatal("nil identity granted review")
	}
	perms := parsePermissionsClaim([]any{"openid", GovernanceReadPermission, GovernanceApprovePermission, "read:messages"})
	if len(perms) != 2 || perms[0] != GovernanceReadPermission || perms[1] != GovernanceApprovePermission {
		t.Fatalf("JWT roles were not retained exactly: %#v", perms)
	}
}

func TestReviewMiddlewareBearerAllowlistAndScope(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, key string
		noProvider              bool
		status                  int
	}{
		{"read", "GET", "/review-api/pending-actions", "reader", false, 200},
		{"head", "HEAD", "/review-api/connector-actions/action", "reader", false, 200},
		{"credential-list", "GET", "/review-api/credential-leases", "reader", false, 200},
		{"credential-head", "HEAD", "/review-api/credential-leases/opaque-ref", "reader", false, 200},
		{"credential-reader-renew", "POST", "/review-api/credential-leases/opaque-ref/renew", "reader", false, 403},
		{"credential-approver-revoke", "POST", "/review-api/credential-leases/opaque-ref/revoke", "approver", false, 403},
		{"credential-admin-revoke", "POST", "/review-api/credential-leases/opaque-ref/revoke", "primary-admin", false, 403},
		{"credential-delete", "DELETE", "/review-api/credential-leases/opaque-ref", "primary-admin", false, 403},
		{"credential-private-path", "GET", "/review-api/credential-leases/opaque-ref/handle", "primary-admin", false, 403},
		{"credential-unscoped", "GET", "/review-api/credential-leases", "unscoped", false, 403},
		{"approve", "POST", "/review-api/pending-actions/id/approve", "approver", false, 200},
		{"deny", "POST", "/review-api/pending-actions/id/deny", "approver", false, 200},
		{"reader-write", "POST", "/review-api/pending-actions/id/approve", "reader", false, 403},
		{"empty", "GET", "/review-api/pending-actions", "empty", false, 403},
		{"missing-scope", "GET", "/review-api/pending-actions", "unscoped", false, 403},
		{"missing-identity", "GET", "/review-api/pending-actions", "no-identity", false, 403},
		{"admin-explicit-scope", "GET", "/review-api/connector-actions", "primary-admin", false, 200},
		{"unknown", "GET", "/review-api/anything", "primary-admin", false, 403},
		{"unknown-mutation", "DELETE", "/review-api/pending-actions/id", "primary-admin", false, 403},
		{"audit-not-exposed", "GET", "/review-api/audit", "primary-admin", false, 403},
		{"missing-auth", "GET", "/review-api/pending-actions", "", false, 401},
		{"no-provider", "GET", "/review-api/pending-actions", "reader", true, 401},
		{"admin-key-not-review", "GET", "/review-api/pending-actions", "admin-only", false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := reviewProvider()
			if tc.noProvider {
				primary = nil
			}
			admin := NewAPIKeyProvider(map[string]APIKeyEntry{"admin-only": {Name: "admin", TenantID: "tenant-a", Permissions: []string{"admin"}}})
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tenant.IsSystem(r.Context()) || tenant.FromContext(r.Context()) != "tenant-a" {
					t.Fatalf("scope not attenuated: %q system=%v", tenant.FromContext(r.Context()), tenant.IsSystem(r.Context()))
				}
				result := FromContext(r.Context())
				if result == nil || result.Extra != nil || result.DisplayName != "" {
					t.Fatal("review forwarded identity extras")
				}
				for _, p := range result.Permissions {
					if p == "admin" {
						t.Fatal("review context retained global Admin")
					}
				}
				w.WriteHeader(200)
			})
			h := MiddlewareWithOpts(primary, admin, nil, MiddlewareOpts{}, next)
			r := httptest.NewRequest(tc.method, tc.path+"?tenant_id=attacker", nil).WithContext(tenant.SystemContext(context.Background()))
			r.Header.Set(HeaderTenantID, "attacker")
			if tc.key != "" {
				r.Header.Set("Authorization", "Bearer "+tc.key)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("review response may be cached")
			}
		})
	}
}

func TestReviewSessionLoginCSRFHeaderPrecedenceAndLogout(t *testing.T) {
	provider := reviewProvider()
	review := NewReviewSessionStore(time.Hour)
	admin := NewAdminSessionStore(time.Hour)
	handlers := &ReviewSessionHandlers{Store: review, Provider: provider, loginLimit: newLoginLimiter()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /review-api/session", handlers.Create)
	mux.HandleFunc("GET /review-api/session", handlers.Status)
	mux.HandleFunc("DELETE /review-api/session", handlers.Delete)
	mux.HandleFunc("/review-api/pending-actions", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/review-api/pending-actions/id/approve", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/admin-api/overview", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := MiddlewareWithOpts(provider, nil, nil, MiddlewareOpts{AdminSessions: admin, ReviewSessions: review}, mux)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/review-api/session", strings.NewReader(`{"credential":"reader"}`)))
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	var status reviewSessionStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if !status.Authenticated || !status.Configured || !status.AuthRequired || !status.CanRead || status.CanApprove || status.TenantID != "tenant-a" || status.ExpiresAt == nil || status.CSRFToken == "" {
		t.Fatalf("unsafe status: %+v", status)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == CookieReviewSession {
			cookie = c
		}
	}
	if cookie == nil || cookie.Path != "/review-api" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("unsafe cookie: %+v", cookie)
	}
	request := func(method, path, bearer, csrf, cookieName string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		if cookieName != "" {
			r.AddCookie(&http.Cookie{Name: cookieName, Value: cookie.Value})
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		if csrf != "" {
			r.Header.Set(HeaderCSRF, csrf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/review-api/pending-actions", "", "", CookieReviewSession); w.Code != 200 {
		t.Fatalf("cookie read: %d", w.Code)
	}
	if w := request("POST", "/review-api/pending-actions/id/approve", "", status.CSRFToken, CookieReviewSession); w.Code != 403 {
		t.Fatalf("reader escalation: %d", w.Code)
	}
	if w := request("GET", "/review-api/pending-actions", "invalid", "", CookieReviewSession); w.Code != 401 {
		t.Fatalf("invalid header fell back to cookie: %d", w.Code)
	}
	if w := request("POST", "/review-api/pending-actions/id/approve", "approver", "", CookieReviewSession); w.Code != 200 {
		t.Fatalf("valid machine header: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", "/admin-api/overview", "", "", CookieAdminSession); w.Code != 401 {
		t.Fatalf("review cookie entered Admin: %d", w.Code)
	}
	if w := request("DELETE", "/review-api/session", "", "", CookieReviewSession); w.Code != 403 {
		t.Fatalf("logout without CSRF: %d", w.Code)
	}
	if w := request("DELETE", "/review-api/session", "approver", "", CookieReviewSession); w.Code != 400 || w.Header().Get("Set-Cookie") != "" || review.Get(cookie.Value) == nil {
		t.Fatalf("mixed auth falsely confirmed browser logout: %d %s", w.Code, w.Body.String())
	}
	if w := request("DELETE", "/review-api/session", "", status.CSRFToken, CookieReviewSession); w.Code != 204 {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	if w := request("GET", "/review-api/pending-actions", "", "", CookieReviewSession); w.Code != 401 {
		t.Fatalf("revoked cookie accepted: %d", w.Code)
	}
}

func TestReviewSessionPurposeIsolationAndPrimaryRemoval(t *testing.T) {
	primary := reviewProvider()
	review := NewReviewSessionStore(0)
	admin := NewAdminSessionStore(0)
	adminSession, err := admin.Create(&AuthResult{Identity: "operator", TenantID: "tenant-a", Permissions: []string{"admin"}})
	if err != nil {
		t.Fatal(err)
	}
	reviewSession, err := review.CreateReview(&AuthResult{Identity: "operator", TenantID: "tenant-a", Permissions: []string{"admin"}, DisplayName: "not-retained", Extra: map[string]interface{}{"fixture": "not-retained"}})
	if err != nil {
		t.Fatal(err)
	}
	if reviewSession.Result.Extra != nil || reviewSession.Result.DisplayName != "" || len(reviewSession.Result.Permissions) != 2 || reviewSession.Purpose != SessionPurposeReview {
		t.Fatalf("session not attenuated: %+v", reviewSession)
	}
	if _, err := admin.CreateReview(&reviewSession.Result); err == nil {
		t.Fatal("Admin store created a review credential")
	}
	if _, err := review.Create(&adminSession.Result); err == nil {
		t.Fatal("review store created an Admin credential")
	}
	if admin.Get(reviewSession.ID) != nil || review.Get(adminSession.ID) != nil {
		t.Fatal("memory namespaces overlap")
	}
	for _, tc := range []struct {
		name, path, cookie, id string
		primary                Provider
		opts                   MiddlewareOpts
	}{
		{"admin-cookie-as-review", "/review-api/pending-actions", CookieReviewSession, adminSession.ID, primary, MiddlewareOpts{ReviewSessions: admin}},
		{"review-cookie-as-admin", "/admin-api/overview", CookieAdminSession, reviewSession.ID, primary, MiddlewareOpts{AdminSessions: review}},
		{"provider-removed", "/review-api/pending-actions", CookieReviewSession, reviewSession.ID, nil, MiddlewareOpts{ReviewSessions: review}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := MiddlewareWithOpts(tc.primary, nil, nil, tc.opts, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Fatal("purpose or provider gate bypassed") }))
			r := httptest.NewRequest("GET", tc.path, nil)
			r.AddCookie(&http.Cookie{Name: tc.cookie, Value: tc.id})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 401 {
				t.Fatalf("expected 401: %d %s", w.Code, w.Body.String())
			}
		})
	}
	for _, store := range []SessionStore{review, nil} {
		h := &ReviewSessionHandlers{Store: store}
		r := httptest.NewRequest("GET", "/review-api/session", nil)
		r.AddCookie(&http.Cookie{Name: CookieReviewSession, Value: reviewSession.ID})
		w := httptest.NewRecorder()
		h.Status(w, r)
		var status reviewSessionStatus
		_ = json.Unmarshal(w.Body.Bytes(), &status)
		if status.Authenticated || status.Configured || !status.AuthRequired {
			t.Fatalf("provider removal status: %+v", status)
		}
	}
}

func TestAdminCookieRechecksAuthorityAndAllowsLegacyAdmin(t *testing.T) {
	for _, tc := range []struct {
		name, purpose string
		permissions   []string
		status        int
	}{
		{"legacy-admin", "", []string{"admin"}, 200},
		{"reader", SessionPurposeAdmin, []string{"read"}, 401},
		{"review-purpose", SessionPurposeReview, []string{"admin"}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := NewAdminSessionStore(0)
			sess, err := store.Create(&AuthResult{Identity: "operator", Permissions: tc.permissions})
			if err != nil {
				t.Fatal(err)
			}
			store.sessions[sess.ID].Purpose = tc.purpose
			h := MiddlewareWithOpts(reviewProvider(), nil, nil, MiddlewareOpts{AdminSessions: store}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
			r := httptest.NewRequest("GET", "/admin-api/overview", nil)
			r.AddCookie(&http.Cookie{Name: CookieAdminSession, Value: sess.ID})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestReviewMemoryAbsoluteExpiryDoesNotSlide(t *testing.T) {
	for _, ttl := range []time.Duration{2 * time.Minute, time.Hour} {
		t.Run(ttl.String(), func(t *testing.T) {
			store := NewReviewSessionStore(ttl)
			now := time.Now()
			store.now = func() time.Time { return now }
			sess, err := store.CreateReview(&AuthResult{Identity: "alice", TenantID: "a", Permissions: []string{GovernanceApprovePermission}})
			if err != nil {
				t.Fatal(err)
			}
			absolute := now.Add(min(ttl, ReviewSessionTTL))
			if !sess.AbsoluteExpires.Equal(absolute) || !sess.Expires.Equal(absolute) {
				t.Fatalf("unbounded session: %+v", sess)
			}
			for now.Before(absolute) {
				got := store.Get(sess.ID)
				if got == nil || !got.AbsoluteExpires.Equal(absolute) || got.Expires.After(absolute) {
					t.Fatalf("activity changed absolute expiry: %+v", got)
				}
				got.Result.Permissions = []string{"admin"}
				now = now.Add(30 * time.Second)
			}
			if store.Get(sess.ID) != nil {
				t.Fatal("activity extended review past absolute expiry")
			}
		})
	}
}

type unsupportedReviewStore struct{ SessionStore }
type failingReviewLogoutStore struct {
	SessionStore
	fail bool
}

func (s *failingReviewLogoutStore) Delete(id string) error {
	if s.fail {
		return errors.New("fixture unavailable")
	}
	return s.SessionStore.Delete(id)
}

func TestReviewCustomStoreUnsupportedAndLogoutFailure(t *testing.T) {
	store := NewReviewSessionStore(0)
	h := &ReviewSessionHandlers{Store: unsupportedReviewStore{store}, Provider: reviewProvider(), loginLimit: newLoginLimiter()}
	w := httptest.NewRecorder()
	h.Create(w, httptest.NewRequest("POST", "/review-api/session", strings.NewReader(`{"credential":"approver"}`)))
	if w.Code != 501 {
		t.Fatalf("custom store falsely supported review: %d", w.Code)
	}
	sess, err := store.CreateReview(&AuthResult{Identity: "alice", TenantID: "a", Permissions: []string{GovernanceReadPermission}})
	if err != nil {
		t.Fatal(err)
	}
	failing := &failingReviewLogoutStore{SessionStore: store, fail: true}
	h.Store = failing
	logout := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/review-api/session", nil)
		r.AddCookie(&http.Cookie{Name: CookieReviewSession, Value: sess.ID})
		w := httptest.NewRecorder()
		h.Delete(w, r)
		return w
	}
	if w := logout(); w.Code != 503 || w.Header().Get("Set-Cookie") != "" || store.Get(sess.ID) == nil {
		t.Fatalf("logout falsely confirmed: %d", w.Code)
	}
	failing.fail = false
	if w := logout(); w.Code != 204 || store.Get(sess.ID) != nil {
		t.Fatalf("logout retry failed: %d", w.Code)
	}
}

func TestReviewLoginRejectsUnknownTrailingAndOversizedJSON(t *testing.T) {
	for _, body := range []string{
		`{"credential":"approver","tenant_id":"attacker"}`,
		`{"credential":"approver"} {}`,
		`{"credential":"approver"} trailing`,
		`{"credential":"` + strings.Repeat("x", 64<<10) + `"}`,
	} {
		h := &ReviewSessionHandlers{Store: NewReviewSessionStore(0), Provider: reviewProvider(), loginLimit: newLoginLimiter()}
		r := httptest.NewRequest(http.MethodPost, "/review-api/session", strings.NewReader(body))
		w := httptest.NewRecorder()
		h.Create(w, r)
		if w.Code != http.StatusBadRequest || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("malformed login accepted: status=%d body=%s", w.Code, w.Body.String())
		}
	}
}

type reviewWebhookTransport func(*http.Request) (*http.Response, error)

func (f reviewWebhookTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReviewWebhookRequiresExplicitIdentityIncludingCachedResults(t *testing.T) {
	for _, identity := range []string{"", "webhook-user", "alice"} {
		t.Run("identity="+identity, func(t *testing.T) {
			provider := NewWebhookProvider(WebhookConfig{URL: "https://fixture.invalid/auth", CacheTTLSeconds: 60})
			provider.client = &http.Client{Transport: reviewWebhookTransport(func(r *http.Request) (*http.Response, error) {
				user := map[string]any{"tenant_id": "a", "permissions": []string{GovernanceReadPermission}}
				if identity != "" {
					user["identity"] = identity
				}
				raw, err := json.Marshal(map[string]any{"allow": true, "user": user})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})}
			for range 2 {
				r := httptest.NewRequest(http.MethodGet, "/review-api/pending-actions", nil)
				result, err := provider.Authenticate(r)
				if err != nil {
					t.Fatal(err)
				}
				if result.Identity == "" {
					t.Fatal("legacy display placeholder changed")
				}
				_, allowed := ReviewCapabilitiesFromResult(result)
				if allowed != (identity != "") {
					t.Fatalf("placeholder acquired verified identity: %+v", result)
				}
				h := Middleware(provider, nil, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := 200
				if identity == "" {
					want = 403
				}
				if w.Code != want {
					t.Fatalf("review placeholder gate: %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}
