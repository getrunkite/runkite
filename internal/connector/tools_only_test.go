package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const toolsOnlyInitialize = `{"jsonrpc":"2.0","id":9007199254740993,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"fixture","version":"test"},"capabilities":{"roots":{"listChanged":true},"sampling":{},"elicitation":{},"experimental":{"fixture":true}}}}`
const toolsOnlyInitialized = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

func toolsOnlyInitResult(id json.RawMessage, capabilities string) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"fixture","version":"test"},"instructions":"public fixture instructions","capabilities":` + capabilities + `,"_meta":{"precise":9007199254740993}}}`)
}

func toolsOnlyTestRegistry(mode string, auth AuthConfig) *Registry {
	return NewRegistry(map[string]ConnectorConfig{"fixture": {
		Auth: auth, MCP: &MCPConfig{URL: "https://upstream.invalid/mcp", Transport: mode, ToolsOnly: true},
		CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: 1, CooldownSeconds: 60},
	}})
}

func TestToolsOnlyDeniedMethodsHaveNoCredentialOrUpstreamActivity(t *testing.T) {
	for _, mode := range []string{"json", "streamable_http"} {
		for _, authType := range []string{"oauth2_client_credentials", "secret_ref", "vault_database"} {
			t.Run(mode+"/"+authType, func(t *testing.T) {
				previous := httpClient
				t.Cleanup(func() { httpClient = previous })
				var requests atomic.Int32
				httpClient = &http.Client{Transport: streamableTestTransport(func(*http.Request) (*http.Response, error) {
					requests.Add(1)
					return nil, errors.New("unexpected credential or MCP activity")
				})}
				auth := AuthConfig{Type: "oauth2_client_credentials", TokenURL: "https://issuer.invalid/token", ClientID: "fixture", ClientSecret: "synthetic-secret"}
				switch authType {
				case "secret_ref":
					auth = AuthConfig{Type: "bearer", SecretRef: "file:" + filepath.Join(t.TempDir(), "absent")}
				case "vault_database":
					auth = AuthConfig{Type: "vault_database", CredentialBinding: "fixture", CredentialBindingRevision: "revision"}
				}
				registry := toolsOnlyTestRegistry(mode, auth)
				c, _ := registry.Get("fixture")
				c.Config.MCP.PinTools = true
				// A denied method must not consult a breaker that is already open.
				c.breaker.RecordFailure()
				for _, method := range []string{
					"server/discover", "resources/list", "resources/templates/list", "resources/read", "resources/subscribe",
					"resources/unsubscribe", "prompts/list", "prompts/get", "completion/complete", "logging/setLevel",
					"subscriptions/listen", "tasks/get", "notifications/cancelled", "notifications/progress",
					"tools/call ", "TOOLS/CALL", "unknown",
				} {
					body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "opaque-id", "method": method, "params": map[string]any{"uri": "synthetic-sensitive-uri"}})
					result, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, body)
					if err != nil || result.StatusCode != http.StatusOK {
						t.Fatalf("denied method did not return a local RPC error: %v", err)
					}
					var response struct {
						ID    string `json:"id"`
						Error struct {
							Code int               `json:"code"`
							Data map[string]string `json:"data"`
						} `json:"error"`
					}
					if json.Unmarshal(result.Body, &response) != nil || response.ID != "opaque-id" || response.Error.Code != -32601 || response.Error.Data["reason_code"] != "connector_mcp_method_denied" {
						t.Fatal("method denial lost its stable RPC identity")
					}
					if bytes.Contains(result.Body, []byte("synthetic-sensitive-uri")) || bytes.Contains(result.Body, []byte("synthetic-secret")) {
						t.Fatal("method denial copied sensitive request/config data")
					}
				}
				result, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"other"}}`))
				if err != nil || result.StatusCode != http.StatusAccepted || len(result.Body) != 0 {
					t.Fatal("denied notification generated a JSON-RPC response")
				}
				if requests.Load() != 0 || registry.BreakerState("fixture") != "open" {
					t.Fatal("denied method touched credentials, upstream, or breaker admission")
				}
			})
		}
	}
}

