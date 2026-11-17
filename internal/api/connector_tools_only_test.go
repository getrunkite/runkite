package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestToolsOnlyConnectorInfoProjectsProfileWithoutSecrets(t *testing.T) {
	s := NewServer(nil, nil, nil, nil)
	configs := map[string]connector.ConnectorConfig{}
	for _, name := range []string{"restricted", "transparent", "non-mcp"} {
		cfg := connector.ConnectorConfig{Auth: connector.AuthConfig{Type: "bearer", BearerToken: "synthetic-private-token", ClientSecret: "synthetic-private-client-secret"}}
		if name != "non-mcp" {
			cfg.MCP = &connector.MCPConfig{URL: "https://synthetic-private-host.invalid/mcp?token=synthetic-private-query", ToolsOnly: name == "restricted"}
		}
		configs[name] = cfg
	}
	s.SetConnectorRegistry(connector.NewRegistry(configs))
	type info struct {
		Name         string `json:"name"`
		MCP          string `json:"mcp"`
		MCPToolsOnly *bool  `json:"mcp_tools_only"`
	}
	check := func(t *testing.T, row info) {
		t.Helper()
		if row.MCPToolsOnly == nil || *row.MCPToolsOnly != (row.Name == "restricted") {
			t.Fatalf("missing or incorrect safe profile: %+v", row)
		}
		want := ""
		if row.Name != "non-mcp" {
			want = "/internal/connectors/" + row.Name + "/mcp"
		}
		if row.MCP != want {
			t.Fatalf("proxy path=%q, want %q", row.MCP, want)
		}
	}
	for _, prefix := range []string{"/admin-api", "/internal"} {
		t.Run(prefix, func(t *testing.T) {
			for _, suffix := range []string{"", "/restricted", "/transparent", "/non-mcp"} {
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, prefix+"/connectors"+suffix, nil))
				if w.Code != 200 || strings.Contains(w.Body.String(), "synthetic-private-") {
					t.Fatalf("unsafe connector projection: status=%d body=%s", w.Code, w.Body.String())
				}
				if suffix == "" {
					var rows []info
					if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 3 {
						t.Fatalf("connector list: %v %s", err, w.Body.String())
					}
					for _, row := range rows {
						check(t, row)
					}
				} else {
					var row info
					if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
						t.Fatal(err)
					}
					check(t, row)
				}
			}
		})
	}
}

type toolsOnlyRoundTripper func(*http.Request) (*http.Response, error)

func (f toolsOnlyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Only the upstream HTTP boundary is scripted. Requests exercise the real mux,
// session validation, policy checks and connector proxy without a listener.
func toolsOnlyUpstream(t *testing.T) *atomic.Int32 {
	t.Helper()
	hits := &atomic.Int32{}
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = toolsOnlyRoundTripper(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		if r.URL.String() != "https://synthetic-mcp.invalid/mcp" || r.Header.Get("Authorization") != "Bearer synthetic-fixture-token" {
			return nil, errors.New("unexpected synthetic upstream request")
		}
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			return nil, err
		}
		status := http.StatusOK
		body := ""
		if len(rpc.ID) == 0 {
			status = http.StatusAccepted
		} else {
			var result any = map[string]any{"synthetic_method": rpc.Method}
			switch rpc.Method {
			case "initialize":
				result = map[string]any{
					"protocolVersion": "2025-11-25", "serverInfo": map[string]any{"name": "synthetic-upstream", "version": "1"},
					"capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}, "logging": map[string]any{}},
				}
			case "tools/list":
				result = map[string]any{"tools": []any{
					map[string]any{"name": "write", "inputSchema": map[string]any{"type": "object"}},
					map[string]any{"name": "blocked", "inputSchema": map[string]any{"type": "object"}},
				}}
			case "tools/call":
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "synthetic-result"}}}
			}
			raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
			if err != nil {
				return nil, err
			}
			body = string(raw)
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	return hits
}

func toolsOnlyRegistry(strict bool) *connector.Registry {
	return connector.NewRegistry(map[string]connector.ConnectorConfig{"fixture": {
		Auth:  connector.AuthConfig{Type: "bearer", BearerToken: "synthetic-fixture-token"},
		MCP:   &connector.MCPConfig{URL: "https://synthetic-mcp.invalid/mcp", ToolsOnly: strict},
		Tools: &connector.ToolFilter{Allow: []string{"write"}},
	}})
}

