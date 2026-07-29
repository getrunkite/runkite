package connector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// fakeMCPServer is a minimal stand-in for a real downstream MCP server --
// just enough JSON-RPC 2.0 surface (tools/list, tools/call, and a generic
// method) to prove the proxy's forwarding, gating, and filtering logic
// against something that behaves like the real thing, not a mock that
// just echoes back whatever the proxy sends it.
type fakeMCPServer struct {
	*httptest.Server
	toolsCallHits atomic.Int32 // how many times tools/call actually reached this server
	lastAuthHdr   atomic.Value // string
}

func newFakeMCPServer(t *testing.T) *fakeMCPServer {
	t.Helper()
	f := &fakeMCPServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.lastAuthHdr.Store(r.Header.Get("Authorization"))

		var req jsonRPCRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "tools/list":
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result": map[string]interface{}{
					"tools": []map[string]string{
						{"name": "query"},
						{"name": "read"},
						{"name": "delete"},
						{"name": "bulkUpdate"},
					},
				},
			}
			json.NewEncoder(w).Encode(resp)
		case "tools/call":
			f.toolsCallHits.Add(1)
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]interface{}{"content": []interface{}{map[string]string{"type": "text", "text": "ok"}}},
			}
			json.NewEncoder(w).Encode(resp)
		default:
			resp := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(req.ID),
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func toolCallRequest(id int, toolName string) []byte {
	req := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": toolName, "arguments": map[string]interface{}{}},
	}
	b, _ := json.Marshal(req)
	return b
}

func toolsListRequest(id int) []byte {
	req := map[string]interface{}{"jsonrpc": "2.0", "id": id, "method": "tools/list"}
	b, _ := json.Marshal(req)
	return b
}

// TestMCPProxy_AllowedToolCall_ReachesDownstream proves the common,
// happy-path case: an allowed tool call is forwarded, and the
// downstream's real response comes back through unchanged.
func TestMCPProxy_AllowedToolCall_ReachesDownstream(t *testing.T) {
	fake := newFakeMCPServer(t)
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth:  AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:   &MCPConfig{URL: fake.URL},
			Tools: &ToolFilter{Allow: []string{"query", "read"}},
		},
	})

	result, err := r.ProxyMCPRequest(context.Background(), "sf", nil, toolCallRequest(1, "query"))
	if err != nil {
		t.Fatalf("ProxyMCPRequest failed: %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", result.StatusCode)
	}
	if fake.toolsCallHits.Load() != 1 {
		t.Fatalf("expected downstream to be hit exactly once, got %d", fake.toolsCallHits.Load())
	}

	var resp jsonRPCResponse
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("response not valid JSON-RPC: %v (%s)", err, result.Body)
	}
	if resp.Error != nil {
		t.Fatalf("expected no error for an allowed tool, got %+v", resp.Error)
	}
}

// TestMCPProxy_DeniedToolCall_NeverReachesDownstream is the core security
// property this whole proxy exists for (plans/pending_items.md item 17):
// a denied tool call must be rejected WITHOUT the downstream server ever
// seeing it -- not just filtered out of an advisory list the caller could
// choose to ignore.
func TestMCPProxy_DeniedToolCall_NeverReachesDownstream(t *testing.T) {
	fake := newFakeMCPServer(t)
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth:  AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:   &MCPConfig{URL: fake.URL},
			Tools: &ToolFilter{Allow: []string{"query", "read"}},
		},
	})

	result, err := r.ProxyMCPRequest(context.Background(), "sf", nil, toolCallRequest(2, "delete"))
	if err != nil {
		t.Fatalf("ProxyMCPRequest should return a JSON-RPC error body, not a Go error: %v", err)
	}
	if fake.toolsCallHits.Load() != 0 {
		t.Fatalf("downstream MCP server was contacted for a denied tool call -- got %d hits, want 0", fake.toolsCallHits.Load())
	}

	var resp jsonRPCResponse
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("response not valid JSON-RPC: %v (%s)", err, result.Body)
	}
	if resp.Error == nil {
		t.Fatal("expected a JSON-RPC error for a denied tool call")
	}
	if resp.Error.Code != toolNotAllowedCode {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, toolNotAllowedCode)
	}
}

// TestMCPProxy_DenyOnlyFilter_ToolsList_FilteredCorrectly proves the fix
// for allowedTools()'s static-preview limitation: a deny-only filter
// (no allow list) can't be represented without knowing the real tool
// universe, but the proxy CAN apply it correctly because it filters the
// downstream server's own real tools/list response.
func TestMCPProxy_DenyOnlyFilter_ToolsList_FilteredCorrectly(t *testing.T) {
	fake := newFakeMCPServer(t)
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth:  AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:   &MCPConfig{URL: fake.URL},
			Tools: &ToolFilter{Deny: []string{"delete", "bulkUpdate"}},
		},
	})

	result, err := r.ProxyMCPRequest(context.Background(), "sf", nil, toolsListRequest(3))
	if err != nil {
		t.Fatalf("ProxyMCPRequest failed: %v", err)
	}

	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("response not valid JSON: %v (%s)", err, result.Body)
	}

	got := map[string]bool{}
	for _, tool := range resp.Result.Tools {
		got[tool.Name] = true
	}
	if !got["query"] || !got["read"] {
		t.Fatalf("expected query and read to survive a deny-only filter, got %v", got)
	}
	if got["delete"] || got["bulkUpdate"] {
		t.Fatalf("expected delete and bulkUpdate to be filtered out, got %v", got)
	}
}

