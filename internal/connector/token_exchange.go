package connector

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/getrunkite/runkite/internal/credentials"
	"github.com/getrunkite/runkite/internal/models"
)

const accessTokenType = "urn:ietf:params:oauth:token-type:access_token"

// TokenExchangeConfig is an operator-owned target and actor contract. It is
// never populated from tool arguments or user-provided target claims.
type TokenExchangeConfig struct {
	Audiences           []string `json:"audiences,omitempty" yaml:"audiences,omitempty"`
	Resources           []string `json:"resources,omitempty" yaml:"resources,omitempty"`
	ActorTokenSecretRef string   `json:"actor_token_secret_ref,omitempty" yaml:"actor_token_secret_ref,omitempty"`
	ActorID             string   `json:"actor_id,omitempty" yaml:"actor_id,omitempty"`
	MaxTTLSeconds       int64    `json:"max_ttl_seconds,omitempty" yaml:"max_ttl_seconds,omitempty"`
}

var errExchangeContract = errors.New("token exchange is outside the registered delegation contract")

type delegatedSubjectKey struct{}
type delegatedSubject struct{ token string }

func (delegatedSubject) String() string { return "[redacted delegated subject assertion]" }

// WithDelegatedSubject is a trusted plane hook. HTTP handlers derive this from
// the assignment's authenticating user, never runner body/header user_context.
func WithDelegatedSubject(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, delegatedSubjectKey{}, delegatedSubject{token})
}

func constrainedSubjectToken(ctx context.Context, supplied map[string]interface{}) (string, error) {
	assertion, ok := ctx.Value(delegatedSubjectKey{}).(delegatedSubject)
	if !ok || assertion.token == "" || len(assertion.token) > 16384 || strings.ContainsAny(assertion.token, "\r\n\x00") {
		return "", errExchangeContract
	}
	if raw, present := supplied["sso_token"]; present {
		token, ok := raw.(string)
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(assertion.token)) != 1 {
			return "", errExchangeContract
		}
	}
	return assertion.token, nil
}

func exchangeText(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// ValidateTokenExchangeConfig rejects accidental use under another grant and
// ambiguous/unbounded targets before credentials are fetched or requests sent.
func ValidateTokenExchangeConfig(cfg AuthConfig) error {
	p := cfg.TokenExchange
	if p == nil {
		return nil
	}
	if cfg.Type != "oauth2_token_exchange" || (len(p.Audiences) == 0 && len(p.Resources) == 0) || len(p.Audiences) > 16 || len(p.Resources) > 16 || len(cfg.Scopes) == 0 || len(cfg.Scopes) > 32 || p.MaxTTLSeconds < 0 || p.MaxTTLSeconds > 86400 {
		return errExchangeContract
	}
	endpoint := cfg.TokenURL
	if endpoint == "" && cfg.Issuer != "" {
		endpoint = strings.TrimRight(cfg.Issuer, "/") + "/oauth2/token"
	}
	if !exchangeEndpointAllowed(endpoint, cfg.AllowInsecureLoopback) {
		return errExchangeContract
	}
	for _, list := range [][]string{p.Audiences, p.Resources, cfg.Scopes} {
		seen := map[string]bool{}
		for _, value := range list {
			if !exchangeText(value) || seen[value] {
				return errExchangeContract
			}
			seen[value] = true
		}
	}
	for _, scope := range cfg.Scopes {
		for _, r := range scope {
			if r < 0x21 || r > 0x7e || r == '"' || r == '\\' {
				return errExchangeContract
			}
		}
	}
	for _, resource := range p.Resources {
		u, err := url.Parse(resource)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
			return errExchangeContract
		}
	}
	if (p.ActorTokenSecretRef == "") != (p.ActorID == "") || (p.ActorID != "" && !exchangeText(p.ActorID)) {
		return errExchangeContract
	}
	if p.ActorTokenSecretRef != "" {
		scheme, rest, ok := strings.Cut(p.ActorTokenSecretRef, ":")
		if !ok || rest == "" || len(p.ActorTokenSecretRef) > 2048 || (scheme != "env" && scheme != "file" && scheme != "vault") {
			return errExchangeContract
		}
	}
	return nil
}

func exchangeEndpointAllowed(endpoint string, loopback bool) bool {
	u, e := url.Parse(endpoint)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return loopback && u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))
}

