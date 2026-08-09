package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestRegistry_GetAndList(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"alpha": {Auth: AuthConfig{Type: "api_key", APIKey: "k1"}},
		"beta":  {Auth: AuthConfig{Type: "bearer", BearerToken: "tok"}},
	})

	names := r.List()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("List() = %v, want [alpha beta]", names)
	}

	c, err := r.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "alpha" {
		t.Fatalf("Get(alpha).Name = %s", c.Name)
	}

	_, err = r.Get("nonexistent")
	if err == nil {
		t.Fatal("expected ErrNotFound for unknown connector")
	}
}

func TestRegistry_EmptyRegistry(t *testing.T) {
	r := NewRegistry(nil)
	if names := r.List(); len(names) != 0 {
		t.Fatalf("empty registry List() = %v", names)
	}
	_, err := r.Get("anything")
	if err == nil {
		t.Fatal("expected error from empty registry")
	}
}

func TestToolFilter_AllowList(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth:  AuthConfig{Type: "api_key", APIKey: "x"},
			Tools: &ToolFilter{Allow: []string{"query", "read"}},
		},
	})

	if !r.IsToolAllowed("sf", "query") {
		t.Error("query should be allowed")
	}
	if !r.IsToolAllowed("sf", "read") {
		t.Error("read should be allowed")
	}
	if r.IsToolAllowed("sf", "delete") {
		t.Error("delete should not be allowed")
	}
}

func TestToolFilter_DenyList(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth:  AuthConfig{Type: "api_key", APIKey: "x"},
			Tools: &ToolFilter{Deny: []string{"delete", "bulkUpdate"}},
		},
	})

	if !r.IsToolAllowed("sf", "query") {
		t.Error("query should be allowed (not in deny)")
	}
	if r.IsToolAllowed("sf", "delete") {
		t.Error("delete should be denied")
	}
	if r.IsToolAllowed("sf", "bulkUpdate") {
		t.Error("bulkUpdate should be denied")
	}
}

func TestToolFilter_AllowAndDeny(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth: AuthConfig{Type: "api_key", APIKey: "x"},
			Tools: &ToolFilter{
				Allow: []string{"query", "read", "delete"},
				Deny:  []string{"delete"},
			},
		},
	})

	if !r.IsToolAllowed("sf", "query") {
		t.Error("query should be allowed")
	}
	// deny takes precedence over allow
	if r.IsToolAllowed("sf", "delete") {
		t.Error("delete should be denied (deny overrides allow)")
	}
	if r.IsToolAllowed("sf", "other") {
		t.Error("other should not be allowed (not in allow list)")
	}
}

func TestToolFilter_NoFilter(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"open": {Auth: AuthConfig{Type: "api_key", APIKey: "x"}},
	})

	if !r.IsToolAllowed("open", "anything") {
		t.Error("no filter = all tools allowed")
	}
}

func TestToolFilter_UnknownConnector(t *testing.T) {
	r := NewRegistry(nil)
	if r.IsToolAllowed("nope", "tool") {
		t.Error("unknown connector should return false")
	}
}

func assertStaticCredentialExpiresAt(t *testing.T, expiresAt string) {
	t.Helper()
	got, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		t.Fatalf("expires_at not RFC3339: %s", expiresAt)
	}
	// Advisory remint hint: ~StaticCredentialSessionTTL from now (not ~365d).
	want := time.Now().Add(StaticCredentialSessionTTL)
	skew := got.Sub(want)
	if skew < -5*time.Second || skew > 5*time.Second {
		t.Fatalf("expires_at = %s, want ~%s (±5s)", got.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339))
	}
}

func TestSession_APIKey(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"myapi": {Auth: AuthConfig{Type: "api_key", APIKey: "secret-key-123"}},
	})

	sess, err := r.GetSession(context.Background(), "myapi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Credentials["access_token"] != "secret-key-123" {
		t.Fatalf("expected api key in access_token, got %s", sess.Credentials["access_token"])
	}
	assertStaticCredentialExpiresAt(t, sess.ExpiresAt)
}

func TestSession_Bearer(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"svc": {Auth: AuthConfig{Type: "bearer", BearerToken: "my-bearer-tok"}},
	})

	sess, err := r.GetSession(context.Background(), "svc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Credentials["access_token"] != "my-bearer-tok" {
		t.Fatalf("expected bearer token, got %s", sess.Credentials["access_token"])
	}
	assertStaticCredentialExpiresAt(t, sess.ExpiresAt)
}

func TestSession_ClientCredentials(t *testing.T) {
	// Mock OAuth2 token endpoint
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.FormValue("grant_type") != "client_credentials" {
			http.Error(w, "wrong grant_type", http.StatusBadRequest)
			return
		}
		if r.FormValue("client_id") != "myid" || r.FormValue("client_secret") != "mysecret" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}

		resp := map[string]interface{}{
			"access_token": "cc-token-abc",
			"expires_in":   7200,
			"token_type":   "Bearer",
			"instance_url": "https://myservice.example.com",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	r := NewRegistry(map[string]ConnectorConfig{
		"svc": {
			Auth: AuthConfig{
				Type:         "oauth2_client_credentials",
				TokenURL:     srv.URL,
				ClientID:     "myid",
				ClientSecret: "mysecret",
				Scopes:       []string{"read", "write"},
			},
		},
	})

	sess, err := r.GetSession(context.Background(), "svc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Credentials["access_token"] != "cc-token-abc" {
		t.Fatalf("expected cc-token-abc, got %s", sess.Credentials["access_token"])
	}
	if sess.Credentials["instance_url"] != "https://myservice.example.com" {
		t.Fatalf("expected instance_url, got %s", sess.Credentials["instance_url"])
	}

	// Second call should use cached token
	sess2, err := r.GetSession(context.Background(), "svc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess2.Credentials["access_token"] != "cc-token-abc" {
		t.Fatal("expected cached token on second call")
	}
}

