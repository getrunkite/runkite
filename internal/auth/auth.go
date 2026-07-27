// Package auth implements authentication middleware for the Agent Protocol API.
// Auth is opt-in: when no provider is configured (type=none), all requests pass through.
package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sharanharsoor/runkite/internal/tenant"
)

// AuthResult is the output of a successful authentication.
type AuthResult struct {
	Identity    string   `json:"identity"`
	Permissions []string `json:"permissions,omitempty"`
	// TenantID scopes this caller's data (master plan: "Multi-tenancy").
	// Empty means "no tenant claim configured" -- resolves to
	// tenant.DefaultTenant, not an error; multi-tenancy is opt-in.
	TenantID string `json:"tenant_id,omitempty"`
	// DisplayName and Extra exist for the Factory Graph runtime.user
	// passthrough (see internal/transport.UserContext / the Python
	// runner's factory_graph.py): JWT (extra_claims/forward_token/
	// forward_headers), API-key entry metadata, and webhook user
	// fields all populate Extra. Never used for authorization
	// decisions in Go -- purely forwarded to the runner.
	DisplayName string                 `json:"display_name,omitempty"`
	Extra       map[string]interface{} `json:"extra,omitempty"`
}

// Provider is the interface all auth providers implement.
type Provider interface {
	Authenticate(r *http.Request) (*AuthResult, error)
}

// ErrUnauthorized is returned when authentication fails.
type ErrUnauthorized struct {
	Message string
}

func (e *ErrUnauthorized) Error() string { return e.Message }

// ErrForbidden is returned when the user is authenticated but not allowed.
type ErrForbidden struct {
	Message string
}

func (e *ErrForbidden) Error() string { return e.Message }

// Header names for runner authentication on /internal/* HTTP routes. Mirrors
// the gRPC metadata keys (runner-kind/runner-token) used by the bridge
// server's interceptors -- same credential, same trust boundary, just
// carried over HTTP instead of gRPC metadata.
const (
	HeaderRunnerKind  = "X-Runner-Kind"
	HeaderRunnerToken = "X-Runner-Token"
)

