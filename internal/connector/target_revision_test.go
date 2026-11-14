package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func revisionFixture() ConnectorConfig {
	return ConnectorConfig{Auth: AuthConfig{Type: "bearer", BearerToken: "synthetic-old-token"}, MCP: &MCPConfig{URL: "https://mcp.example.test/payments"}}
}

func TestTargetRevisionIdentityAndSecretRotation(t *testing.T) {
	base := revisionFixture()
	want, err := TargetRevisionForConfig(base)
	if err != nil || !strings.HasPrefix(want, "target-v1-") || len(want) != 74 {
		t.Fatalf("revision=%q error=%v", want, err)
	}
	// Existing approval rows depend on the exact target-v1 descriptor encoding.
	// An intentional encoding change must introduce another version.
	if want != "target-v1-4748f63384ea97d2e5d8f52a575f02b91d63cea1d202d7075375113dcf5fe991" {
		t.Fatal("target-v1 encoding changed")
	}
	for _, change := range []func(*ConnectorConfig){
		func(c *ConnectorConfig) { c.Auth.BearerToken = "synthetic-new-token" },
		func(c *ConnectorConfig) { c.Auth.APIKey = "synthetic-key"; c.Auth.ClientSecret = "synthetic-secret" },
		func(c *ConnectorConfig) { c.MCP.Transport = "json" },
		func(c *ConnectorConfig) { c.CircuitBreaker = &CircuitBreakerConfig{FailureThreshold: 7} },
	} {
		cfg := snapshotConnectorConfig(base)
		change(&cfg)
		if got, err := TargetRevisionForConfig(cfg); err != nil || got != want {
			t.Fatalf("credential rotation or operational tuning invalidated target: error=%v", err)
		}
	}
	for _, change := range []func(*ConnectorConfig){
		func(c *ConnectorConfig) { c.MCP.URL = "https://mcp.example.test/other" },
		func(c *ConnectorConfig) { c.MCP.URL += "?tenant=other" },
		func(c *ConnectorConfig) { c.MCP.Transport = "streamable_http" },
		func(c *ConnectorConfig) { c.MCP.Revision = "payments-contract-2" },
		func(c *ConnectorConfig) { c.Auth.SecretRef = "env:PAYMENTS_TOKEN" },
		func(c *ConnectorConfig) { c.Auth.Type = "api_key" },
		func(c *ConnectorConfig) { c.Auth.Issuer = "https://issuer.example.test" },
		func(c *ConnectorConfig) { c.Auth.TokenURL = "https://issuer.example.test/token" },
		func(c *ConnectorConfig) { c.Auth.ClientID = "another-principal" },
		func(c *ConnectorConfig) { c.Auth.Scopes = []string{"write"} },
		func(c *ConnectorConfig) { c.Tools = &ToolFilter{Deny: []string{"delete"}} },
	} {
		cfg := snapshotConnectorConfig(base)
		change(&cfg)
		if got, err := TargetRevisionForConfig(cfg); err != nil || got == want {
			t.Fatalf("target contract change did not invalidate revision: error=%v", err)
		}
	}
	a, b := snapshotConnectorConfig(base), snapshotConnectorConfig(base)
	a.Auth.Scopes = []string{"write", "read", "read"}
	b.Auth.Scopes = []string{"read", "write"}
	first, _ := TargetRevisionForConfig(a)
	second, _ := TargetRevisionForConfig(b)
	if first != second {
		t.Fatal("equivalent scope sets changed target")
	}
}

func TestTargetRevisionRejectsAmbiguousOrCredentialEndpoints(t *testing.T) {
	for _, endpoint := range []string{"https://user:secret@mcp.example.test", "https://mcp.example.test?api_key=secret", "https://mcp.example.test?accessToken=secret", "https://mcp.example.test?bad=%zz", "file:///tmp/mcp", "not a URL"} {
		for field := range 3 {
			cfg := revisionFixture()
			switch field {
			case 0:
				cfg.MCP.URL = endpoint
			case 1:
				cfg.Auth.Issuer = endpoint
			case 2:
				cfg.Auth.TokenURL = endpoint
			}
			if _, err := TargetRevisionForConfig(cfg); !errors.Is(err, ErrTargetRevisionUnavailable) {
				t.Fatal("unsafe endpoint accepted")
			}
		}
	}
	cfg := revisionFixture()
	cfg.MCP.Revision = "bad\nrevision"
	if _, err := TargetRevisionForConfig(cfg); !errors.Is(err, ErrTargetRevisionUnavailable) {
		t.Fatal("control-bearing contract accepted")
	}
}

func TestPinnedMCPRequestUsesApprovedSnapshot(t *testing.T) {
	var oldCalls, newCalls atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-old-token" {
			t.Error("pinned auth configuration changed")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	defer old.Close()
	newTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { newCalls.Add(1) }))
	defer newTarget.Close()
	cfg := revisionFixture()
	cfg.MCP.URL = old.URL
	registry := NewRegistry(map[string]ConnectorConfig{"fixture": cfg})
	bound, revision, err := registry.BindMCPRequestTarget(context.Background(), "fixture")
	if err != nil || BoundMCPTransport(bound) != "json" {
		t.Fatal("target was not bound")
	}
	c, _ := registry.Get("fixture")
	// Sequential mutation simulates a config replacement after binding. Live
	// concurrent writes to exported Go config are not a supported reload API.
	c.Config.MCP.URL = newTarget.URL
	c.Config.Auth.BearerToken = "synthetic-new-token"
	if _, err := registry.ProxyMCPRequest(bound, "fixture", nil, toolCallRequest(1, "write")); err != nil {
		t.Fatal(err)
	}
	if oldCalls.Load() != 1 || newCalls.Load() != 0 {
		t.Fatal("request escaped its approved target snapshot")
	}
	// A fresh request using the old revision must fail before target effects.
	ctx := WithExpectedTargetRevision(context.Background(), revision)
	if _, err := registry.ProxyMCPRequest(ctx, "fixture", nil, toolCallRequest(1, "write")); !errors.Is(err, ErrTargetRevisionChanged) {
		t.Fatalf("mismatch error=%v", err)
	}
	if oldCalls.Load() != 1 || newCalls.Load() != 0 {
		t.Fatal("mismatched target was contacted")
	}
}

func TestTargetReferenceRotationIsResolvedAtDispatch(t *testing.T) {
	t.Setenv("RUNKITE_TARGET_TEST_TOKEN", "synthetic-before")
	var observed string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	defer target.Close()
	cfg := revisionFixture()
	cfg.MCP.URL = target.URL
	cfg.Auth.BearerToken = ""
	cfg.Auth.SecretRef = "env:RUNKITE_TARGET_TEST_TOKEN"
	registry := NewRegistry(map[string]ConnectorConfig{"fixture": cfg})
	ctx, revision, err := registry.BindMCPRequestTarget(context.Background(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNKITE_TARGET_TEST_TOKEN", "synthetic-after")
	if current, _ := registry.TargetRevision("fixture"); current != revision {
		t.Fatal("resolved secret value affected revision")
	}
	if _, err := registry.ProxyMCPRequest(ctx, "fixture", nil, toolsListRequest(1)); err != nil {
		t.Fatal(err)
	}
	if observed != "Bearer synthetic-after" {
		t.Fatal("secret reference rotation was not resolved at dispatch")
	}
}