func TestToolsOnlyJSONInitializationClearsBothCapabilityDirections(t *testing.T) {
	previous := httpClient
	t.Cleanup(func() { httpClient = previous })
	calls := 0
	httpClient = &http.Client{Transport: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		var request struct {
			ID     json.RawMessage `json:"id"`
			Params struct {
				Capabilities map[string]json.RawMessage `json:"capabilities"`
				ClientInfo   map[string]string          `json:"clientInfo"`
			} `json:"params"`
		}
		if json.Unmarshal(raw, &request) != nil || len(request.Params.Capabilities) != 0 || request.Params.Capabilities == nil || request.Params.ClientInfo["name"] != "fixture" {
			t.Fatal("caller capabilities survived or unrelated initialization fields changed")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(toolsOnlyInitResult(request.ID,
			`{"tools":{"listChanged":true,"unknown":true},"resources":{"subscribe":true},"prompts":{},"logging":{},"completions":{},"experimental":{"unknown":true},"extensions":{"vendor/extension":{}}}`)))}, nil
	})}
	registry := toolsOnlyTestRegistry("json", AuthConfig{Type: "bearer", BearerToken: "synthetic"})
	result, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, []byte(toolsOnlyInitialize))
	if err != nil || result.StatusCode != 200 || calls != 1 {
		t.Fatalf("initialization failed: %v", err)
	}
	assertToolsOnlyInitialization(t, result.Body, true)
	if !bytes.Contains(result.Body, []byte(`9007199254740993`)) || !bytes.Contains(result.Body, []byte("public fixture instructions")) {
		t.Fatal("sanitization changed precise metadata, ID or instructions")
	}
}

func assertToolsOnlyInitialization(t *testing.T, raw []byte, tools bool) {
	t.Helper()
	var response struct {
		Result struct {
			Capabilities map[string]json.RawMessage `json:"capabilities"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Result.Capabilities == nil {
		t.Fatal("initialization result has no capability object")
	}
	caps := response.Result.Capabilities
	if tools {
		if len(caps) != 1 || string(caps["tools"]) != "{}" {
			t.Fatal("tools-only initialization advertised unsupported capabilities")
		}
	} else if len(caps) != 0 {
		t.Fatal("tools capability was fabricated for an upstream without tools")
	}
}

func TestToolsOnlyJSONInitializationRejectsAmbiguityBeforeCredentials(t *testing.T) {
	for _, params := range []string{
		`{"Capabilities":{}}`, `{"capabilities":{},"CAPABILITIES":{}}`, `{"capabilities":null}`,
		`{"capabilities":[]}`, `{"capabilities":{},"ProtocolVersion":"2025-11-25"}`,
		`{"capabilities":{},"ClientInfo":{"name":"fixture","version":"test"}}`,
	} {
		registry := toolsOnlyTestRegistry("json", AuthConfig{Type: "bearer", SecretRef: "file:" + filepath.Join(t.TempDir(), "absent")})
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":` + params + `}`)
		if _, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, body); !errors.Is(err, ErrMCPInitializationInvalid) {
			t.Fatalf("initialization alias/type error reached credential resolution: %v", err)
		}
	}
}

