package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/tenant"
)

const (
	GovernanceReadPermission    = "governance:read"
	GovernanceApprovePermission = "governance:approve"
)

type ReviewCapabilities struct {
	CanRead    bool
	CanApprove bool
}

// ReviewCapabilitiesFromResult never inherits default-open client permissions.
// Scope comes from verified identity claims, not request metadata.
func ReviewCapabilitiesFromResult(result *AuthResult) (ReviewCapabilities, bool) {
	var caps ReviewCapabilities
	if !HasVerifiedIdentity(result) || strings.TrimSpace(result.TenantID) == "" {
		return caps, false
	}
	for _, permission := range result.Permissions {
		switch permission {
		case GovernanceReadPermission:
			caps.CanRead = true
		case GovernanceApprovePermission, "admin":
			caps.CanRead, caps.CanApprove = true, true
		}
	}
	return caps, caps.CanRead
}

func reviewAuthResult(result *AuthResult) (*AuthResult, bool) {
	caps, ok := ReviewCapabilitiesFromResult(result)
	if !ok {
		return nil, false
	}
	permissions := []string{GovernanceReadPermission}
	if caps.CanApprove {
		permissions = append(permissions, GovernanceApprovePermission)
	}
	return &AuthResult{Identity: result.Identity, TenantID: result.TenantID, Permissions: permissions}, true
}

func isReviewAPIPath(path string) bool {
	return path == "/review-api" || strings.HasPrefix(path, "/review-api/")
}

// reviewRoutePermission is deliberately an allowlist, independent of the mux.
func reviewRoutePermission(method, path string) string {
	path = strings.TrimSuffix(path, "/")
	if path == "/review-api/session" && method == http.MethodDelete {
		return GovernanceReadPermission
	}
	parts := strings.Split(strings.TrimPrefix(path, "/review-api/"), "/")
	if len(parts) == 0 || (parts[0] != "pending-actions" && parts[0] != "connector-actions" && parts[0] != "credential-leases") {
		return ""
	}
	if (method == http.MethodGet || method == http.MethodHead) && (len(parts) == 1 || (len(parts) == 2 && validReviewID(parts[1]))) {
		return GovernanceReadPermission
	}
	if method == http.MethodPost && parts[0] == "pending-actions" && len(parts) == 3 && validReviewID(parts[1]) && (parts[2] == "approve" || parts[2] == "deny") {
		return GovernanceApprovePermission
	}
	return ""
}

func validReviewID(id string) bool { return id != "" && id != "." && id != ".." }

func validReviewSession(sess *AdminSession) bool {
	if sess == nil || sess.Purpose != SessionPurposeReview || sess.AbsoluteExpires.IsZero() || sessionExpired(sess, time.Now()) {
		return false
	}
	_, ok := ReviewCapabilitiesFromResult(&sess.Result)
	return ok && sess.CSRF != ""
}

func serveReviewRequest(w http.ResponseWriter, r *http.Request, provider Provider, opts MiddlewareOpts, next http.Handler) {
	permission := reviewRoutePermission(r.Method, r.URL.Path)
	if permission == "" {
		writeReviewForbidden(w, "review route is not permitted")
		return
	}
	if provider == nil {
		writeUnauthorized(w, "review requires configured primary authentication")
		return
	}
	var result *AuthResult
	if !hasAuthHeader(r) && opts.ReviewSessions != nil {
		sess := opts.ReviewSessions.Get(ReviewSessionIDFromRequest(r))
		if !validReviewSession(sess) {
			writeUnauthorized(w, "invalid or expired review session")
			return
		}
		if isMutatingMethod(r.Method) && !csrfTokenMatch(r.Header.Get(HeaderCSRF), sess.CSRF) {
			writeReviewForbidden(w, "invalid or missing CSRF token")
			return
		}
		result = &sess.Result
	} else {
		var err error
		result, err = provider.Authenticate(r)
		if err != nil {
			writeReviewAuthenticationError(w, err)
			return
		}
	}
	caps, ok := ReviewCapabilitiesFromResult(result)
	if !ok || (permission == GovernanceApprovePermission && !caps.CanApprove) {
		writeReviewForbidden(w, "insufficient scoped review permissions")
		return
	}
	attenuated, _ := reviewAuthResult(result)
	ctx := WithContext(r.Context(), attenuated)
	ctx = tenant.ScopedContext(ctx, attenuated.TenantID)
	next.ServeHTTP(w, r.WithContext(ctx))
}

func writeReviewForbidden(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusForbidden, map[string]string{"message": message})
}

func writeReviewAuthenticationError(w http.ResponseWriter, err error) {
	if _, ok := err.(*ErrForbidden); ok {
		writeReviewForbidden(w, "review credential is not permitted")
		return
	}
	writeUnauthorized(w, "invalid or missing review credential")
}
