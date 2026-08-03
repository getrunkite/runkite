package auth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// AdminSessionHandlers serves POST/GET/DELETE /admin-api/session.
// Login authenticates the pasted credential once; the browser never
// retains the API key/JWT afterward.
type AdminSessionHandlers struct {
	Store         *AdminSessionStore
	AdminProvider Provider
	Provider      Provider
	Strict        bool
}

// Create handles POST /admin-api/session.
func (h *AdminSessionHandlers) Create(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.Store == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "admin sessions not configured"})
		return
	}
	var body struct {
		Credential string `json:"credential"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	credential := strings.TrimSpace(body.Credential)
	if credential == "" {
		// Also accept Authorization on the login call itself (curl/scripts).
		credential = extractKey(r)
	}

	result, err := authenticateAdminCredential(h.AdminProvider, h.Provider, credential, h.Strict)
	if err != nil {
		status := http.StatusUnauthorized
		msg := "unauthorized"
		if e, ok := err.(*ErrForbidden); ok {
			status = http.StatusForbidden
			msg = e.Message
		} else if e, ok := err.(*ErrUnauthorized); ok {
			msg = e.Message
		}
		writeJSON(w, status, map[string]string{"message": msg})
		return
	}

	sess, err := h.Store.Create(result)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "failed to create session"})
		return
	}
	SetSessionCookie(w, r, sess.ID, h.Store.ttl)
	writeJSON(w, http.StatusOK, map[string]any{
		"csrf_token":    sess.CSRF,
		"identity":      sess.Result.Identity,
		"auth_required": true,
	})
}

// Status handles GET /admin-api/session -- bootstrap for page refresh
// and the "is auth configured" probe (no cookie, open deployment).
func (h *AdminSessionHandlers) Status(w http.ResponseWriter, r *http.Request) {
	authRequired := h != nil && (h.Provider != nil || h.AdminProvider != nil)
	if h == nil || h.Store == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": !authRequired,
			"auth_required": authRequired,
		})
		return
	}
	if sess := h.Store.Get(SessionIDFromRequest(r)); sess != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"auth_required": true,
			"csrf_token":    sess.CSRF,
			"identity":      sess.Result.Identity,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": !authRequired,
		"auth_required": authRequired,
	})
}

// Delete handles DELETE /admin-api/session (logout). Middleware already
// authenticated the session cookie + CSRF before this runs.
func (h *AdminSessionHandlers) Delete(w http.ResponseWriter, r *http.Request) {
	if h != nil && h.Store != nil {
		h.Store.Delete(SessionIDFromRequest(r))
	}
	ClearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// authenticateAdminCredential verifies a pasted admin credential the
// same way Middleware would for /admin-api/* (admin_keys first, then
// primary + admin permission). Empty credential succeeds only when no
// providers are configured (open local/dev).
func authenticateAdminCredential(adminProvider, provider Provider, credential string, strict bool) (*AuthResult, error) {
	if adminProvider == nil && provider == nil {
		if credential != "" {
			return nil, &ErrUnauthorized{Message: "auth is not configured"}
		}
		return &AuthResult{Identity: "anonymous", Permissions: nil}, nil
	}

	req, err := http.NewRequest(http.MethodGet, "/admin-api/session", nil)
	if err != nil {
		return nil, err
	}
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}

	if adminProvider != nil {
		if result, err := adminProvider.Authenticate(req); err == nil {
			return result, nil
		} else if provider == nil {
			return nil, err
		}
	}

	if provider == nil {
		return nil, &ErrUnauthorized{Message: "invalid or missing admin credential"}
	}
	result, err := provider.Authenticate(req)
	if err != nil {
		return nil, err
	}
	if !authorizedAdmin(result, strict) {
		return nil, &ErrForbidden{Message: "insufficient permissions (requires 'admin')"}
	}
	return result, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
