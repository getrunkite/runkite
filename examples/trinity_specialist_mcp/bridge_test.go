package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bridgeTestUpstream struct {
	server *httptest.Server
	mu     sync.Mutex
	key    string
	// All mutable fields are protected by mu because the official MCP SDK
	// handles HTTP requests and tool calls on independent goroutines.
	calls             int
	reads             int
	waits             int
	badAuth           int
	reference         string
	replaceAfterRead  bool
	status            string
	missing           bool
	wrongThread       bool
	waitInputOverride map[string]any
	run               map[string]any
}

func newBridgeTestUpstream(t *testing.T, advertiseRetry bool) *bridgeTestUpstream {
	t.Helper()
	u := &bridgeTestUpstream{key: "synthetic-bridge-test-key", status: "success"}
	properties := map[string]any{"message": map[string]any{"type": "string"}, "thread_id": map[string]any{"type": "string"}}
	if advertiseRetry {
		properties["run_id"] = map[string]any{"type": "string"}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test-specialist", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "specialist", InputSchema: map[string]any{"type": "object", "properties": properties}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			RunID    string `json:"run_id"`
			ThreadID string `json:"thread_id"`
			Message  string `json:"message"`
		}
		if err := json.Unmarshal(request.Params.Arguments, &args); err != nil {
			return nil, err
		}
		u.mu.Lock()
		u.calls++
		u.run = map[string]any{
			"run_id": args.RunID, "thread_id": args.ThreadID, "agent_id": "specialist", "status": u.status,
			"input": map[string]any{"messages": []any{map[string]any{"role": "user", "content": args.Message}}},
			"error": "Scripted specialist failure.",
		}
		status := u.status
		u.mu.Unlock()
		return &mcp.CallToolResult{IsError: status != "success", Content: []mcp.Content{&mcp.TextContent{Text: "Scripted specialist result."}}}, nil
	})
	mcpHTTP := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+u.key {
			u.mu.Lock()
			u.badAuth++
			u.mu.Unlock()
			http.Error(w, "authorization failed", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/mcp" {
			mcpHTTP.ServeHTTP(w, r)
			return
		}
		u.mu.Lock()
		defer u.mu.Unlock()
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/runs/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/wait") {
			u.waits++
		} else {
			u.reads++
		}
		if u.missing || u.run == nil {
			http.NotFound(w, r)
			return
		}
		row := make(map[string]any, len(u.run))
		for key, value := range u.run {
			row[key] = value
		}
		row["status"] = u.status
		if u.wrongThread {
			row["thread_id"] = "another-thread"
		}
		if strings.HasSuffix(r.URL.Path, "/wait") && u.waitInputOverride != nil {
			row["input"] = u.waitInputOverride
		}
		reference := u.reference
		if reference == "" {
			reference = "runkite-run-v1-" + strings.Repeat("a", 64)
		}
		if expected := r.Header.Get("Runkite-Expected-Run-Reference"); expected != "" && expected != reference {
			http.Error(w, "run reference changed", http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("Runkite-Run-Reference", reference)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/wait") {
			_ = json.NewEncoder(w).Encode(map[string]any{"run": row, "values": map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": "Scripted specialist result."}}}})
		} else {
			_ = json.NewEncoder(w).Encode(row)
			if u.replaceAfterRead {
				u.reference = "runkite-run-v1-" + strings.Repeat("b", 64)
			}
		}
	}))
	t.Cleanup(u.server.Close)
	return u
}

func bridgeTestConfig(t *testing.T, upstream *bridgeTestUpstream) bridgeConfig {
	t.Helper()
	return bridgeConfig{URL: upstream.server.URL, Agent: "specialist", Profile: "test-personal", Key: upstream.key, State: receiptTestDirectory(t), LocalProof: true, Timeout: 5 * time.Second}
}

