package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	ErrTargetRevisionChanged     = errors.New("connector target changed; resubmit the operation for fresh review")
	ErrTargetRevisionUnavailable = errors.New("connector target revision is unavailable")
)

// TargetRevisionForConfig fingerprints configured target identity, never a
// resolved credential or an upstream implementation. Endpoint URLs must not
// contain credentials. Revision is an operator's nonsecret contract version.
func TargetRevisionForConfig(cfg ConnectorConfig) (string, error) {
	if cfg.MCP == nil {
		return "", ErrTargetRevisionUnavailable
	}
	if !nonsecretTargetURL(cfg.MCP.URL) {
		return "", ErrTargetRevisionUnavailable
	}
	for _, endpoint := range []string{cfg.Auth.Issuer, cfg.Auth.TokenURL} {
		if endpoint != "" && !nonsecretTargetURL(endpoint) {
			return "", ErrTargetRevisionUnavailable
		}
	}
	mode := cfg.MCP.Transport
	if mode == "" {
		mode = "json"
	}
	if mode != "json" && mode != "streamable_http" {
		return "", ErrTargetRevisionUnavailable
	}
	if len(cfg.MCP.Revision) > 128 || strings.ContainsFunc(cfg.MCP.Revision, unicode.IsControl) {
		return "", ErrTargetRevisionUnavailable
	}
	stringsToCheck := []string{cfg.MCP.URL, cfg.MCP.Revision, cfg.Auth.Type, cfg.Auth.Issuer, cfg.Auth.TokenURL, cfg.Auth.ClientID, cfg.Auth.SecretRef}
	stringsToCheck = append(stringsToCheck, cfg.Auth.Scopes...)
	var allow, deny []string
	if cfg.Tools != nil {
		allow, deny = cfg.Tools.Allow, cfg.Tools.Deny
		stringsToCheck = append(stringsToCheck, allow...)
		stringsToCheck = append(stringsToCheck, deny...)
	}
	for _, value := range stringsToCheck {
		if !utf8.ValidString(value) {
			return "", ErrTargetRevisionUnavailable
		}
	}
	// Field order is part of target-v1. Changing this descriptor requires a new
	// version, not reinterpretation of already approved fingerprints.
	descriptor := struct {
		Endpoint        string   `json:"endpoint"`
		Transport       string   `json:"transport"`
		Contract        string   `json:"contract"`
		AuthType        string   `json:"auth_type"`
		Issuer          string   `json:"issuer"`
		TokenEndpoint   string   `json:"token_endpoint"`
		ClientID        string   `json:"client_id"`
		SecretReference string   `json:"secret_reference"`
		Scopes          []string `json:"scopes"`
		AllowedTools    []string `json:"allowed_tools"`
		DeniedTools     []string `json:"denied_tools"`
	}{cfg.MCP.URL, mode, cfg.MCP.Revision, cfg.Auth.Type, cfg.Auth.Issuer, cfg.Auth.TokenURL, cfg.Auth.ClientID, cfg.Auth.SecretRef, targetSet(cfg.Auth.Scopes), targetSet(allow), targetSet(deny)}
	var value any = descriptor
	prefix := "target-v1-"
	if cfg.MCP.PinTools || cfg.Auth.CredentialBinding != "" {
		prefix = "target-v2-"
		value = struct {
			Base                                         any
			PinTools                                     bool
			CredentialBinding, CredentialBindingRevision string
		}{descriptor, cfg.MCP.PinTools, cfg.Auth.CredentialBinding, cfg.Auth.CredentialBindingRevision}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", ErrTargetRevisionUnavailable
	}
	digest := sha256.Sum256(raw)
	return prefix + hex.EncodeToString(digest[:]), nil
}

func nonsecretTargetURL(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for key := range query {
		key = strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(key))
		switch key {
		case "apikey", "accesstoken", "refreshtoken", "clientsecret", "password", "authorization", "bearertoken", "token":
			return false
		}
	}
	return true
}

func targetSet(values []string) []string {
	copy := append([]string{}, values...)
	slices.Sort(copy)
	return slices.Compact(copy)
}

type mcpRequestTarget struct {
	registry       *Registry
	connector      *Connector
	name, revision string
	config         ConnectorConfig
}

