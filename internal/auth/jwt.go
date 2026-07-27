package auth

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// JWTConfig configures the JWT auth provider.
type JWTConfig struct {
	JWKSURL  string `json:"jwks_url"`
	Issuer   string `json:"issuer,omitempty"`
	Audience string `json:"audience,omitempty"`
	// TenantClaim is the JWT claim name to read the caller's tenant ID
	// from (master plan: "Multi-tenancy"). Defaults to "tenant_id" if
	// unset. A token missing this claim resolves to tenant.DefaultTenant,
	// not an auth error -- multi-tenancy is opt-in per deployment.
	TenantClaim string `json:"tenant_claim,omitempty"`
	// ExtraClaims lists additional JWT claims to surface in
	// AuthResult.Extra verbatim by claim name (and, on the Python
	// runner, RunnerUser's dict-like/`.to_dict()` access), for
	// application code that needs more than identity/permissions/
	// tenant_id -- e.g. an `email` claim for per-user notifications, or
	// a custom claim a downstream tool needs. Absent means none are
	// surfaced (only Identity/Permissions/TenantID, the pre-existing
	// behavior, unchanged). A listed claim that's absent from a given
	// token is simply omitted from Extra, not an error.
	ExtraClaims []string `json:"extra_claims,omitempty"`
	// ForwardToken, when true, adds the raw, still-encoded bearer token
	// itself to AuthResult.Extra under RawTokenField -- needed when
	// downstream code does its own token exchange (e.g. RFC 8693) using
	// the SAME credential that authenticated the original request,
	// rather than a claim extracted from it. Off by default: forwarding
	// a live bearer credential to every consumer of AuthResult (and,
	// from there, every runner that receives RunAssignment.User) is a
	// meaningfully bigger trust surface than forwarding parsed claims,
	// so this opts in explicitly rather than happening automatically
	// just because JWT auth is configured.
	ForwardToken bool `json:"forward_token,omitempty"`
	// RawTokenField names the Extra key the raw token is stored under
	// when ForwardToken is true. Defaults to "token". Configurable
	// (rather than a fixed name) so an existing deployment's tool code
	// that already expects a specific key (e.g. a field named
	// "sso_token") can be pointed at unchanged, without runkite
	// needing to know that name in advance.
	RawTokenField string `json:"raw_token_field,omitempty"`
	// ClaimAliases renames ExtraClaims when writing AuthResult.Extra:
	// map key = claim name in the JWT, value = Extra key the runner
	// should see. Needed when JWT claim casing/naming (e.g. "orgUserId")
	// doesn't match what application code already looks up (e.g.
	// "org_user_id"). Claims not listed keep their JWT name.
	ClaimAliases map[string]string `json:"claim_aliases,omitempty"`
	// ForwardHeaders copies selected request headers into
	// AuthResult.Extra (map: header name → Extra key). Opt-in so a
	// deployment can forward sidecar tokens (e.g. X-HRSystem-Token →
	// hr_system_token) without baking header names into runkite.
	// Empty/missing header values are omitted, not errors.
	ForwardHeaders map[string]string `json:"forward_headers,omitempty"`
	// ScopeAsPermissions opts into treating the standard OAuth2 "scope"
	// claim as this app's read/write/admin permission list when no
	// "permissions" claim is present. Off by default: "scope" is a
	// client-consent concept (what the token issuer let the calling
	// client app request about the resource owner -- typically
	// "openid profile email" from any standard OIDC provider), not an
	// app-level authorization signal. Auto-converting it silently
	// denied every request from real SSO providers (their scope values
	// never happen to be "read"/"write"/"admin"), which is worse than
	// just leaving permissions empty -- an empty list already means
	// "unrestricted" (see authorized() in auth.go), the correct
	// behavior for an IdP that doesn't carry app RBAC in the token at
	// all. Set this only for deployments that deliberately mint scope
	// values matching this app's permission strings.
	ScopeAsPermissions bool `json:"scope_as_permissions,omitempty"`
}

// JWTProvider validates JWTs against a JWKS endpoint.
type JWTProvider struct {
	jwks               keyfunc.Keyfunc
	issuer             string
	audience           string
	tenantClaim        string
	extraClaims        []string
	forwardToken       bool
	rawTokenField      string
	claimAliases       map[string]string
	forwardHeaders     map[string]string
	scopeAsPermissions bool
}

