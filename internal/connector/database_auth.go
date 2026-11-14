package connector

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
)

var ErrDatabaseCredentialsUnavailable = errors.New("database connector credentials unavailable")

type databaseAuthKey struct{}
type databaseAuthorization struct{ binding, revision, header string }

// WithMCPDatabaseCredentials is a trusted executor hook, not an HTTP header or
// runner capability. Its values must never be serialized or returned to agents.
func WithMCPDatabaseCredentials(ctx context.Context, binding, revision, username, password string) (context.Context, error) {
	if binding == "" || revision == "" || username == "" || password == "" {
		return ctx, ErrDatabaseCredentialsUnavailable
	}
	for _, c := range username {
		if c == ':' || c == '\r' || c == '\n' {
			return ctx, ErrDatabaseCredentialsUnavailable
		}
	}
	value := databaseAuthorization{binding, revision, "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))}
	return context.WithValue(ctx, databaseAuthKey{}, value), nil
}

func mcpAuthorization(ctx context.Context, c *Connector, cfg ConnectorConfig, user map[string]interface{}, method string) (string, error) {
	if cfg.Auth.Type != "vault_database" {
		token, err := c.getTokenForAuth(ctx, user, false, cfg.Auth)
		if err != nil {
			return "", err
		}
		if token.AccessToken == "" {
			return "", nil
		}
		return "Bearer " + token.AccessToken, nil
	}
	if cfg.MCP == nil || cfg.Auth.CredentialBinding == "" || cfg.Auth.CredentialBindingRevision == "" {
		return "", ErrDatabaseCredentialsUnavailable
	}
	u, err := url.Parse(cfg.MCP.URL)
	if err != nil {
		return "", ErrDatabaseCredentialsUnavailable
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || !cfg.Auth.AllowInsecureLoopback || ip == nil || !ip.IsLoopback() {
			return "", ErrDatabaseCredentialsUnavailable
		}
	}
	switch method {
	case "initialize", "notifications/initialized", "ping", "tools/list":
		return "", nil
	case "tools/call":
		value, ok := ctx.Value(databaseAuthKey{}).(databaseAuthorization)
		if !ok || value.binding != cfg.Auth.CredentialBinding || value.revision != cfg.Auth.CredentialBindingRevision {
			return "", ErrDatabaseCredentialsUnavailable
		}
		return value.header, nil
	default:
		return "", ErrDatabaseCredentialsUnavailable
	}
}

func (r *Registry) BoundCredentialBinding(ctx context.Context, name string) (string, string, error) {
	target, err := r.requestTarget(ctx, name)
	if err != nil {
		return "", "", err
	}
	if target.config.Auth.Type != "vault_database" {
		return "", "", nil
	}
	if _, err := mcpAuthorization(ctx, target.connector, target.config, nil, "tools/list"); err != nil {
		return "", "", err
	}
	return target.config.Auth.CredentialBinding, target.config.Auth.CredentialBindingRevision, nil
}