type mcpRequestTargetKey struct{}
type expectedTargetRevisionKey struct{}

func snapshotConnectorConfig(cfg ConnectorConfig) ConnectorConfig {
	copy := cfg
	copy.Auth.Scopes = append([]string(nil), cfg.Auth.Scopes...)
	if cfg.MCP != nil {
		mcp := *cfg.MCP
		copy.MCP = &mcp
	}
	if cfg.Tools != nil {
		copy.Tools = &ToolFilter{Allow: append([]string(nil), cfg.Tools.Allow...), Deny: append([]string(nil), cfg.Tools.Deny...)}
	}
	if cfg.CircuitBreaker != nil {
		breaker := *cfg.CircuitBreaker
		copy.CircuitBreaker = &breaker
	}
	if cfg.Errors != nil {
		copy.Errors = make(map[string]string, len(cfg.Errors))
		for key, value := range cfg.Errors {
			copy.Errors[key] = value
		}
	}
	return copy
}

func (r *Registry) prepareMCPRequestTarget(name string) (*mcpRequestTarget, error) {
	if r == nil {
		return nil, ErrTargetRevisionUnavailable
	}
	c, err := r.Get(name)
	if err != nil {
		return nil, ErrTargetRevisionUnavailable
	}
	cfg := snapshotConnectorConfig(c.Config)
	revision, err := TargetRevisionForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.MCP.Transport == "" {
		cfg.MCP.Transport = "json"
	}
	return &mcpRequestTarget{registry: r, connector: c, name: name, revision: revision, config: cfg}, nil
}

func (r *Registry) TargetRevision(name string) (string, error) {
	target, err := r.prepareMCPRequestTarget(name)
	if err != nil {
		return "", err
	}
	return target.revision, nil
}

// BindMCPRequestTarget pins one request's endpoint, transport, filter and auth
// configuration before authority is evaluated or consumed. Secret references
// are still resolved at session time; values never enter the fingerprint.
func (r *Registry) BindMCPRequestTarget(ctx context.Context, name string) (context.Context, string, error) {
	target, err := r.prepareMCPRequestTarget(name)
	if err != nil {
		return ctx, "", err
	}
	ctx = context.WithValue(ctx, mcpRequestTargetKey{}, target)
	return WithExpectedTargetRevision(ctx, target.revision), target.revision, nil
}

func WithExpectedTargetRevision(ctx context.Context, revision string) context.Context {
	return context.WithValue(ctx, expectedTargetRevisionKey{}, revision)
}

func BoundMCPTransport(ctx context.Context) string {
	if target, ok := ctx.Value(mcpRequestTargetKey{}).(*mcpRequestTarget); ok {
		return target.config.MCP.Transport
	}
	return ""
}

func (r *Registry) requestTarget(ctx context.Context, name string) (*mcpRequestTarget, error) {
	target, bound := ctx.Value(mcpRequestTargetKey{}).(*mcpRequestTarget)
	if bound && (target.registry != r || target.name != name) {
		return nil, ErrTargetRevisionChanged
	}
	if !bound {
		var err error
		if _, expected := ctx.Value(expectedTargetRevisionKey{}).(string); expected {
			target, err = r.prepareMCPRequestTarget(name)
		} else {
			// Direct Go proxy callers have no managed approval contract. Keep
			// their existing transport/error behavior without fingerprinting a
			// potentially credential-bearing legacy endpoint. Managed HTTP calls
			// bind a target before evaluating policy and cannot take this path.
			var c *Connector
			c, err = r.Get(name)
			if err == nil {
				cfg := snapshotConnectorConfig(c.Config)
				if cfg.MCP == nil {
					return nil, fmt.Errorf("connector %s has no MCP endpoint configured", name)
				}
				if cfg.MCP.Transport == "" {
					cfg.MCP.Transport = "json"
				}
				if cfg.MCP.Transport != "json" && cfg.MCP.Transport != "streamable_http" {
					return nil, fmt.Errorf("connector %s: unsupported MCP transport", name)
				}
				target = &mcpRequestTarget{registry: r, connector: c, name: name, config: cfg}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if expected, present := ctx.Value(expectedTargetRevisionKey{}).(string); present && expected != target.revision {
		return nil, ErrTargetRevisionChanged
	}
	return target, nil
}