func TestSession_ClientCredentials_Error(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	r := NewRegistry(map[string]ConnectorConfig{
		"bad": {Auth: AuthConfig{Type: "oauth2_client_credentials", TokenURL: srv.URL}},
	})

	_, err := r.GetSession(context.Background(), "bad", nil)
	if err == nil {
		t.Fatal("expected error from failed token request")
	}
}

func TestSession_NotFound(t *testing.T) {
	r := NewRegistry(nil)
	_, err := r.GetSession(context.Background(), "nope", nil)
	if err == nil {
		t.Fatal("expected error for unknown connector")
	}
}

func TestSession_MCPInfo(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth:  AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:   &MCPConfig{URL: "https://sf-mcp.internal/sse"},
			Tools: &ToolFilter{Allow: []string{"soqlQuery", "getSchema"}},
		},
	})

	sess, err := r.GetSession(context.Background(), "sf", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.MCP == nil {
		t.Fatal("expected MCP in session response")
	}
	// URL points at this control plane's own MCP proxy (relative path),
	// NOT the connector's raw downstream URL -- see MCPSession.URL's own
	// doc comment / mcpproxy.go for why: routing through the proxy is
	// what makes tool allow/deny enforcement real instead of advisory.
	if sess.MCP.URL != "/internal/connectors/sf/mcp" {
		t.Fatalf("MCP URL = %s, want the proxy path", sess.MCP.URL)
	}
	if len(sess.MCP.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %v", sess.MCP.Tools)
	}
}

func TestSession_ResponseShape(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"x": {Auth: AuthConfig{Type: "api_key", APIKey: "test"}},
	})

	sess, err := r.GetSession(context.Background(), "x", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Verify JSON marshalling produces expected shape
	b, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	json.Unmarshal(b, &m)

	if _, ok := m["credentials"]; !ok {
		t.Fatal("missing 'credentials' in JSON")
	}
	if _, ok := m["expires_at"]; !ok {
		t.Fatal("missing 'expires_at' in JSON")
	}

	assertStaticCredentialExpiresAt(t, sess.ExpiresAt)
}

func TestEnvVarExpansion(t *testing.T) {
	os.Setenv("TEST_CONNECTOR_KEY", "expanded-value")
	defer os.Unsetenv("TEST_CONNECTOR_KEY")

	r := NewRegistry(map[string]ConnectorConfig{
		"env": {Auth: AuthConfig{Type: "api_key", APIKey: "${TEST_CONNECTOR_KEY}"}},
	})

	sess, err := r.GetSession(context.Background(), "env", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Credentials["access_token"] != "expanded-value" {
		t.Fatalf("env var not expanded: got %s", sess.Credentials["access_token"])
	}
}

func TestEnvVarExpansion_Multiple(t *testing.T) {
	os.Setenv("TEST_CID", "client123")
	os.Setenv("TEST_CSEC", "secret456")
	defer os.Unsetenv("TEST_CID")
	defer os.Unsetenv("TEST_CSEC")

	r := NewRegistry(map[string]ConnectorConfig{
		"multi": {Auth: AuthConfig{
			Type:         "oauth2_client_credentials",
			TokenURL:     "http://example.com/token",
			ClientID:     "${TEST_CID}",
			ClientSecret: "${TEST_CSEC}",
		}},
	})

	c, _ := r.Get("multi")
	if c.Config.Auth.ClientID != "client123" {
		t.Fatalf("ClientID not expanded: %s", c.Config.Auth.ClientID)
	}
	if c.Config.Auth.ClientSecret != "secret456" {
		t.Fatalf("ClientSecret not expanded: %s", c.Config.Auth.ClientSecret)
	}
}

func TestEnvVarExpansion_Unset(t *testing.T) {
	os.Unsetenv("TOTALLY_MISSING_VAR")

	r := NewRegistry(map[string]ConnectorConfig{
		"missing": {Auth: AuthConfig{Type: "api_key", APIKey: "${TOTALLY_MISSING_VAR}"}},
	})

	sess, err := r.GetSession(context.Background(), "missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Unset env var expands to empty string
	if sess.Credentials["access_token"] != "" {
		t.Fatalf("expected empty string for unset var, got %q", sess.Credentials["access_token"])
	}
}

func TestSession_UnsupportedType(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"bad": {Auth: AuthConfig{Type: "kerberos"}},
	})

	_, err := r.GetSession(context.Background(), "bad", nil)
	if err == nil {
		t.Fatal("expected error for unsupported auth type")
	}
}

func TestSession_TokenExchange_NoSSOToken(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"exch": {Auth: AuthConfig{
			Type:   "oauth2_token_exchange",
			Issuer: "https://idp.example.com",
		}},
	})

	_, err := r.GetSession(context.Background(), "exch", nil)
	if err == nil {
		t.Fatal("expected error when sso_token missing")
	}
}
