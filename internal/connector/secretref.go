package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ResolveSecretRef fetches a secret for connector auth at GetSession time.
// Supported schemes:
//
//	env:VAR_NAME
//	file:/absolute/or/relative/path
//	vault:secret/data/runkite/...#field   (KV v2 JSON; requires VAULT_ADDR + token)
//
// Empty ref returns ("", nil). Unknown schemes and Vault paths outside
// VAULT_ALLOWED_PREFIX (default "secret/data/runkite/") fail closed.
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

func resolveVaultKV(ctx context.Context, pathAndField string) (string, error) {
	path, field, ok := strings.Cut(pathAndField, "#")
	if !ok || path == "" || field == "" {
		return "", fmt.Errorf("secret_ref vault: want path#field, got %q", pathAndField)
	}
	prefix := os.Getenv("VAULT_ALLOWED_PREFIX")
	if prefix == "" {
		prefix = "secret/data/runkite/"
	}
	if !strings.HasPrefix(path, prefix) {
		return "", fmt.Errorf("secret_ref vault: path %q outside allowed prefix %q", path, prefix)
	}
	addr := strings.TrimRight(os.Getenv("VAULT_ADDR"), "/")
	if addr == "" {
		return "", fmt.Errorf("secret_ref vault: VAULT_ADDR is unset")
	}
	token, err := vaultToken()
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr+"/v1/"+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Vault-Token", token)
	if ns := os.Getenv("VAULT_NAMESPACE"); ns != "" {
		req.Header.Set("X-Vault-Namespace", ns)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
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

	var envelope struct {
		Data struct {
			Data map[string]interface{} `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", fmt.Errorf("secret_ref vault: decode: %w", err)
	}
	raw, ok := envelope.Data.Data[field]
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