// Middleware returns an http.Handler that enforces auth before delegating to
// next. client auth (provider), admin auth (adminProvider), and runner auth
// (runnerTokens) are three separate trust boundaries: provider protects the
// client-facing Agent Protocol surface, adminProvider is an optional,
// independent credential accepted ONLY for /admin-api/* (see its own doc
// comment below for why a deployment would configure one), and
// runnerTokens protects /internal/* (connector credentials, run status)
// which client auth always bypasses. Pass nil/disabled for any of the
// three that aren't configured -- adminProvider and a disabled
// runnerTokens are both common (local mode, or a deployment happy to gate
// /admin-api/* via the primary provider's own "admin" permission, the
// pre-existing behavior).
func Middleware(provider Provider, adminProvider Provider, runnerTokens *RunnerTokens, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Public paths must be decided HERE, not by the mux: this
		// middleware wraps the entire API handler, so a trailing-slash
		// mismatch (or bare prefix) never reaches ServeMux's redirect
		// logic -- see isPublicPath / isInternalPath.
		if isPublicPath(r.Method, path) {
			next.ServeHTTP(w, r)
			return
		}

		// Internal routes (runner-facing) use runner auth, not client auth.
		if isInternalPath(path) {
			if runnerTokens.Enabled() {
				kind := r.Header.Get(HeaderRunnerKind)
				token := r.Header.Get(HeaderRunnerToken)
				if kind == "" || token == "" || !runnerTokens.Validate(kind, token) {
					writeUnauthorized(w, "invalid or missing runner credentials")
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}

		// An independent admin credential (master plan: "Admin API +
		// UI"), if configured, is tried first for /admin-api/* only --
		// never for the client-facing surface. This exists because the
		// primary provider is often a real end-user identity system
		// (e.g. SSO) issuing short-lived tokens: fine for a browser
		// session that silently refreshes them, awkward for an
		// operator who just wants to check the dashboard and doesn't
		// want to keep re-pasting a token that expires every few
		// minutes. Falling through (rather than failing) when it's
		// absent or rejects the request preserves the pre-existing
		// path unchanged: authenticate via the primary provider and
		// require it to carry "admin".
		if isAdminAPIPath(path) && adminProvider != nil {
			if result, err := adminProvider.Authenticate(r); err == nil {
				ctx := WithContext(r.Context(), result)
				ctx = tenant.WithContext(ctx, result.TenantID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		if provider == nil {
			next.ServeHTTP(w, r)
			return
		}

		result, err := provider.Authenticate(r)
		if err != nil {
			status := http.StatusUnauthorized
			msg := "unauthorized"

			if e, ok := err.(*ErrForbidden); ok {
				status = http.StatusForbidden
				msg = e.Message
			} else if e, ok := err.(*ErrUnauthorized); ok {
				msg = e.Message
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"message": msg})
			return
		}

		// The Admin API/UI (master plan: "Admin API + UI") is a stricter
		// tier than the rest of the client-facing surface: every method,
		// including GET, requires "admin" specifically -- viewing the
		// dashboard is itself an admin action, not something "read"
		// should imply. Empty permissions still means unrestricted (same
		// backward-compatible convention as everywhere else), so an
		// existing deployment authenticating without configuring
		// fine-grained permissions keeps working unchanged.
		if isAdminAPIPath(path) {
			if len(result.Permissions) > 0 && !hasPermission(result.Permissions, "admin") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{"message": "insufficient permissions (requires 'admin')"})
				return
			}
		} else if !authorized(result, r.Method) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]string{
				"message": "insufficient permissions for " + r.Method + " (requires '" + requiredPermission(r.Method) + "')",
			})
			return
		}

		ctx := WithContext(r.Context(), result)
		ctx = tenant.WithContext(ctx, result.TenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// isPublicPath reports paths that must skip client auth entirely.
// Auth runs *before* the mux, so every form a browser/probe might hit
// (with or without trailing slash; GET or HEAD) has to be listed here --
// otherwise the mux never gets to redirect or serve, and the client
// sees raw 401 JSON (the /admin vs /admin/ bug).
//
// Deliberately does NOT use HasPrefix("/admin") without the slash:
// "/admin-api/..." must stay authenticated (admin permission gate).
func isPublicPath(method, path string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	switch path {
	case "/health", "/health/":
		return true
	case "/admin":
		return true
	}
	return strings.HasPrefix(path, "/admin/")
}

// isInternalPath is the runner-auth surface. Bare "/internal" is included
// so it doesn't fall through to client JWT auth (same pre-mux trap).
func isInternalPath(path string) bool {
	return path == "/internal" || strings.HasPrefix(path, "/internal/")
}

// isAdminAPIPath is the stricter admin-permission gate. Bare "/admin-api"
// included for the same pre-mux reason as /admin and /internal.
func isAdminAPIPath(path string) bool {
	return path == "/admin-api" || strings.HasPrefix(path, "/admin-api/")
}

// requiredPermission maps an HTTP method to the permission string a caller
// must hold to use it: reads need "read", mutations need "write". This is
// deliberately coarse-grained (route-level RBAC, not per-resource ACLs) --
// matches the "read"/"write" permission strings already used throughout the
// auth providers and connector config examples.
func requiredPermission(method string) string {
	if method == http.MethodGet || method == http.MethodHead {
		return "read"
	}
	return "write"
}

// authorized checks whether an authenticated caller may perform the given
// HTTP method.
//
// An EMPTY permissions list means "unrestricted" (backward compatible --
// authenticating without configuring fine-grained permissions keeps
// today's all-access behavior; you only get restricted access by
// explicitly granting a limited list). A NON-EMPTY list is treated as a
// real allow-list: the caller must hold the specific permission the
// request requires.
//
// "write" implies "read" (a writer can GET). "admin" implies everything.
func authorized(result *AuthResult, method string) bool {
	if result == nil || len(result.Permissions) == 0 {
		return true
	}
	return hasPermission(result.Permissions, requiredPermission(method))
}

// hasPermission reports whether permissions grants required. "admin"
// always grants everything; "write" implies "read" (a writer can GET).
func hasPermission(permissions []string, required string) bool {
	for _, p := range permissions {
		if p == "admin" || p == required {
			return true
		}
		if required == "read" && p == "write" {
			return true
		}
	}
	return false
}

// appPermission reports whether p is one of this control plane's own
// RBAC vocabulary strings (read/write/admin). Anything else -- OIDC
// consent scopes, Auth0 API permissions like "read:messages", custom
// realm roles -- is not something authorized() understands, and must
// NOT be kept in AuthResult.Permissions: a non-empty list of unknown
// strings is treated as a restrictive allow-list and silently 403s
// every real request (the Keycloak scope bug; same class for an
// always-on "permissions" claim from Auth0).
func appPermission(p string) bool {
	switch p {
	case "read", "write", "admin":
		return true
	default:
		return false
	}
}

// filterAppPermissions keeps only read/write/admin. Unknown values are
// dropped so IdP-native claim vocabularies collapse to empty (=
// unrestricted), matching the correct behavior for an identity
// provider that isn't doing runkite-level RBAC.
func filterAppPermissions(perms []string) []string {
	if len(perms) == 0 {
		return nil
	}
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		if appPermission(p) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parsePermissionsClaim accepts the shapes real issuers emit for a
// permissions-like claim: JSON array of strings, or a single
// space-separated string. Anything else yields nil (treated as
// "no app RBAC"), never a bogus non-empty list.
func parsePermissionsClaim(raw interface{}) []string {
	switch v := raw.(type) {
	case []interface{}:
		perms := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				perms = append(perms, s)
			}
		}
		return filterAppPermissions(perms)
	case []string:
		return filterAppPermissions(v)
	case string:
		if v == "" {
			return nil
		}
		return filterAppPermissions(strings.Fields(v))
	default:
		return nil
	}
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// --- Context helpers ---

type contextKey struct{}

// FromContext extracts AuthResult from request context.
func FromContext(ctx context.Context) *AuthResult {
	v, _ := ctx.Value(contextKey{}).(*AuthResult)
	return v
}

// WithContext attaches AuthResult to context.
func WithContext(ctx context.Context, result *AuthResult) context.Context {
	return context.WithValue(ctx, contextKey{}, result)
}