// NewJWTProvider creates a JWT provider that fetches keys from a JWKS URL.
// The keyfunc library handles caching and automatic refresh.
func NewJWTProvider(cfg JWTConfig) (*JWTProvider, error) {
	k, err := keyfunc.NewDefault([]string{cfg.JWKSURL})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch JWKS from %s: %w", cfg.JWKSURL, err)
	}

	tenantClaim := cfg.TenantClaim
	if tenantClaim == "" {
		tenantClaim = "tenant_id"
	}
	rawTokenField := cfg.RawTokenField
	if rawTokenField == "" {
		rawTokenField = "token"
	}

	return &JWTProvider{
		jwks:               k,
		issuer:             cfg.Issuer,
		audience:           cfg.Audience,
		tenantClaim:        tenantClaim,
		extraClaims:        cfg.ExtraClaims,
		forwardToken:       cfg.ForwardToken,
		rawTokenField:      rawTokenField,
		claimAliases:       cfg.ClaimAliases,
		forwardHeaders:     cfg.ForwardHeaders,
		scopeAsPermissions: cfg.ScopeAsPermissions,
	}, nil
}

func (p *JWTProvider) Authenticate(r *http.Request) (*AuthResult, error) {
	tokenStr := extractBearerToken(r)
	if tokenStr == "" {
		return nil, &ErrUnauthorized{Message: "missing bearer token"}
	}

	parserOpts := []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512"}),
	}
	if p.issuer != "" {
		parserOpts = append(parserOpts, jwt.WithIssuer(p.issuer))
	}
	if p.audience != "" {
		parserOpts = append(parserOpts, jwt.WithAudience(p.audience))
	}

	token, err := jwt.Parse(tokenStr, p.jwks.Keyfunc, parserOpts...)
	if err != nil {
		return nil, &ErrUnauthorized{Message: "invalid token: " + err.Error()}
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, &ErrUnauthorized{Message: "invalid token claims"}
	}

	identity := "unknown"
	if sub, _ := claims["sub"].(string); sub != "" {
		identity = sub
	}

	var perms []string
	// "permissions" claim -- only read/write/admin survive filtering.
	// Auth0-style API permissions ("read:messages") and other IdP
	// vocabularies must not become a restrictive allow-list (same
	// failure mode as the old always-on scope fallback).
	if rawPerms, ok := claims["permissions"]; ok {
		perms = parsePermissionsClaim(rawPerms)
	}
	// Fallback: "scope" claim (space-separated string, OAuth2 convention)
	// -- opt-in only, see ScopeAsPermissions's doc comment for why.
	// Even when opted in, filter to app vocabulary so a misconfigured
	// deployment pointing ScopeAsPermissions at a normal OIDC token
	// still doesn't 403 on "openid profile email".
	if len(perms) == 0 && p.scopeAsPermissions {
		if scope, ok := claims["scope"].(string); ok && scope != "" {
			perms = filterAppPermissions(strings.Fields(scope))
		}
	}

	var tenantID string
	if v, _ := claims[p.tenantClaim].(string); v != "" {
		tenantID = v
	}

	var displayName string
	for _, claim := range []string{"name", "given_name", "preferred_username"} {
		if v, _ := claims[claim].(string); v != "" {
			displayName = v
			break
		}
	}

	needExtra := len(p.extraClaims) > 0 || p.forwardToken || len(p.forwardHeaders) > 0
	var extra map[string]interface{}
	if needExtra {
		extra = make(map[string]interface{}, len(p.extraClaims)+len(p.forwardHeaders)+1)
		for _, claimName := range p.extraClaims {
			if v, ok := claims[claimName]; ok {
				key := claimName
				if alias, ok := p.claimAliases[claimName]; ok && alias != "" {
					key = alias
				}
				extra[key] = v
			}
		}
		if p.forwardToken {
			extra[p.rawTokenField] = tokenStr
		}
		for headerName, extraKey := range p.forwardHeaders {
			if v := strings.TrimSpace(r.Header.Get(headerName)); v != "" {
				key := extraKey
				if key == "" {
					key = headerName
				}
				extra[key] = v
			}
		}
	}

	return &AuthResult{
		Identity:    identity,
		Permissions: perms,
		TenantID:    tenantID,
		DisplayName: displayName,
		Extra:       extra,
	}, nil
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}
