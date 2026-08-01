package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/transport"
)

// mcpConnect connects an MCP client to env's /mcp endpoint, optionally
// setting an Authorization header on every request (nil for none) --
// the same headerRoundTripper pattern any real MCP client config (e.g.
// Claude Desktop's static per-server API key) would need.
func mcpConnect(t *testing.T, baseURL string, headers map[string]string) *mcp.ClientSession {
	t.Helper()
	httpClient := &http.Client{Transport: &headerRoundTripper{headers: headers, base: http.DefaultTransport}}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: baseURL + "/mcp", HTTPClient: httpClient}
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.base.RoundTrip(req)
}

// runMCPAgentInBackground dequeues the next python-langgraph job and
// completes it with a single AI reply, mirroring every other test in
// this package that simulates a runner (e.g. TestTS002's own goroutine).
func runMCPAgentInBackground(t *testing.T, env *testEnv, reply string) {
	t.Helper()
	go func() {
		ctx := context.Background()
		assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		data, _ := json.Marshal(map[string]interface{}{
			"messages": []map[string]interface{}{{"role": "ai", "content": reply}},
		})
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_1", Seq: 1, Method: "values",
			Namespace: []string{}, Data: data, Ts: time.Now().UnixMilli(),
		})
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_2", Seq: 2, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()
}

// TestMCP_ToolsListReflectsConfiguredAgents proves every registered
// agent shows up as exactly one MCP tool, named after the agent_id,
// with the agent's own description carried through when set, and a
// generic fallback description when it isn't (seedAgent's helper
// doesn't set Description at all, so "summarizer" below exercises that
// fallback path for free).
func TestMCP_ToolsListReflectsConfiguredAgents(t *testing.T) {
	env := newTestEnv(t)
	if err := env.store.UpsertAgent(context.Background(), &models.Agent{
		AgentID: "chatbot", Name: "chatbot", Description: "A friendly chatbot agent.",
	}); err != nil {
		t.Fatal(err)
	}
	seedAgent(t, env, "summarizer", "summarizer", nil)

	session := mcpConnect(t, env.srv.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tools := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	if tools["chatbot"] == nil || tools["summarizer"] == nil {
		t.Fatalf("expected tools for both configured agents, got %+v", tools)
	}
	if tools["chatbot"].Description != "A friendly chatbot agent." {
		t.Fatalf("expected the agent's own description to be carried through, got %q", tools["chatbot"].Description)
	}
	if tools["summarizer"].Description == "" {
		t.Fatal("expected a non-empty fallback description for an agent with none set")
	}
}

// TestMCP_ToolCallDispatchesRealRunAndReturnsResult proves calling an
// agent's MCP tool actually dispatches a real Runkite run through the
// same path any other client-facing create-and-wait call uses, and
// returns the agent's final reply as the tool's text content.
func TestMCP_ToolCallDispatchesRealRunAndReturnsResult(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "chatbot", "chatbot", nil)
	runMCPAgentInBackground(t, env, "Hello from the agent")

	session := mcpConnect(t, env.srv.URL, nil)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "chatbot",
		Arguments: map[string]any{"message": "hi there"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected a successful result, got error content: %+v", res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected exactly one content item, got %d: %+v", len(res.Content), res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	if text.Text != "Hello from the agent" {
		t.Fatalf("expected the agent's reply, got %q", text.Text)
	}
}

// TestMCP_ToolCallMissingMessageAndInputIsToolError proves a call with
// neither "message" nor "input" is reported as a TOOL-level error (per
// mcp.Tool.IsError's own contract -- visible to the calling LLM so it
// can self-correct), not a raw MCP protocol error.
func TestMCP_ToolCallMissingMessageAndInputIsToolError(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "chatbot", "chatbot", nil)

	session := mcpConnect(t, env.srv.URL, nil)

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "chatbot",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool transport-level error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true for a call with neither message nor input, got %+v", res)
	}
}

// TestMCP_ToolCallUnknownAgentIsProtocolError proves calling a tool name
// that doesn't correspond to any configured agent is rejected by the
// SDK itself (unknown tool name), not silently dispatched.
func TestMCP_ToolCallUnknownAgentIsProtocolError(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "chatbot", "chatbot", nil)

	session := mcpConnect(t, env.srv.URL, nil)

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "never-registered-agent",
		Arguments: map[string]any{"message": "hi"},
	})
	if err == nil {
		t.Fatal("expected an error calling an unregistered tool name")
	}
}

