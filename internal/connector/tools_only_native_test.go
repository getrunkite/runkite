package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A real pinned SDK caller uses StreamableClientTransport over loopback HTTP.
// The JSON upstream is the existing simple-POST transport contract. The
// streamable upstream uses the real SDK handler, including its negotiation.
func TestToolsOnlyNativeSDKCallerFallsBackAndUsesTools(t *testing.T) {
	for _, mode := range []string{"json", "streamable_http"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var calls, upstreamRequests, proxyDiscovery, proxyInitialize atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "native-upstream", Version: "fixture"}, &mcp.ServerOptions{
				Capabilities: &mcp.ServerCapabilities{
					Tools: &mcp.ToolCapabilities{ListChanged: true}, Resources: &mcp.ResourceCapabilities{Subscribe: true},
					Prompts: &mcp.PromptCapabilities{}, Experimental: map[string]any{"fixture": true},
				},
			})
			server.AddTool(&mcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "native tool result"}}}, nil
			})
			server.AddTool(&mcp.Tool{Name: "delete", InputSchema: map[string]any{"type": "object"}}, nil)
			streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamRequests.Add(1)
				if r.Header.Get("Authorization") != "Bearer synthetic-native-fixture" {
					t.Error("native upstream did not receive configured authorization")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if mode == "streamable_http" {
					streamable.ServeHTTP(w, r)
					return
				}
				body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxMCPRequestBytes))
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				req, err := ParseMCPRequest(body)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if req.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if req.Method == "initialize" {
					_, _ = w.Write(toolsOnlyInitResult(req.ID, `{"tools":{"listChanged":true},"resources":{"subscribe":true},"prompts":{},"experimental":{"fixture":true}}`))
					return
				}
				var result any
				switch req.Method {
				case "tools/list":
					result = map[string]any{"tools": []any{
						map[string]any{"name": "read", "inputSchema": map[string]any{"type": "object"}},
						map[string]any{"name": "delete", "inputSchema": map[string]any{"type": "object"}},
					}}
				case "tools/call":
					if req.Tool != "read" {
						t.Error("native denied tool reached upstream")
					}
					calls.Add(1)
					result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "native tool result"}}}
				case "ping":
					result = map[string]any{}
				default:
					t.Error("native non-tool method reached upstream")
					w.WriteHeader(400)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer upstream.Close()
			previous := httpClient
			// Explicitly disable ambient proxy discovery in this owned fixture.
			transport := &http.Transport{Proxy: nil}
			defer transport.CloseIdleConnections()
			httpClient = &http.Client{Timeout: 5 * time.Second, Transport: transport,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			defer func() { httpClient = previous }()
			registry := NewRegistry(map[string]ConnectorConfig{"fixture": {
				Auth: AuthConfig{Type: "bearer", BearerToken: "synthetic-native-fixture"},
				MCP:  &MCPConfig{URL: upstream.URL, Transport: mode, ToolsOnly: true}, Tools: &ToolFilter{Allow: []string{"read"}},
			}})
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxMCPRequestBytes))
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				request, err := ParseMCPRequest(body)
				if err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if request.Method == "server/discover" {
					proxyDiscovery.Add(1)
					if upstreamRequests.Load() != 0 {
						t.Error("native discovery denial contacted upstream")
					}
				}
				if request.Method == "initialize" {
					proxyInitialize.Add(1)
				}
				result, err := registry.ProxyMCPRequest(r.Context(), "fixture", nil, body)
				if err != nil {
					t.Errorf("native proxy operation failed: %v", err)
					w.WriteHeader(502)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(result.StatusCode)
				_, _ = w.Write(result.Body)
			}))
			defer proxy.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "native-caller", Version: "fixture"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
			session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint: proxy.URL, HTTPClient: &http.Client{Timeout: 5 * time.Second, Transport: transport},
				MaxRetries: -1, DisableStandaloneSSE: true,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if proxyDiscovery.Load() != 1 || proxyInitialize.Load() != 1 {
				t.Fatal("native SDK did not take discovery-to-initialize fallback")
			}
			initJSON, _ := json.Marshal(session.InitializeResult())
			var initialized map[string]json.RawMessage
			_ = json.Unmarshal(initJSON, &initialized)
			if string(initialized["capabilities"]) != `{"tools":{}}` {
				t.Fatal("native SDK observed capabilities beyond tools")
			}
			listed, err := session.ListTools(ctx, nil)
			if err != nil || len(listed.Tools) != 1 || listed.Tools[0].Name != "read" {
				t.Fatalf("native tool discovery failed: %v", err)
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "read", Arguments: map[string]any{}})
			if err != nil || result.IsError || len(result.Content) != 1 || calls.Load() != 1 {
				t.Fatalf("native tool call failed: %v", err)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || text.Text != "native tool result" {
				t.Fatal("native tool result changed")
			}
			for _, method := range []string{"resources/list", "prompts/list"} {
				before := upstreamRequests.Load()
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 99, "method": method})
				denied, err := registry.ProxyMCPRequest(ctx, "fixture", nil, body)
				if err != nil || !bytes.Contains(denied.Body, []byte(`-32601`)) || upstreamRequests.Load() != before {
					t.Fatal("native profile allowed an advertised upstream non-tool method")
				}
			}
		})
	}
}