func toolsOnlyRequest(s *Server, ctx context.Context, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/mcp", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set(connector.HeaderConnectorSession, token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func toolsOnlyMint(t *testing.T, s *Server, ctx context.Context) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/session", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	var response connector.SessionResponse
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.SessionToken == "" || response.Credentials != nil {
		t.Fatalf("session mint failed or exposed credentials: %d %s", w.Code, w.Body.String())
	}
	return response.SessionToken
}

func TestToolsOnlyExistingSessionUsesCurrentConnectorProfile(t *testing.T) {
	hits := toolsOnlyUpstream(t)
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(toolsOnlyRegistry(false))
	s.SetConnectorSessionStore(connector.NewMemoryConnectorSessionStore(0))
	binding := &auth.RunBinding{RunID: "synthetic-run", Generation: 1, TenantID: "acme", AgentID: "agent"}
	ctx := auth.WithRunBinding(tenant.WithContext(t.Context(), binding.TenantID), binding)
	token := toolsOnlyMint(t, s, ctx)
	resources := `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`
	if w := toolsOnlyRequest(s, ctx, token, resources); w.Code != 200 || !strings.Contains(w.Body.String(), `"synthetic_method":"resources/list"`) || hits.Load() != 1 {
		t.Fatalf("default profile lost transparency: %d %s hits=%d", w.Code, w.Body.String(), hits.Load())
	}
	s.SetConnectorRegistry(toolsOnlyRegistry(true))
	if w := toolsOnlyRequest(s, ctx, "", resources); w.Code != 401 || hits.Load() != 1 {
		t.Fatalf("strict method gate bypassed session requirement: %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{"resources/list", "prompts/get", "logging/setLevel", "server/discover", "extension/execute"} {
		w := toolsOnlyRequest(s, ctx, token, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method))
		var response struct {
			Error struct {
				Code int `json:"code"`
				Data struct {
					Reason string `json:"reason_code"`
				} `json:"data"`
			} `json:"error"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Error.Code != -32601 || response.Error.Data.Reason != "connector_mcp_method_denied" || hits.Load() != 1 {
			t.Fatalf("current profile did not deny %s locally: %d %s hits=%d", method, w.Code, w.Body.String(), hits.Load())
		}
	}
	if w := toolsOnlyRequest(s, ctx, token, `{"jsonrpc":"2.0","method":"resources/subscribe"}`); w.Code != 202 || w.Body.Len() != 0 || hits.Load() != 1 {
		t.Fatalf("denied notification forwarded or fabricated response: %d %s hits=%d", w.Code, w.Body.String(), hits.Load())
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"synthetic-client","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write","arguments":{}}}`,
	} {
		before := hits.Load()
		w := toolsOnlyRequest(s, ctx, token, body)
		if (w.Code != 200 && w.Code != 202) || hits.Load() != before+1 {
			t.Fatalf("allowed operation did not forward: %d %s hits=%d", w.Code, w.Body.String(), hits.Load())
		}
		if strings.Contains(body, `"method":"initialize"`) && (strings.Contains(w.Body.String(), `"resources"`) || strings.Contains(w.Body.String(), `"prompts"`) || strings.Contains(w.Body.String(), `"logging"`)) {
			t.Fatalf("initialize advertised unsupported capabilities: %s", w.Body.String())
		}
		if strings.Contains(body, `"method":"tools/list"`) && strings.Contains(w.Body.String(), `"blocked"`) {
			t.Fatalf("tools-only bypassed the existing tool filter: %s", w.Body.String())
		}
	}
	s.SetConnectorRegistry(toolsOnlyRegistry(false))
	before := hits.Load()
	if w := toolsOnlyRequest(s, ctx, token, resources); w.Code != 200 || hits.Load() != before+1 {
		t.Fatalf("same session did not observe restored default profile: %d %s", w.Code, w.Body.String())
	}
	if w := toolsOnlyRequest(s, ctx, "", resources); w.Code != 401 || hits.Load() != before+1 {
		t.Fatalf("method gate bypassed session requirement: %d %s", w.Code, w.Body.String())
	}
}

func TestToolsOnlyInvalidInitializationIsBadRequestBeforeCredentials(t *testing.T) {
	hits := toolsOnlyUpstream(t)
	s := NewServer(nil, nil, nil, nil)
	s.SetConnectorRegistry(toolsOnlyRegistry(false))
	s.SetConnectorSessionStore(connector.NewMemoryConnectorSessionStore(0))
	binding := &auth.RunBinding{RunID: "synthetic-run", Generation: 1, TenantID: "acme", AgentID: "agent"}
	ctx := auth.WithRunBinding(tenant.WithContext(t.Context(), binding.TenantID), binding)
	token := toolsOnlyMint(t, s, ctx)
	strict, err := toolsOnlyRegistry(true).Get("fixture")
	if err != nil {
		t.Fatal(err)
	}
	cfg := strict.Config
	cfg.Auth = connector.AuthConfig{Type: "oauth2_client_credentials", TokenURL: "https://synthetic-oauth.invalid/token", ClientID: "synthetic-client", ClientSecret: "synthetic-private-client-secret"}
	s.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{"fixture": cfg}))
	for _, params := range []string{`{"capabilities":[],"clientInfo":{"name":"synthetic-private-input"}}`, `{"Capabilities":{"roots":{}},"clientInfo":{"name":"synthetic-private-input"}}`} {
		w := toolsOnlyRequest(s, ctx, token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+params+`}`)
		if w.Code != 400 || hits.Load() != 0 || strings.Contains(w.Body.String(), "synthetic-private-") || !strings.Contains(w.Body.String(), "bad initialization request") {
			t.Fatalf("invalid initialization reached credentials/upstream or leaked input: %d %s hits=%d", w.Code, w.Body.String(), hits.Load())
		}
	}
}

