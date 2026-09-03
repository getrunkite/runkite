package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// Shared Vault HTTP client — one Timeout for all secret_ref vault: lookups
// (avoids per-call Client allocation and keeps dial/timeout behavior consistent).
var vaultHTTPClient = &http.Client{Timeout: 10 * time.Second}

// ResolveSecretRef fetches a secret for connector auth at GetSession time.
// Supported schemes:
//
//	env:VAR_NAME
//	file:/absolute/or/relative/path
//	vault:secret/data/runkite/...#field   (KV v2 JSON data.data)
//	vault:secret/runkite/...#field        (KV v1 JSON data — no /data/ segment)
//
// Empty ref returns ("", nil). Unknown schemes and Vault paths outside
// VAULT_ALLOWED_PREFIX (default "secret/data/runkite/") fail closed.
// Paths are cleaned before the prefix check so `..` cannot escape the allowlist.
func ResolveSecretRef(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", fmt.Errorf("secret_ref: expected scheme:value, got %q", ref)
	}
	switch strings.ToLower(scheme) {
	case "env":
		v := os.Getenv(rest)
		if v == "" {
			return "", fmt.Errorf("secret_ref env %q is empty or unset", rest)
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(rest)
		if err != nil {
			return "", fmt.Errorf("secret_ref file %q: %w", rest, err)
		}
		return strings.TrimSpace(string(b)), nil
	case "vault":
		return resolveVaultKV(ctx, rest)
	default:
		return "", fmt.Errorf("secret_ref: unsupported scheme %q (want env, file, or vault)", scheme)
	}
}

// cleanVaultPath rejects ".." escapes then re-checks the allowlist prefix on
// the cleaned path (HasPrefix alone on the raw string is not enough).
func cleanVaultPath(raw, prefix string) (string, error) {
	if strings.Contains(raw, "..") {
		return "", fmt.Errorf("secret_ref vault: path must not contain '..'")
	}
	cleaned := path.Clean("/" + strings.TrimPrefix(raw, "/"))
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("secret_ref vault: empty path")
	}
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("secret_ref vault: path must not contain '..'")
	}
	if !strings.HasPrefix(cleaned, prefix) {
		return "", fmt.Errorf("secret_ref vault: path %q outside allowed prefix %q", cleaned, prefix)
	}
	return cleaned, nil
}

func resolveVaultKV(ctx context.Context, pathAndField string) (string, error) {
	rawPath, field, ok := strings.Cut(pathAndField, "#")
	if !ok || rawPath == "" || field == "" {
		return "", fmt.Errorf("secret_ref vault: want path#field, got %q", pathAndField)
	}
	prefix := os.Getenv("VAULT_ALLOWED_PREFIX")
	if prefix == "" {
		prefix = "secret/data/runkite/"
	}
	vaultPath, err := cleanVaultPath(rawPath, prefix)
	if err != nil {
		return "", err
	}
	addr := strings.TrimRight(os.Getenv("VAULT_ADDR"), "/")
	if addr == "" {
		return "", fmt.Errorf("secret_ref vault: VAULT_ADDR is unset")
	}
	token, err := vaultToken()
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/v1/"+vaultPath, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Vault-Token", token)
	if ns := os.Getenv("VAULT_NAMESPACE"); ns != "" {
		req.Header.Set("X-Vault-Namespace", ns)
	}

	resp, err := vaultHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("secret_ref vault: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("secret_ref vault: HTTP %d", resp.StatusCode)
	}

	raw, err := vaultFieldFromBody(body, field, strings.Contains(vaultPath, "/data/"))
	if err != nil {
		return "", err
	}
	return raw, nil
}

// vaultFieldFromBody reads KV v2 (data.data) when preferV2, else KV v1 (data).
// If preferV2 fails the nested lookup, falls back to v1 shape so mixed mounts work.
func vaultFieldFromBody(body []byte, field string, preferV2 bool) (string, error) {
	if preferV2 {
		var v2 struct {
			Data struct {
				Data map[string]interface{} `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &v2); err != nil {
			return "", fmt.Errorf("secret_ref vault: decode: %w", err)
		}
		if v2.Data.Data != nil {
			return vaultStringField(v2.Data.Data, field)
		}
	}
	var v1 struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(body, &v1); err != nil {
		return "", fmt.Errorf("secret_ref vault: decode: %w", err)
	}
	return vaultStringField(v1.Data, field)
}

func vaultStringField(m map[string]interface{}, field string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("secret_ref vault: field %q missing", field)
	}
	raw, ok := m[field]
	if !ok {
		return "", fmt.Errorf("secret_ref vault: field %q missing", field)
	}
	switch v := raw.(type) {
	case string:
		if v == "" {
			return "", fmt.Errorf("secret_ref vault: field %q is empty", field)
		}
		return v, nil
	default:
		return "", fmt.Errorf("secret_ref vault: field %q is not a string", field)
	}
}

func vaultToken() (string, error) {
	if t := os.Getenv("VAULT_TOKEN"); t != "" {
		return t, nil
	}
	if p := os.Getenv("VAULT_TOKEN_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("secret_ref vault: read VAULT_TOKEN_FILE: %w", err)
		}
		t := strings.TrimSpace(string(b))
		if t == "" {
			return "", fmt.Errorf("secret_ref vault: VAULT_TOKEN_FILE is empty")
		}
		return t, nil
	}
	return "", fmt.Errorf("secret_ref vault: set VAULT_TOKEN or VAULT_TOKEN_FILE")
}

// materializeAuthSecret fills APIKey / BearerToken / ClientSecret from
// SecretRef when that is how the connector stores the credential. Fails if
// both an inline secret and secret_ref are set for the same auth type.
func materializeAuthSecret(ctx context.Context, auth *AuthConfig) error {
	if auth == nil || strings.TrimSpace(auth.SecretRef) == "" {
		return nil
	}
	secret, err := ResolveSecretRef(ctx, auth.SecretRef)
	if err != nil {
		return err
	}
	switch auth.Type {
	case "api_key":
		if auth.APIKey != "" {
			return fmt.Errorf("auth: set api_key or secret_ref, not both")
		}
		auth.APIKey = secret
	case "bearer":
		if auth.BearerToken != "" {
			return fmt.Errorf("auth: set bearer_token or secret_ref, not both")
		}
		auth.BearerToken = secret
	case "oauth2_client_credentials", "oauth2_token_exchange":
		if auth.ClientSecret != "" {
			return fmt.Errorf("auth: set client_secret or secret_ref, not both")
		}
		auth.ClientSecret = secret
	default:
		return fmt.Errorf("auth: secret_ref unsupported for type %q", auth.Type)
	}
	return nil
}
