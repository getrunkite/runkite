// Package customroutes builds the control-plane reverse proxy that mounts
// a product-owned HTTP app (in-runner ASGI/Node or sidecar) alongside the
// Agent Protocol API.
//
// Trust model for identity: the control plane's auth middleware has already
// authenticated the caller before this proxy runs. We strip any inbound
// X-Runkite-* identity headers (anti-spoof) and re-inject them from the
// authenticated AuthResult / tenant context so the custom app can trust
// "who is this?" without re-implementing JWT/API-key verification.
package customroutes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/tenant"
)

// Header names injected into proxied requests. Custom apps SHOULD treat
// these as the trust boundary for identity (not re-parse Authorization).
const (
	HeaderIdentity    = "X-Runkite-Identity"
	HeaderTenantID    = "X-Runkite-Tenant-Id"
	HeaderPermissions = "X-Runkite-Permissions"
	HeaderDisplayName = "X-Runkite-Display-Name"
	HeaderUserJSON    = "X-Runkite-User" // flat JSON: identity, permissions, tenant_id, display_name
)

// DefaultMount is used when custom_routes.mount is omitted.
const DefaultMount = "/custom"

// identityHeaders are always stripped from the inbound client request
// before we overwrite them from the authenticated context.
var identityHeaders = []string{
	HeaderIdentity,
	HeaderTenantID,
	HeaderPermissions,
	HeaderDisplayName,
	HeaderUserJSON,
}

// reservedMountPrefixes collide with Agent Protocol / platform routes and
// must not be used as a custom_routes.mount.
var reservedMountPrefixes = []string{
	"/threads",
	"/runs",
	"/agents",
	"/assistants",
	"/store",
	"/admin",
	"/admin-api",
	"/internal",
	"/health",
	"/ready",
	"/live",
	"/metrics",
	"/vectors",
	"/ok",
	"/openapi",
	"/docs",
	"/a2a",
}

// NormalizeMount validates and canonicalizes a mount path.
// Empty → DefaultMount. Result always has a leading slash and no trailing
// slash (except we never return "/").
func NormalizeMount(mount string) (string, error) {
	mount = strings.TrimSpace(mount)
	if mount == "" {
		mount = DefaultMount
	}
	if !strings.HasPrefix(mount, "/") {
		return "", fmt.Errorf("custom_routes.mount must start with / (got %q)", mount)
	}
	if mount == "/" {
		return "", fmt.Errorf("custom_routes.mount cannot be / (would shadow the Agent Protocol API)")
	}
	mount = strings.TrimRight(mount, "/")
	if strings.Contains(mount, "//") {
		return "", fmt.Errorf("custom_routes.mount must not contain empty segments (got %q)", mount)
	}
	for _, reserved := range reservedMountPrefixes {
		if mount == reserved || strings.HasPrefix(mount, reserved+"/") {
			return "", fmt.Errorf("custom_routes.mount %q collides with reserved path %q", mount, reserved)
		}
	}
	return mount, nil
}

// Matches reports whether path is under mount (exact or prefix with /).
func Matches(mount, path string) bool {
	return path == mount || strings.HasPrefix(path, mount+"/")
}

// NewProxy returns a handler that strips mount and reverse-proxies to
// target, injecting authenticated identity headers on every request.
func NewProxy(target *url.URL, mount string) (http.Handler, error) {
	mount, err := NormalizeMount(mount)
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.WriteHeader(http.StatusBadGateway)
	}
	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		injectIdentity(req)
	}
	return http.StripPrefix(mount, proxy), nil
}

func injectIdentity(req *http.Request) {
	for _, h := range identityHeaders {
		req.Header.Del(h)
	}
	ar := auth.FromContext(req.Context())
	if ar == nil {
		return
	}
	if ar.Identity != "" {
		req.Header.Set(HeaderIdentity, ar.Identity)
	}
	// Prefer the ambient tenant context (what every store call sees)
	// over AuthResult.TenantID alone -- auth middleware writes both.
	tenantID := tenant.FromContext(req.Context())
	req.Header.Set(HeaderTenantID, tenantID)
	if len(ar.Permissions) > 0 {
		req.Header.Set(HeaderPermissions, strings.Join(ar.Permissions, ","))
	}
	if ar.DisplayName != "" {
		req.Header.Set(HeaderDisplayName, ar.DisplayName)
	}
	user := map[string]any{
		"identity":    ar.Identity,
		"permissions": ar.Permissions,
		"tenant_id":   tenantID,
	}
	if ar.DisplayName != "" {
		user["display_name"] = ar.DisplayName
	}
	if raw, err := json.Marshal(user); err == nil {
		req.Header.Set(HeaderUserJSON, string(raw))
	}
}
