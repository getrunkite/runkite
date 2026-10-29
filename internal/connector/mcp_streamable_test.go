package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type streamableTestTransport func(*http.Request) (*http.Response, error)

func (f streamableTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Use the real upstream SDK handler over an in-memory HTTP transport. This
// exercises negotiation, session headers and SSE decoding without local ports.
func TestStreamableConnectorNegotiatesAndFilters(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "modern", true: "legacy-session"}[legacy], func(t *testing.T) {
			var calls, initialized, sessionRequests, closed atomic.Int32
			server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "test"}, nil)
			server.AddTool(&mcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"owner": map[string]any{"type": "string", "x-mcp-header": "Owner"},
			}}}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				if !bytes.Contains(request.Params.Arguments, []byte(`9007199254740993`)) {
					t.Errorf("arguments changed before dispatch: %s", request.Params.Arguments)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "from upstream"}}}, nil
			})
			server.AddTool(&mcp.Tool{Name: "delete", InputSchema: map[string]any{"type": "object"}}, nil)
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
				&mcp.StreamableHTTPOptions{SessionTimeout: time.Minute, Stateless: !legacy})
			previous := httpClient
			t.Cleanup(func() { httpClient = previous })
			httpClient = &http.Client{Timeout: 2 * time.Second, Transport: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
				if got := r.Header.Get("Authorization"); got != "Bearer synthetic" {
					t.Errorf("outbound auth = %q", got)
				}
				var body []byte
				if r.Body != nil {
					body, _ = io.ReadAll(r.Body)
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
				}
				_ = json.Unmarshal(body, &request)
				recorder := httptest.NewRecorder()
				if legacy && request.Method == "server/discover" {
					recorder.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(recorder).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID,
						"error": map[string]any{"code": -32601, "message": "legacy server"}})
					return recorder.Result(), nil
				}
				if request.Method == "initialize" {
					initialized.Add(1)
				}
				if !legacy && request.Method == "tools/call" && r.Header.Get("Mcp-Param-Owner") != "example" {
					t.Errorf("missing schema-derived owner header: %v", r.Header)
				}
				if r.Header.Get("Mcp-Session-Id") != "" {
					sessionRequests.Add(1)
				}
				if r.Method == http.MethodDelete {
					closed.Add(1)
				}
				handler.ServeHTTP(recorder, r)
				return recorder.Result(), nil
			})}
			registry := NewRegistry(map[string]ConnectorConfig{"test": {
				Auth:  AuthConfig{Type: "bearer", BearerToken: "synthetic"},
				MCP:   &MCPConfig{URL: "https://upstream.invalid/mcp", Transport: "streamable_http"},
				Tools: &ToolFilter{Allow: []string{"read"}},
			}})
			response, err := registry.ProxyMCPRequest(context.Background(), "test", nil, toolsListRequest(7))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(response.Body), `"name":"delete"`) || !strings.Contains(string(response.Body), `"name":"read"`) {
				t.Fatalf("unfiltered list: %s", response.Body)
			}
			response, err = registry.ProxyMCPRequest(context.Background(), "test", nil, []byte(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"read","arguments":{"owner":"example","counter":9007199254740993}}}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(response.Body), "from upstream") || calls.Load() != 1 {
				t.Fatalf("call response %s, hits %d", response.Body, calls.Load())
			}
			_, err = registry.ProxyMCPRequest(context.Background(), "test", nil, toolCallRequest(10, "delete"))
			if err != nil || calls.Load() != 1 {
				t.Fatalf("denied tool forwarded: %v", err)
			}
			if legacy && (initialized.Load() != 2 || sessionRequests.Load() < 2 || closed.Load() != 2) {
				t.Fatalf("session lifecycle initialize=%d requests=%d close=%d", initialized.Load(), sessionRequests.Load(), closed.Load())
			}
		})
	}
}

func TestStreamableConnectorDoesNotReplayFailedTool(t *testing.T) {
	var calls atomic.Int32
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	httpClient = &http.Client{Transport: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
		var body []byte
		if r.Body != nil {
			body, _ = io.ReadAll(r.Body)
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Method == "tools/call" {
			calls.Add(1)
			return nil, errors.New("response lost after tool executed")
		}
		var result any
		switch req.Method {
		case "server/discover":
			result = map[string]any{"supportedVersions": []string{"2026-07-28"}, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "read", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			result = map[string]any{}
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(encoded))}, nil
	})}
	registry := NewRegistry(map[string]ConnectorConfig{"test": {Auth: AuthConfig{Type: "api_key", APIKey: "synthetic"}, MCP: &MCPConfig{URL: "https://upstream.invalid/mcp", Transport: "streamable_http"}}})
	response, err := registry.ProxyMCPRequest(context.Background(), "test", nil, toolCallRequest(1, "read"))
	var rpcResponse jsonRPCResponse
	if response != nil {
		_ = json.Unmarshal(response.Body, &rpcResponse)
	}
	if (err == nil && rpcResponse.Error == nil) || calls.Load() != 1 {
		t.Fatalf("error=%v rpcError=%v calls=%d", err, rpcResponse.Error, calls.Load())
	}
}

func TestFilteredToolsPreservePaginationAndExtensions(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":"original","result":{"tools":[{"name":"read"},{"name":"delete"}],"nextCursor":"page-2","_meta":{"source":"upstream"}}}`)
	filtered := filterToolsListResponse(body, &ToolFilter{Allow: []string{"read"}})
	if strings.Contains(string(filtered), `"name":"delete"`) || !strings.Contains(string(filtered), `"nextCursor":"page-2"`) || !strings.Contains(string(filtered), `"source":"upstream"`) {
		t.Fatalf("lost pagination or extension fields: %s", filtered)
	}
}

func TestStreamableResponseBodyCap(t *testing.T) {
	reader := &mcpLimitedBody{ReadCloser: io.NopCloser(strings.NewReader("four")), remaining: 3}
	if _, err := io.ReadAll(reader); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("overflow error = %v", err)
	}
	if _, err := reader.Read(make([]byte, 1)); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("repeated overflow error = %v", err)
	}
}
