package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveSecretRefEnv(t *testing.T) {
	t.Setenv("RK_TEST_SECRET", "from-env")
	got, err := ResolveSecretRef(context.Background(), "env:RK_TEST_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-env" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveSecretRefFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	if err := os.WriteFile(path, []byte("  file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSecretRef(context.Background(), "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "file-secret" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveSecretRefVault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok" {
			t.Errorf("missing token header")
		}
		if r.URL.Path != "/v1/secret/data/runkite/connectors/github" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"data":{"token":"vault-val"}}}`))
	}))
	defer srv.Close()

	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", "tok")
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")

	got, err := ResolveSecretRef(context.Background(), "vault:secret/data/runkite/connectors/github#token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "vault-val" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveSecretRefVaultPrefixDenied(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
	t.Setenv("VAULT_TOKEN", "tok")
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")
	_, err := ResolveSecretRef(context.Background(), "vault:secret/data/other/x#token")
	if err == nil || !strings.Contains(err.Error(), "outside allowed prefix") {
		t.Fatalf("want prefix deny, got %v", err)
	}
}

func TestResolveSecretRefVaultDotDotDenied(t *testing.T) {
	t.Setenv("VAULT_ADDR", "http://127.0.0.1:8200")
	t.Setenv("VAULT_TOKEN", "tok")
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")
	_, err := ResolveSecretRef(context.Background(), "vault:secret/data/runkite/../other/x#token")
	if err == nil || (!strings.Contains(err.Error(), "..") && !strings.Contains(err.Error(), "outside allowed prefix")) {
		t.Fatalf("want .. deny, got %v", err)
	}
}

func TestResolveSecretRefVaultHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["denied"]}`))
	}))
	defer srv.Close()
	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", "tok")
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/data/runkite/")
	_, err := ResolveSecretRef(context.Background(), "vault:secret/data/runkite/connectors/github#token")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("want HTTP 403, got %v", err)
	}
}

func TestResolveSecretRefVaultKV1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secret/runkite/connectors/github" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"token":"kv1-val"}}`))
	}))
	defer srv.Close()
	t.Setenv("VAULT_ADDR", srv.URL)
	t.Setenv("VAULT_TOKEN", "tok")
	t.Setenv("VAULT_ALLOWED_PREFIX", "secret/runkite/")
	got, err := ResolveSecretRef(context.Background(), "vault:secret/runkite/connectors/github#token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "kv1-val" {
		t.Fatalf("got %q", got)
	}
}

func TestMaterializeAuthSecretRejectsBoth(t *testing.T) {
	t.Setenv("RK_BOTH", "x")
	auth := AuthConfig{Type: "api_key", APIKey: "inline", SecretRef: "env:RK_BOTH"}
	err := materializeAuthSecret(context.Background(), &auth)
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("want conflict error, got %v", err)
	}
}

func TestGetSessionSecretRefAPIKey(t *testing.T) {
	t.Setenv("RK_SESSION_KEY", "session-secret")
	r := NewRegistry(map[string]ConnectorConfig{
		"svc": {Auth: AuthConfig{Type: "api_key", SecretRef: "env:RK_SESSION_KEY"}},
	})
	sess, err := r.GetSession(context.Background(), "svc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Credentials["access_token"] != "session-secret" {
		t.Fatalf("credentials = %#v", sess.Credentials)
	}
	// Registry config must stay unset (no mutate shared auth).
	c, _ := r.Get("svc")
	if c.Config.Auth.APIKey != "" {
		t.Fatalf("shared APIKey mutated to %q", c.Config.Auth.APIKey)
	}
}