func TestToolsOnlyProfileToggleRequiresFreshManagedApproval(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(fmt.Sprint(initial), func(t *testing.T) {
			hits := toolsOnlyUpstream(t)
			db, err := sqlite.New("")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.Init(t.Context()); err != nil {
				t.Fatal(err)
			}
			s := NewServer(db, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
			s.SetConnectorRegistry(toolsOnlyRegistry(initial))
			s.SetConnectorSessionStore(connector.NewMemoryConnectorSessionStore(0))
			s.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow,
				MandatoryHITL: []policy.MandatoryHITLRule{{TenantID: "acme", AgentID: "agent", Connector: "fixture", Tools: []string{"write"}}},
			}))
			binding := &auth.RunBinding{RunID: "synthetic-run", Generation: 1, TenantID: "acme", AgentID: "agent"}
			seedManagedPendingFixture(t, db, binding)
			ctx := auth.WithRunBinding(tenant.WithContext(t.Context(), binding.TenantID), binding)
			token := toolsOnlyMint(t, s, ctx)
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write","arguments":{"amount":3}}}`
			id := pendingTargetFixture(t, toolsOnlyRequest(s, ctx, token, body))
			oldTarget := currentFixtureTargetRevision(t, s, "fixture")
			approve := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/admin-api/pending-actions/"+id+"/approve", nil)
				r = r.WithContext(auth.WithContext(r.Context(), &auth.AuthResult{Identity: "synthetic-reviewer", Permissions: []string{"admin"}}))
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				return w
			}
			s.SetConnectorRegistry(toolsOnlyRegistry(!initial))
			if w := approve(); w.Code != 409 {
				t.Fatalf("review accepted a changed tools-only profile: %d %s", w.Code, w.Body.String())
			}
			unapproved, err := db.GetPendingAction(ctx, id)
			if err != nil || unapproved.Status != models.PendingStatusPending {
				t.Fatalf("failed review changed pending authority: %+v %v", unapproved, err)
			}
			s.SetConnectorRegistry(toolsOnlyRegistry(initial))
			if w := approve(); w.Code != 200 {
				t.Fatalf("approve original profile: %d %s", w.Code, w.Body.String())
			}
			s.SetConnectorRegistry(toolsOnlyRegistry(!initial))
			if currentFixtureTargetRevision(t, s, "fixture") == oldTarget {
				t.Fatal("profile toggle retained prior target authority")
			}
			fresh := pendingTargetFixture(t, toolsOnlyRequest(s, ctx, token, body))
			old, err := db.GetPendingAction(ctx, id)
			if err != nil || fresh == id || old.Status != models.PendingStatusApproved || old.TargetRevision != oldTarget || hits.Load() != 0 {
				t.Fatalf("profile toggle reused approval or rewrote history: fresh=%q old=%+v err=%v hits=%d", fresh, old, err, hits.Load())
			}
		})
	}
}