func bridgeTestInvoke(t *testing.T, ctx context.Context, b *bridge, in invocation) (*mcp.CallToolResult, callHandle) {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.invoke(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: raw}})
	if err != nil || result == nil {
		t.Fatalf("bridge returned a protocol failure: %v", err)
	}
	return result, bridgeTestHandle(t, result)
}

func bridgeTestHandle(t *testing.T, result *mcp.CallToolResult) callHandle {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var handle callHandle
	if json.Unmarshal(raw, &handle) != nil || handle.Outcome == "" {
		t.Fatal("result has no structured operation handle")
	}
	if len(result.Content) == 0 {
		t.Fatal("result has no text operation handle")
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != string(raw) {
		t.Fatalf("structured/text handles differ: %s", raw)
	}
	return handle
}

func TestBridgeAuthenticatedMCPThenReadOnlyRetry(t *testing.T) {
	u := newBridgeTestUpstream(t, true)
	b := &bridge{config: bridgeTestConfig(t, u)}
	in := receiptTestInvocation()
	first, firstHandle := bridgeTestInvoke(t, context.Background(), b, in)
	if first.IsError || firstHandle.Outcome != "success" || firstHandle.OperationID != in.OperationID || firstHandle.ThreadID != in.ThreadID || firstHandle.RunID == "" || len(first.Content) != 2 {
		t.Fatalf("invalid first result: %+v", firstHandle)
	}
	// A fresh bridge object simulates the caller process restarting. It must
	// use retained REST observation, never send a second tools/call.
	second, secondHandle := bridgeTestInvoke(t, context.Background(), &bridge{config: b.config}, in)
	if second.IsError || secondHandle != firstHandle || len(second.Content) != 2 {
		t.Fatalf("retry lost its successful operation binding: %+v", secondHandle)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.calls != 1 || u.reads != 1 || u.waits != 1 || u.badAuth != 0 {
		t.Fatalf("unexpected traffic: calls=%d reads=%d waits=%d bad_auth=%d", u.calls, u.reads, u.waits, u.badAuth)
	}
	data, err := os.ReadFile(filepath.Join(b.config.State, receiptName(in.OperationID)+".json"))
	if err != nil || strings.Contains(string(data), u.key) || strings.Contains(string(data), "Scripted specialist result.") {
		t.Fatal("credential or output persisted with intent")
	}
}

func TestBridgeRefusesServerWithoutRetrySchema(t *testing.T) {
	u := newBridgeTestUpstream(t, false)
	b := &bridge{config: bridgeTestConfig(t, u)}
	result, handle := bridgeTestInvoke(t, context.Background(), b, receiptTestInvocation())
	if !result.IsError || handle.Outcome != "rejected" || handle.Reason != "required_run_id_schema_not_advertised" {
		t.Fatalf("old server was accepted: %+v", handle)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.calls != 0 {
		t.Fatal("tool dispatched without retry contract")
	}
	if _, err := os.Lstat(filepath.Join(b.config.State, receiptName(receiptTestInvocation().OperationID)+".attempt")); !os.IsNotExist(err) {
		t.Fatal("discovery failure consumed the first-send claim")
	}
}

func TestBridgeUnconfirmedRetainedRunNeverRedispatches(t *testing.T) {
	for _, kind := range []string{"missing", "wrong_thread", "nonterminal"} {
		t.Run(kind, func(t *testing.T) {
			u := newBridgeTestUpstream(t, true)
			b := &bridge{config: bridgeTestConfig(t, u)}
			_, _ = bridgeTestInvoke(t, context.Background(), b, receiptTestInvocation())
			u.mu.Lock()
			switch kind {
			case "missing":
				u.missing = true
			case "wrong_thread":
				u.wrongThread = true
			case "nonterminal":
				u.status = "running"
			}
			u.mu.Unlock()
			result, handle := bridgeTestInvoke(t, context.Background(), b, receiptTestInvocation())
			if !result.IsError || handle.Outcome != "delivery_unknown" {
				t.Fatalf("unconfirmed run became success/new work: %+v", handle)
			}
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.calls != 1 {
				t.Fatalf("retry dispatched again: %d calls", u.calls)
			}
		})
	}
}

func TestBridgeTerminalErrorsStayErrorsOnRetry(t *testing.T) {
	for _, status := range []string{"error", "interrupted", "timeout"} {
		t.Run(status, func(t *testing.T) {
			u := newBridgeTestUpstream(t, true)
			u.mu.Lock()
			u.status = status
			u.mu.Unlock()
			b := &bridge{config: bridgeTestConfig(t, u)}
			for range 2 {
				result, handle := bridgeTestInvoke(t, context.Background(), b, receiptTestInvocation())
				if !result.IsError || handle.Outcome != "tool_error" {
					t.Fatalf("terminal %s rendered as success: %+v", status, handle)
				}
			}
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.calls != 1 {
				t.Fatal("terminal error triggered automatic new work")
			}
		})
	}
}

func TestBridgeAuthenticationFailureIsNotToolSuccess(t *testing.T) {
	u := newBridgeTestUpstream(t, true)
	cfg := bridgeTestConfig(t, u)
	cfg.Key = "wrong-synthetic-key"
	result, handle := bridgeTestInvoke(t, context.Background(), &bridge{config: cfg}, receiptTestInvocation())
	if !result.IsError || handle.Outcome != "delivery_unknown" || strings.Contains(handle.Reason, cfg.Key) {
		t.Fatalf("authentication failed unsafely: %+v", handle)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.calls != 0 || u.badAuth == 0 {
		t.Fatalf("auth failure did not stop execution: calls=%d rejects=%d", u.calls, u.badAuth)
	}
}

func TestBridgeCancelledCallerSendsNoToolAction(t *testing.T) {
	u := newBridgeTestUpstream(t, true)
	b := &bridge{config: bridgeTestConfig(t, u)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, handle := bridgeTestInvoke(t, ctx, b, receiptTestInvocation())
	if !result.IsError || handle.Outcome == "success" {
		t.Fatalf("canceled caller succeeded: %+v", handle)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.calls != 0 {
		t.Fatal("canceled caller dispatched a tool")
	}
	if _, err := os.Lstat(filepath.Join(b.config.State, receiptName(receiptTestInvocation().OperationID)+".attempt")); !os.IsNotExist(err) {
		t.Fatal("canceled caller created a send claim")
	}
}

func TestBridgeConnectionFailureDoesNotExposeUpstreamBody(t *testing.T) {
	const privateDetail = "private-upstream-error-detail"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, privateDetail, http.StatusBadGateway)
	}))
	defer server.Close()
	cfg := bridgeConfig{URL: server.URL, Agent: "specialist", Profile: "test", Key: "synthetic-key", State: receiptTestDirectory(t), LocalProof: true, Timeout: 3 * time.Second}
	result, handle := bridgeTestInvoke(t, context.Background(), &bridge{config: cfg}, receiptTestInvocation())
	raw, _ := json.Marshal(result)
	if !result.IsError || handle.Outcome != "delivery_unknown" || strings.Contains(string(raw), privateDetail) || strings.Contains(string(raw), cfg.Key) {
		t.Fatalf("network failure was exposed or became success: %+v", handle)
	}
}

func TestBridgeStrictInvocationArguments(t *testing.T) {
	b := &bridge{config: bridgeConfig{URL: "invalid", Timeout: time.Second}}
	for name, raw := range map[string]string{
		"duplicate":       `{"operation_id":"op","thread_id":"thread","message":"first","message":"replacement"}`,
		"case_variant":    `{"operation_id":"op","thread_id":"thread","Message":"text"}`,
		"extra_field":     `{"operation_id":"op","thread_id":"thread","message":"text","url":"https://another.example"}`,
		"null":            `{"operation_id":"op","thread_id":"thread","message":null}`,
		"array":           `[]`,
		"string":          `"text"`,
		"missing":         `{"operation_id":"op","message":"text"}`,
		"blank_message":   `{"operation_id":"op","thread_id":"thread","message":" \n "}`,
		"invalid_handle":  `{"operation_id":"../op","thread_id":"thread","message":"text"}`,
		"high_surrogate":  `{"operation_id":"op","thread_id":"thread","message":"\ud800"}`,
		"low_surrogate":   `{"operation_id":"op","thread_id":"thread","message":"\udfff"}`,
		"trailing_object": `{"operation_id":"op","thread_id":"thread","message":"text"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			result, err := b.invoke(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(raw)}})
			if err != nil || !result.IsError || bridgeTestHandle(t, result).Reason != "invalid_arguments" {
				t.Fatalf("invalid raw arguments reached endpoint handling: %v", err)
			}
		})
	}
	for _, raw := range []string{
		`{"operation_id":"op","thread_id":"thread","message":"\ud83d\ude80"}`,
		`{"operation_id":"op","thread_id":"thread","message":"literal \\ud800 is not a surrogate escape"}`,
	} {
		result, err := b.invoke(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(raw)}})
		if err != nil || bridgeTestHandle(t, result).Reason != "endpoint_not_allowed" {
			t.Fatalf("valid Unicode invocation was rejected by argument parsing: %v", err)
		}
	}
}

func TestBridgeProtocolPublishesOneCallerBoundTool(t *testing.T) {
	b := &bridge{config: bridgeConfig{}}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := b.server().Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test-caller", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 1 || tools.Tools[0].Name != "invoke_specialist" {
		t.Fatalf("unexpected tool discovery: %+v error=%v", tools, err)
	}
	raw, _ := json.Marshal(tools.Tools[0].InputSchema)
	var schema struct {
		Required []string       `json:"required"`
		Fields   map[string]any `json:"properties"`
	}
	if json.Unmarshal(raw, &schema) != nil || len(schema.Required) != 3 || len(schema.Fields) != 3 {
		t.Fatal("caller-owned operation/thread/message contract missing")
	}
	for _, field := range []string{"operation_id", "thread_id", "message"} {
		if _, ok := schema.Fields[field]; !ok {
			t.Fatalf("missing caller field %s", field)
		}
	}
}

func TestCredentialTransportBindsOriginAndKeepsCallerRequest(t *testing.T) {
	origin, _ := url.Parse("https://specialist.example")
	seen := 0
	transport := &credentialTransport{origin: origin, key: "synthetic-key", base: endpointTestRoundTripper(func(request *http.Request) (*http.Response, error) {
		seen++
		if request.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Fatal("credential not set for configured origin")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	request, _ := http.NewRequest(http.MethodGet, origin.String()+"/mcp", nil)
	request.Header.Set("Authorization", "Bearer caller-placeholder")
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if request.Header.Get("Authorization") != "Bearer caller-placeholder" {
		t.Fatal("transport mutated caller headers")
	}
	other, _ := http.NewRequest(http.MethodGet, "https://another.example/mcp", nil)
	if _, err := transport.RoundTrip(other); err == nil || seen != 1 {
		t.Fatal("credential sent to another origin")
	}
}

func TestBridgeOutcomeIncludesHandleBeforeUpstreamContent(t *testing.T) {
	r := &receipt{RunID: "run-1", invocation: invocation{OperationID: "operation-1", ThreadID: "conversation-1"}}
	upstream := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "answer"}}}
	for _, name := range []string{"success", "rejected", "delivery_unknown", "tool_error"} {
		result := outcome(r, name, "safe_reason", upstream)
		handle := bridgeTestHandle(t, result)
		if handle.RunID != r.RunID || handle.OperationID != r.OperationID || handle.ThreadID != r.ThreadID || result.IsError != (name != "success") || len(result.Content) != 2 {
			t.Fatalf("incorrect outcome contract: %+v", handle)
		}
		if text, ok := result.Content[1].(*mcp.TextContent); !ok || text.Text != "answer" {
			t.Fatal("upstream content was changed")
		}
	}
}
