package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// ReviewSessionHandlers authenticates only through the primary identity provider.
// Its cookie, CSRF token, storage namespace and authority are separate from Admin.
type ReviewSessionHandlers struct {
	Store      SessionStore
	Provider   Provider
	loginLimit *loginLimiter
}

type reviewSessionStatus struct {
	Authenticated bool       `json:"authenticated"`
	AuthRequired  bool       `json:"auth_required"`
	Configured    bool       `json:"configured"`
	Identity      string     `json:"identity"`
	TenantID      string     `json:"tenant_id"`
	CanRead       bool       `json:"can_read"`
	CanApprove    bool       `json:"can_approve"`
	CSRFToken     string     `json:"csrf_token"`
	ExpiresAt     *time.Time `json:"expires_at"`
}

func (h *ReviewSessionHandlers) status(result *AuthResult, sess *AdminSession) reviewSessionStatus {
	out := reviewSessionStatus{AuthRequired: true, Configured: h != nil && h.Provider != nil}
	if !out.Configured {
		return out
	}
	caps, ok := ReviewCapabilitiesFromResult(result)
	if !ok {
		return out
	}
	out.Authenticated, out.Identity, out.TenantID = true, result.Identity, result.TenantID
	out.CanRead, out.CanApprove = caps.CanRead, caps.CanApprove
	if sess != nil {
		out.CSRFToken = sess.CSRF
		expiry := sess.AbsoluteExpires.UTC()
		out.ExpiresAt = &expiry
	}
	return out
}

func (h *ReviewSessionHandlers) Create(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h == nil || h.Provider == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "review requires configured primary authentication"})
		return
	}
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "review sessions are not configured"})
		return
	}
	creator, ok := h.Store.(ReviewSessionCreator)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"message": "session store does not support review sessions"})
		return
	}
	lim := h.loginLimit
	if lim == nil {
		lim = defaultLoginLimiter
	}
	if !lim.allow(clientIP(r)) {
		w.Header().Set("Retry-After", loginRetryAfter())
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "too many login attempts"})
		return
	}
	var body struct {
		Credential string `json:"credential"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid login request"})
			return
		}
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid login request"})
			return
		}
	}
	credential := strings.TrimSpace(body.Credential)
	request := r.Clone(r.Context())
	request.Header = r.Header.Clone()
	if credential != "" {
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Del("X-API-Key")
	}
	result, err := h.Provider.Authenticate(request)
	if err != nil {
		writeReviewAuthenticationError(w, err)
		return
	}
	if _, ok := ReviewCapabilitiesFromResult(result); !ok {
		writeReviewForbidden(w, "review requires an explicit identity, tenant and role")
		return
	}
	sess, err := creator.CreateReview(result)
	if err != nil || !validReviewSession(sess) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "failed to create review session"})
		return
	}
	setReviewSessionCookie(w, r, sess.ID, time.Until(sess.AbsoluteExpires))
	writeJSON(w, http.StatusOK, h.status(&sess.Result, sess))
}

func (h *ReviewSessionHandlers) Status(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h == nil || h.Provider == nil {
		writeJSON(w, http.StatusOK, h.status(nil, nil))
		return
	}
	if hasAuthHeader(r) {
		result, err := h.Provider.Authenticate(r)
		if err != nil {
			writeReviewAuthenticationError(w, err)
			return
		}
		if _, ok := ReviewCapabilitiesFromResult(result); !ok {
			writeReviewForbidden(w, "insufficient scoped review permissions")
			return
		}
		writeJSON(w, http.StatusOK, h.status(result, nil))
		return
	}
	if h.Store != nil {
		sess := h.Store.Get(ReviewSessionIDFromRequest(r))
		if validReviewSession(sess) {
			writeJSON(w, http.StatusOK, h.status(&sess.Result, sess))
			return
		}
	}
	writeJSON(w, http.StatusOK, h.status(nil, nil))
}

// Delete requires middleware authentication and cookie CSRF for cookie callers.
func (h *ReviewSessionHandlers) Delete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h == nil || h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "review sessions are not configured"})
		return
	}
	if hasAuthHeader(r) && ReviewSessionIDFromRequest(r) != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "omit Authorization and API-key headers to log out the browser session with its CSRF token"})
		return
	}
	// A machine bearer authenticates itself, not an ambient browser cookie.
	if !hasAuthHeader(r) {
		if err := h.Store.Delete(ReviewSessionIDFromRequest(r)); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "logout could not be confirmed; try again", "reason_code": "review_session_revoke_failed"})
			return
		}
	}
	clearReviewSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func ReviewSessionIDFromRequest(r *http.Request) string {
	cookie, err := r.Cookie(CookieReviewSession)
	if err != nil || cookie == nil {
		return ""
	}
	return cookie.Value
}

func setReviewSessionCookie(w http.ResponseWriter, r *http.Request, id string, ttl time.Duration) {
	seconds := int(ttl.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	http.SetCookie(w, &http.Cookie{Name: CookieReviewSession, Value: id, Path: "/review-api", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: requestLooksHTTPS(r), MaxAge: seconds})
}

func clearReviewSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: CookieReviewSession, Path: "/review-api", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: requestLooksHTTPS(r), MaxAge: -1})
}