// TestMCP_RawInputOverridesMessage proves a caller that already knows
// the target agent's own expected input shape (the "input" escape
// hatch) gets it passed through verbatim, not wrapped as a message.
func TestMCP_RawInputOverridesMessage(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "chatbot", "chatbot", nil)

	var capturedInput json.RawMessage
	go func() {
		ctx := context.Background()
		assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		capturedInput = assignment.Input
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_1", Seq: 1, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()

	session := mcpConnect(t, env.srv.URL, nil)
	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "chatbot",
		Arguments: map[string]any{
			"message": "this should be ignored",
			"input":   map[string]any{"custom_field": "custom_value"},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let the goroutine above capture the input
	var decoded map[string]interface{}
	if err := json.Unmarshal(capturedInput, &decoded); err != nil {
		t.Fatalf("captured input is not valid JSON: %v (%s)", err, capturedInput)
	}
	if decoded["custom_field"] != "custom_value" {
		t.Fatalf("expected raw input to be passed through verbatim, got %v", decoded)
	}
}

// TestMCP_SessionHijackAcrossTenantsIsRejected is a regression test for
// a real, live-confirmed gap: the MCP SDK's own session-hijack
// protection keys off an OAuth-specific field Runkite's auth providers
// never populate, so a SECOND, entirely different but otherwise valid
// credential presenting the FIRST session's leaked or guessed
// Mcp-Session-Id must not be allowed to continue that session --
// especially not under the ORIGINAL (wrong) tenant.
func TestMCP_SessionHijackAcrossTenantsIsRejected(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "chatbot", "chatbot", nil)

	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"tenant-a-key": {Name: "Tenant A User", Permissions: []string{"read", "write"}, TenantID: "tenant-a"},
		"tenant-b-key": {Name: "Tenant B User", Permissions: []string{"read", "write"}, TenantID: "tenant-b"},
	})
	env.srv.Close()
	env.srv = httptest.NewServer(auth.Middleware(provider, nil, nil, env.apiServer.Handler()))
	t.Cleanup(env.srv.Close)

	// Tenant A establishes a real session and gets a real session ID.
	sessionA := mcpConnect(t, env.srv.URL, map[string]string{"Authorization": "Bearer tenant-a-key"})
	hijackedID := sessionA.ID()
	if hijackedID == "" {
		t.Fatal("expected a non-empty session ID after connecting")
	}

	// Tenant B presents its OWN, otherwise perfectly valid credential,
	// but tenant A's session ID, with a well-formed tools/list request
	// -- must be rejected outright, not silently continue tenant A's
	// session (a malformed/empty body would also get a non-200, but for
	// the wrong reason -- this proves the SESSION check specifically).
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	req, _ := http.NewRequest(http.MethodPost, env.srv.URL+"/mcp", body)
	req.Header.Set("Authorization", "Bearer tenant-b-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", hijackedID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("hijack attempt request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a session hijack attempt across tenants, got %d: %s", resp.StatusCode, readBody(t, resp))
	}

	// Tenant A's own, legitimate continued use of its OWN session must
	// still work -- this isn't a blanket "reject known sessions" bug.
	if _, err := sessionA.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("tenant A's own session should still work: %v", err)
	}
}

// TestMCP_RequiresAuthWhenConfigured proves /mcp goes through the exact
// same client auth middleware as every other client-facing route --
// no separate or missing auth path for this endpoint.
func TestMCP_RequiresAuthWhenConfigured(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "chatbot", "chatbot", nil)

	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"test-key": {Name: "Tester", Permissions: []string{"read", "write"}},
	})
	env.srv.Close()
	env.srv = httptest.NewServer(auth.Middleware(provider, nil, nil, env.apiServer.Handler()))
	t.Cleanup(env.srv.Close)

	// No Authorization header at all -- the initial POST (MCP's
	// "initialize" call) must be rejected before ever reaching the tool
	// registration/listing logic.
	resp, err := http.Post(env.srv.URL+"/mcp", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /mcp without auth: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for /mcp with no credentials, got %d", resp.StatusCode)
	}

	// With a valid key, the full MCP handshake + ListTools succeeds.
	session := mcpConnect(t, env.srv.URL, map[string]string{"Authorization": "Bearer test-key"})
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools with valid auth: %v", err)
	}
}