func TestToolsOnlyJSONInitializationResponseFailuresStayDistinct(t *testing.T) {
	valid := string(toolsOnlyInitResult(json.RawMessage(`9007199254740993`), `{"tools":{}}`))
	cases := map[string]string{
		"result_and_error":       strings.TrimSuffix(valid, "}") + `,"error":{"code":-32601,"message":"synthetic-sensitive"}}`,
		"duplicate_capabilities": strings.Replace(valid, `"capabilities":`, `"capabilities":{},"capabilities":`, 1),
		"alias_capabilities":     strings.Replace(valid, `"capabilities":`, `"Capabilities":`, 1),
		"alias_tools":            strings.Replace(valid, `"tools":`, `"Tools":`, 1),
		"null_tools":             strings.Replace(valid, `"tools":{}`, `"tools":null`, 1),
		"wrong_id":               strings.Replace(valid, `"id":9007199254740993`, `"id":1`, 1),
		"request_and_response":   strings.TrimSuffix(valid, "}") + `,"method":"ping"}`,
		"invalid_error":          `{"jsonrpc":"2.0","id":9007199254740993,"error":{"code":null,"message":"synthetic-sensitive"}}`,
		"empty":                  "", "batch": "[" + valid + "]",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			previous := httpClient
			t.Cleanup(func() { httpClient = previous })
			httpClient = &http.Client{Transport: streamableTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(raw))}, nil
			})}
			registry := toolsOnlyTestRegistry("json", AuthConfig{Type: "bearer", BearerToken: "synthetic"})
			result, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, []byte(toolsOnlyInitialize))
			if result != nil || !errors.Is(err, ErrMCPInitializationUnverified) || strings.Contains(err.Error(), "synthetic-sensitive") {
				t.Fatal("unverified initialization response did not fail closed with a fixed error")
			}
		})
	}
	for _, status := range []int{200, 403, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			previous := httpClient
			t.Cleanup(func() { httpClient = previous })
			raw := `{"jsonrpc":"2.0","id":9007199254740993,"error":{"code":-32601,"message":"upstream rejected initialization"}}`
			if status != 200 {
				raw = `synthetic HTTP failure`
			}
			httpClient = &http.Client{Transport: streamableTestTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(raw))}, nil
			})}
			registry := toolsOnlyTestRegistry("json", AuthConfig{Type: "bearer", BearerToken: "synthetic"})
			result, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, []byte(toolsOnlyInitialize))
			if err != nil || result.StatusCode != status || string(result.Body) != raw {
				t.Fatal("valid RPC or HTTP failure was confused with a successful initialization")
			}
		})
	}
}

func TestToolsOnlyDoesNotFabricateToolsAndOffPreservesProxyBehavior(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			previous := httpClient
			t.Cleanup(func() { httpClient = previous })
			response := toolsOnlyInitResult(json.RawMessage(`9007199254740993`), `{"resources":{},"extensions":{"vendor/extension":{}}}`)
			httpClient = &http.Client{Transport: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				if !enabled && !bytes.Contains(body, []byte(`"sampling"`)) {
					t.Fatal("disabled profile changed caller capabilities")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(response))}, nil
			})}
			registry := toolsOnlyTestRegistry("json", AuthConfig{Type: "bearer", BearerToken: "synthetic"})
			c, _ := registry.Get("fixture")
			c.Config.MCP.ToolsOnly = enabled
			result, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, []byte(toolsOnlyInitialize))
			if err != nil {
				t.Fatal(err)
			}
			if enabled {
				assertToolsOnlyInitialization(t, result.Body, false)
			} else if !bytes.Equal(result.Body, response) {
				t.Fatal("disabled profile changed upstream bytes")
			}
		})
	}
}

