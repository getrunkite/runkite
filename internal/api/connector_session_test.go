package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestConnectorSession_MintAndRequire(t *testing.T) {
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"sf": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: "http://127.0.0.1:1"},
		},
	})
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetConnectorSessionStore(connector.NewMemoryConnectorSessionStore(0))

	binding := &auth.RunBinding{RunID: "run-1", Generation: 2, TenantID: "acme", AgentID: "sales"}

	// Mint via session handler
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/session", nil)
	req.SetPathValue("name", "sf")
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	rec := httptest.NewRecorder()
	s.handleGetConnectorSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("session: %d %s", rec.Code, rec.Body.String())
	}
	var sess connector.SessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.SessionToken == "" {
		t.Fatal("expected session_token")
	}
	if sess.Credentials != nil {
		t.Fatalf("MCP session must omit credentials: %v", sess.Credentials)
	}
	exp, err := time.Parse(time.RFC3339, sess.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at: %v", err)
	}
	if exp.Before(time.Now().Add(10 * time.Minute)) {
		t.Fatalf("expected ~15m capability expiry, got %v", sess.ExpiresAt)
	}

	// /mcp without token
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req = httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/mcp", strings.NewReader(body))
	req.SetPathValue("name", "sf")
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	rec = httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}

	// Wrong run_id
	req = httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/mcp", strings.NewReader(body))
	req.SetPathValue("name", "sf")
	req.Header.Set(connector.HeaderConnectorSession, sess.SessionToken)
	req = req.WithContext(auth.WithRunBinding(req.Context(), &auth.RunBinding{
		RunID: "other", Generation: 2, TenantID: "acme", AgentID: "sales",
	}))
	rec = httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 run_id mismatch, got %d %s", rec.Code, rec.Body.String())
	}

	// Same run_id, different generation (reclaim / redequeue)
	req = httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/mcp", strings.NewReader(body))
	req.SetPathValue("name", "sf")
	req.Header.Set(connector.HeaderConnectorSession, sess.SessionToken)
	req = req.WithContext(auth.WithRunBinding(req.Context(), &auth.RunBinding{
		RunID: "run-1", Generation: 3, TenantID: "acme", AgentID: "sales",
	}))
	rec = httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 generation mismatch, got %d %s", rec.Code, rec.Body.String())
	}

	// Matching token — reaches proxy (downstream 127.0.0.1:1 → 502 or similar is fine)
	req = httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/mcp", strings.NewReader(body))
	req.SetPathValue("name", "sf")
	req.Header.Set(connector.HeaderConnectorSession, sess.SessionToken)
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	rec = httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Fatalf("token should be accepted, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestConnectorSession_NonMCPNoToken(t *testing.T) {
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"myapi": {Auth: connector.AuthConfig{Type: "api_key", APIKey: "k"}},
	})
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetConnectorSessionStore(connector.NewMemoryConnectorSessionStore(0))

	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/myapi/session", nil)
	req.SetPathValue("name", "myapi")
	req = req.WithContext(auth.WithRunBinding(req.Context(), &auth.RunBinding{
		RunID: "r", Generation: 1, TenantID: "t", AgentID: "a",
	}))
	rec := httptest.NewRecorder()
	s.handleGetConnectorSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var sess connector.SessionResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &sess)
	if sess.SessionToken != "" {
		t.Fatalf("non-MCP must not mint session_token, got %q", sess.SessionToken)
	}
	if sess.Credentials["access_token"] != "k" {
		t.Fatalf("credentials = %v", sess.Credentials)
	}
}