// TestMCPProxy_AllowListFilter_ToolsList_FilteredCorrectly is the same
// check for an allow-list filter, confirmed against the real downstream
// list rather than the static allowedTools() preview.
func TestMCPProxy_AllowListFilter_ToolsList_FilteredCorrectly(t *testing.T) {
	fake := newFakeMCPServer(t)
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth:  AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:   &MCPConfig{URL: fake.URL},
			Tools: &ToolFilter{Allow: []string{"query"}},
		},
	})

	result, err := r.ProxyMCPRequest(context.Background(), "sf", nil, toolsListRequest(4))
	if err != nil {
		t.Fatalf("ProxyMCPRequest failed: %v", err)
	}

	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal(result.Body, &resp)
	if len(resp.Result.Tools) != 1 || resp.Result.Tools[0].Name != "query" {
		t.Fatalf("expected exactly [query], got %v", resp.Result.Tools)
	}
}

// TestMCPProxy_NonToolMethod_PassedThroughTransparently proves methods
// other than tools/call and tools/list (e.g. initialize) aren't touched
// at all -- this proxy only intercepts the two methods where enforcement
// matters, not a full protocol reimplementation.
func TestMCPProxy_NonToolMethod_PassedThroughTransparently(t *testing.T) {
	fake := newFakeMCPServer(t)
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth: AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:  &MCPConfig{URL: fake.URL},
		},
	})

	req := map[string]interface{}{"jsonrpc": "2.0", "id": 5, "method": "initialize"}
	b, _ := json.Marshal(req)
	result, err := r.ProxyMCPRequest(context.Background(), "sf", nil, b)
	if err != nil {
		t.Fatalf("ProxyMCPRequest failed: %v", err)
	}
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	json.Unmarshal(result.Body, &resp)
	if resp.Result.ProtocolVersion != "2024-11-05" {
		t.Fatalf("expected the downstream's real response to pass through unchanged, got %s", result.Body)
	}
}

// TestMCPProxy_NoFilter_AllToolsAllowed proves a connector with no Tools
// filter at all lets everything through, matching isAllowed's own
// "no filter = all allowed" contract.
func TestMCPProxy_NoFilter_AllToolsAllowed(t *testing.T) {
	fake := newFakeMCPServer(t)
	r := NewRegistry(map[string]ConnectorConfig{
		"open": {Auth: AuthConfig{Type: "api_key", APIKey: "k"}, MCP: &MCPConfig{URL: fake.URL}},
	})

	result, err := r.ProxyMCPRequest(context.Background(), "open", nil, toolCallRequest(6, "delete"))
	if err != nil {
		t.Fatalf("ProxyMCPRequest failed: %v", err)
	}
	if fake.toolsCallHits.Load() != 1 {
		t.Fatalf("expected the call to reach downstream with no filter configured, got %d hits", fake.toolsCallHits.Load())
	}
	var resp jsonRPCResponse
	json.Unmarshal(result.Body, &resp)
	if resp.Error != nil {
		t.Fatalf("expected no error with no filter configured, got %+v", resp.Error)
	}
}

// TestMCPProxy_InjectsConnectorCredentials proves the runner never needs
// to see or forward raw downstream credentials -- the proxy injects the
// connector's own token as the Authorization header on the downstream
// request.
func TestMCPProxy_InjectsConnectorCredentials(t *testing.T) {
	fake := newFakeMCPServer(t)
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {Auth: AuthConfig{Type: "bearer", BearerToken: "sf-secret-token"}, MCP: &MCPConfig{URL: fake.URL}},
	})

	if _, err := r.ProxyMCPRequest(context.Background(), "sf", nil, toolsListRequest(7)); err != nil {
		t.Fatalf("ProxyMCPRequest failed: %v", err)
	}
	got, _ := fake.lastAuthHdr.Load().(string)
	if got != "Bearer sf-secret-token" {
		t.Fatalf("Authorization header = %q, want the connector's own bearer token", got)
	}
}

// TestMCPProxy_UnknownConnector and TestMCPProxy_NoMCPConfigured cover
// the two ways a proxy request can fail before ever reaching a
// downstream server.
func TestMCPProxy_UnknownConnector(t *testing.T) {
	r := NewRegistry(nil)
	_, err := r.ProxyMCPRequest(context.Background(), "nope", nil, toolsListRequest(8))
	if err == nil {
		t.Fatal("expected error for unknown connector")
	}
}

func TestMCPProxy_NoMCPConfigured(t *testing.T) {
	r := NewRegistry(map[string]ConnectorConfig{
		"nomc": {Auth: AuthConfig{Type: "api_key", APIKey: "k"}},
	})
	_, err := r.ProxyMCPRequest(context.Background(), "nomc", nil, toolsListRequest(9))
	if err == nil {
		t.Fatal("expected error for a connector with no MCP endpoint configured")
	}
}

// TestMCPProxy_CircuitOpen_RejectsWithoutNetworkCall proves the proxy
// reuses the connector's existing circuit breaker (the same one guarding
// token fetches) -- a connector already known to be unhealthy fails fast
// on MCP calls too, instead of adding its own unguarded network path.
func TestMCPProxy_CircuitOpen_RejectsWithoutNetworkCall(t *testing.T) {
	fake := newFakeMCPServer(t)
	r := NewRegistry(map[string]ConnectorConfig{
		"sf": {
			Auth:           AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:            &MCPConfig{URL: fake.URL},
			CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: 1, CooldownSeconds: 3600},
		},
	})
	c, _ := r.Get("sf")
	c.breaker.RecordFailure() // one failure trips a threshold-1 breaker open

	_, err := r.ProxyMCPRequest(context.Background(), "sf", nil, toolsListRequest(10))
	var circuitOpen *ErrCircuitOpen
	if !errors.As(err, &circuitOpen) {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
}
