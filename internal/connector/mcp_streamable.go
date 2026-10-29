package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Each operation owns and closes its upstream session. No session or bearer
// credential can be reused by another run, generation, tenant or CP replica.
// The runner-facing response stays JSON even when upstream uses SSE.
func forwardStreamableMCP(ctx context.Context, endpoint, token string, req *MCPRequest) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := *httpClient
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var oversized atomic.Bool
	client.Transport = mcpAuthTransport{next: base, token: token, oversized: &oversized, scope: ctx}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	upstream := mcp.NewClient(&mcp.Implementation{Name: "runkite-connector", Version: "0.5.7"},
		&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	session, err := upstream.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &client, MaxRetries: -1, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		if oversized.Load() {
			return nil, 0, ErrBodyTooLarge
		}
		return nil, 0, fmt.Errorf("initialize upstream MCP: %w", err)
	}
	defer session.Close()
	if oversized.Load() {
		return nil, 0, ErrBodyTooLarge
	}

	var envelope struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(req.Body, &envelope); err != nil {
		return nil, 0, err
	}
	params := envelope.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var result any
	switch req.Method {
	case "initialize":
		result = session.InitializeResult()
	case "notifications/initialized":
		return nil, http.StatusAccepted, nil
	case "ping":
		err = session.Ping(ctx, nil)
		result = struct{}{}
	case "tools/list":
		var p mcp.ListToolsParams
		if err = json.Unmarshal(params, &p); err == nil {
			result, err = session.ListTools(ctx, &p)
		}
	case "tools/call":
		var p mcp.CallToolParams
		if err = json.Unmarshal(params, &p); err == nil {
			p.Name = req.Tool
			p.Arguments = json.RawMessage(`{}`)
			if len(req.Arguments) > 0 {
				p.Arguments = req.Arguments
			}
			// Negotiated protocol revisions can derive required HTTP parameter
			// headers from the advertised schema. Populate the SDK's tool cache
			// before calling, without retrying a tool after dispatch.
			err = findUpstreamTool(ctx, session, p.Name)
			if err == nil {
				result, err = session.CallTool(ctx, &p)
			}
		}
	case "resources/list":
		var p mcp.ListResourcesParams
		if err = json.Unmarshal(params, &p); err == nil {
			result, err = session.ListResources(ctx, &p)
		}
	case "resources/templates/list":
		var p mcp.ListResourceTemplatesParams
		if err = json.Unmarshal(params, &p); err == nil {
			result, err = session.ListResourceTemplates(ctx, &p)
		}
	case "resources/read":
		var p mcp.ReadResourceParams
		if err = json.Unmarshal(params, &p); err == nil {
			result, err = session.ReadResource(ctx, &p)
		}
	case "prompts/list":
		var p mcp.ListPromptsParams
		if err = json.Unmarshal(params, &p); err == nil {
			result, err = session.ListPrompts(ctx, &p)
		}
	case "prompts/get":
		var p mcp.GetPromptParams
		if err = json.Unmarshal(params, &p); err == nil {
			result, err = session.GetPrompt(ctx, &p)
		}
	default:
		err = &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method unavailable on request-scoped connector transport"}
	}
	// The SDK formats some HTTP read errors into JSON-RPC errors rather than
	// wrapping them. Retain our own overflow signal for the API's stable code.
	if oversized.Load() {
		return nil, 0, ErrBodyTooLarge
	}
	response := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
	if err != nil {
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) {
			return nil, 0, err
		}
		response.Error = &jsonRPCError{Code: int(rpcErr.Code), Message: rpcErr.Message, Data: rpcErr.Data}
	} else {
		response.Result, err = json.Marshal(result)
		if err != nil {
			return nil, 0, err
		}
	}
	body, err := json.Marshal(response)
	if err == nil && int64(len(body)) > MaxMCPResponseBytes {
		err = ErrBodyTooLarge
	}
	return body, http.StatusOK, err
}

func findUpstreamTool(ctx context.Context, session *mcp.ClientSession, name string) error {
	cursor := ""
	seen := map[string]bool{}
	for range 100 {
		page, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return err
		}
		for _, tool := range page.Tools {
			if tool.Name == name {
				return nil
			}
		}
		if page.NextCursor == "" || seen[page.NextCursor] {
			break
		}
		cursor = page.NextCursor
		seen[cursor] = true
	}
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "tool not advertised by upstream MCP server"}
}

type mcpAuthTransport struct {
	next      http.RoundTripper
	token     string
	oversized *atomic.Bool
	scope     context.Context
}

func (t mcpAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// The SDK detaches its session context and uses it for DELETE cleanup.
	// Preserve each request's own cancellation while bounding every request by
	// the lifetime and deadline of this one proxy operation.
	var requestCtx context.Context
	var cancel context.CancelFunc
	if t.scope != nil {
		if err := t.scope.Err(); err != nil {
			return nil, err
		}
		if deadline, ok := t.scope.Deadline(); ok {
			requestCtx, cancel = context.WithDeadline(r.Context(), deadline)
		} else {
			requestCtx, cancel = context.WithCancel(r.Context())
		}
	} else {
		requestCtx, cancel = context.WithCancel(r.Context())
	}
	stopScope := func() bool { return false }
	if t.scope != nil {
		stopScope = context.AfterFunc(t.scope, func() {
			// WithDeadline already propagates deadline expiry with its correct cause.
			// This callback forwards an earlier explicit cancellation of the scope.
			if !errors.Is(t.scope.Err(), context.DeadlineExceeded) {
				cancel()
			}
		})
	}
	cleanup := func() { stopScope(); cancel() }
	r = r.Clone(requestCtx)
	if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	response, err := t.next.RoundTrip(r)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		cleanup()
		return nil, err
	}
	if response.ContentLength > MaxMCPResponseBytes {
		if t.oversized != nil {
			t.oversized.Store(true)
		}
		response.Body.Close()
		cleanup()
		return nil, ErrBodyTooLarge
	}
	response.Body = &mcpLimitedBody{ReadCloser: response.Body, remaining: MaxMCPResponseBytes, oversized: t.oversized, cleanup: cleanup}
	return response, nil
}

// Count streamed bytes as they arrive instead of buffering an unbounded SSE
// response before the normal JSON result cap can run.
type mcpLimitedBody struct {
	io.ReadCloser
	remaining int64
	oversized *atomic.Bool
	cleanup   func()
}

func (r *mcpLimitedBody) Close() error {
	if r.cleanup != nil {
		defer r.cleanup()
	}
	return r.ReadCloser.Close()
}

func (r *mcpLimitedBody) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		return 0, ErrBodyTooLarge
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		if r.oversized != nil {
			r.oversized.Store(true)
		}
		return n, ErrBodyTooLarge
	}
	return n, err
}