func TestToolsOnlyStreamableSDKInitializationToolsAndPinning(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "upstream", Version: "fixture"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{
				Tools: &mcp.ToolCapabilities{ListChanged: true}, Resources: &mcp.ResourceCapabilities{Subscribe: true}, Prompts: &mcp.PromptCapabilities{},
				Experimental: map[string]any{"fixture": true},
			}})
			var calls atomic.Int32
			server.AddTool(&mcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				if !bytes.Contains(request.Params.Arguments, []byte(`9007199254740993`)) {
					t.Error("precise arguments changed")
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "fixture result"}}}, nil
			})
			server.AddTool(&mcp.Tool{Name: "delete", InputSchema: map[string]any{"type": "object"}}, nil)
			handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: !legacy, SessionTimeout: time.Minute})
			previous := httpClient
			t.Cleanup(func() { httpClient = previous })
			var upstreamRequests atomic.Int32
			httpClient = &http.Client{Transport: streamableTestTransport(func(r *http.Request) (*http.Response, error) {
				upstreamRequests.Add(1)
				var body []byte
				if r.Body != nil {
					body, _ = io.ReadAll(r.Body)
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				var request struct {
					Method string                     `json:"method"`
					ID     json.RawMessage            `json:"id"`
					Params map[string]json.RawMessage `json:"params"`
				}
				_ = json.Unmarshal(body, &request)
				if r.Header.Get("Authorization") != "Bearer synthetic" {
					t.Error("upstream auth changed")
				}
				recorder := httptest.NewRecorder()
				if legacy && request.Method == "server/discover" {
					recorder.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(recorder, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"legacy fixture"}}`, request.ID)
				} else {
					handler.ServeHTTP(recorder, r)
				}
				return recorder.Result(), nil
			})}
			registry := toolsOnlyTestRegistry("streamable_http", AuthConfig{Type: "bearer", BearerToken: "synthetic"})
			c, _ := registry.Get("fixture")
			c.Config.Tools = &ToolFilter{Allow: []string{"read"}}
			c.Config.MCP.PinTools = true
			result, err := registry.ProxyMCPRequest(context.Background(), "fixture", nil, []byte(toolsOnlyInitialize))
			if err != nil {
				t.Fatal(err)
			}
			assertToolsOnlyInitialization(t, result.Body, true)
			result, err = registry.ProxyMCPRequest(context.Background(), "fixture", nil, []byte(toolsOnlyInitialized))
			if err != nil || result.StatusCode != 202 || len(result.Body) != 0 {
				t.Fatal("allowed initialized notification changed")
			}
			result, err = registry.ProxyMCPRequest(context.Background(), "fixture", nil, toolsListRequest(1))
			if err != nil || bytes.Contains(result.Body, []byte(`"delete"`)) || !bytes.Contains(result.Body, []byte(`"read"`)) {
				t.Fatal("tool list filtering changed")
			}
			ctx, _, err := registry.BindMCPRequestTarget(context.Background(), "fixture")
			if err != nil {
				t.Fatal(err)
			}
			ctx, revision, err := registry.BindMCPToolRevision(ctx, "fixture", "read", nil)
			if err != nil || revision == "" {
				t.Fatal("tool definition pinning failed")
			}
			result, err = registry.ProxyMCPRequest(ctx, "fixture", nil, []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read","arguments":{"precise":9007199254740993}}}`))
			if err != nil || calls.Load() != 1 || !bytes.Contains(result.Body, []byte("fixture result")) {
				t.Fatalf("pinned tool call failed: %v", err)
			}
			before := upstreamRequests.Load()
			result, err = registry.ProxyMCPRequest(context.Background(), "fixture", nil, toolCallRequest(5, "delete"))
			if err != nil || calls.Load() != 1 || upstreamRequests.Load() != before || !bytes.Contains(result.Body, []byte(`"error"`)) {
				t.Fatal("denied tool reached upstream")
			}
		})
	}
}

func TestToolsOnlyInitializationPreservesOtherMetadataForClientValidation(t *testing.T) {
	// Capability fencing does not impose unrelated implementation/version or
	// instructions schema rules. The caller's MCP client owns those checks.
	raw := []byte(`{"jsonrpc":"2.0","id":1,"result":{"capabilities":{"tools":{},"resources":{}},"protocolVersion":"","serverInfo":{"name":"fixture","version":""},"instructions":{"precise":9007199254740993}}}`)
	filtered, err := toolsOnlyInitializationResponse(raw, json.RawMessage(`1`))
	if err != nil {
		t.Fatal(err)
	}
	assertToolsOnlyInitialization(t, filtered, true)
	var before, after map[string]json.RawMessage
	var beforeResult, afterResult map[string]json.RawMessage
	_ = json.Unmarshal(raw, &before)
	_ = json.Unmarshal(filtered, &after)
	_ = json.Unmarshal(before["result"], &beforeResult)
	_ = json.Unmarshal(after["result"], &afterResult)
	for _, field := range []string{"protocolVersion", "serverInfo", "instructions"} {
		if !bytes.Equal(beforeResult[field], afterResult[field]) {
			t.Fatal("unrelated initialization metadata changed")
		}
	}
}