func validateRawExchangeResponse(body []byte) error {
	if !utf8.Valid(body) {
		return errExchangeContract
	}
	fields, err := strictRPCObject(body, []string{"access_token", "refresh_token", "expires_in", "token_type", "issued_token_type", "scope", "instance_url"})
	if err != nil {
		return errExchangeContract
	}
	for _, name := range []string{"access_token", "refresh_token", "token_type", "issued_token_type", "scope", "instance_url"} {
		if raw, present := fields[name]; present {
			var value string
			if string(raw) == "null" || json.Unmarshal(raw, &value) != nil || len(value) > 16384 || strings.ContainsFunc(value, unicode.IsControl) {
				return errExchangeContract
			}
		}
	}
	return nil
}

func applyExchangeContract(ctx context.Context, cfg AuthConfig, subject string, data url.Values) error {
	if err := ValidateTokenExchangeConfig(cfg); err != nil {
		return err
	}
	p := cfg.TokenExchange
	if p == nil {
		return nil
	}
	if len(subject) == 0 || len(subject) > 16384 || strings.ContainsAny(subject, "\r\n\x00") {
		return errExchangeContract
	}
	for _, value := range p.Audiences {
		data.Add("audience", value)
	}
	for _, value := range p.Resources {
		data.Add("resource", value)
	}
	if p.ActorTokenSecretRef != "" {
		actor, err := resolveExchangeSecret(ctx, p.ActorTokenSecretRef, cfg.AllowInsecureLoopback)
		if err != nil || actor == "" || len(actor) > 16384 || strings.ContainsAny(actor, "\r\n\x00") {
			return errExchangeContract
		}
		data.Set("actor_token", actor)
		data.Set("actor_token_type", accessTokenType)
	}
	return nil
}

func resolveExchangeSecret(ctx context.Context, ref string, allowLoopback bool) (string, error) {
	scheme, path, _ := strings.Cut(ref, ":")
	if scheme == "vault" && !exchangeEndpointAllowed(os.Getenv("VAULT_ADDR"), allowLoopback) {
		return "", errExchangeContract
	}
	if scheme == "file" {
		value, err := credentials.ReadPrivateFile(path, 16384)
		if err != nil {
			return "", errExchangeContract
		}
		defer clear(value)
		return strings.TrimSpace(string(value)), nil
	}
	return ResolveSecretRef(ctx, ref)
}

func validateExchangeResponse(cfg AuthConfig, tokenType, issuedType, scope string, expires int64) error {
	p := cfg.TokenExchange
	if p == nil {
		return nil
	}
	maxTTL := p.MaxTTLSeconds
	if maxTTL == 0 {
		maxTTL = 3600
	}
	if !strings.EqualFold(tokenType, "Bearer") || issuedType != accessTokenType || expires <= 0 || expires > maxTTL {
		return errExchangeContract
	}
	// RFC 6749 permits omission when granted scope equals requested scope.
	// An explicit response may only preserve or narrow registered scopes.
	if scope != "" {
		for _, granted := range strings.Split(scope, " ") {
			if granted == "" || !slices.Contains(cfg.Scopes, granted) {
				return errExchangeContract
			}
		}
	}
	return nil
}

func cloneExchangeConfig(p *TokenExchangeConfig) *TokenExchangeConfig {
	if p == nil {
		return nil
	}
	copy := *p
	copy.Audiences = append([]string(nil), p.Audiences...)
	copy.Resources = append([]string(nil), p.Resources...)
	return &copy
}

// BoundDelegationContract uses the frozen request target, never live config or
// fields supplied by an agent. It describes intended exchange constraints.
func (r *Registry) BoundDelegationContract(ctx context.Context, name string) (*models.DelegationContract, error) {
	target, err := r.requestTarget(ctx, name)
	if err != nil {
		return nil, err
	}
	cfg := target.config.Auth
	if cfg.TokenExchange == nil {
		return nil, nil
	}
	if err := ValidateTokenExchangeConfig(cfg); err != nil {
		return nil, err
	}
	p := cfg.TokenExchange
	ttl := p.MaxTTLSeconds
	if ttl == 0 {
		ttl = 3600
	}
	return &models.DelegationContract{Audiences: append([]string(nil), p.Audiences...), Resources: append([]string(nil), p.Resources...), Scopes: append([]string(nil), cfg.Scopes...), ConfiguredActor: p.ActorID, MaxTTLSeconds: ttl}, nil
}
