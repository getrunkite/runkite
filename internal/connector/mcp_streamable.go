package connector

import (
	"context"
	"encoding/json"
	"errors"
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
func forwardStreamableMCPAuthorization(ctx context.Context, endpoint, authorization string, req *MCPRequest) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client := *httpClient
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	var oversized atomic.Bool
	trace := actionTrace(ctx)
	capture, _ := ctx.Value(rawToolCaptureKey{}).(*rawToolCapture)
	if _, bound := ctx.Value(toolRevisionKey{}).(boundToolRevision); bound && capture == nil {
		capture = newRawToolCapture()
	}
	client.Transport = mcpAuthTransport{next: base, authorization: authorization, oversized: &oversized, scope: ctx, action: trace, toolCapture: capture}
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
		return nil, 0, safeConnectorRequestError(ctx, "initialize upstream MCP failed", err)
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
			if capture != nil && err == nil {
				page, pageErr := capture.page(p.Cursor)
				if pageErr != nil {
					return nil, 0, ErrToolRevisionUnavailable
				}
				result = page.result
			}
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
			err = findUpstreamTool(ctx, session, p.Name, capture)
			if errors.Is(err, ErrToolRevisionUnavailable) || errors.Is(err, ErrToolRevisionChanged) {
				return nil, 0, err
			}
			if err == nil {
				if err := trace.beforeDispatch(ctx); err != nil {
					return nil, 0, err
				}
				result, err = session.CallTool(ctx, &p)
				if err != nil {
					// SDK errors may also be synthesized locally. Do not claim an
					// upstream JSON-RPC error was observed solely from their type.
					trace.failure("connector_sdk_operation_failed")
				}
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
	if capture != nil && capture.failure() != nil {
		return nil, 0, ErrToolRevisionUnavailable
	}
	response := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
	if err != nil {
		var rpcErr *jsonrpc.Error
		if !errors.As(err, &rpcErr) {
			return nil, 0, safeConnectorRequestError(ctx, "upstream MCP operation failed", err)
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

func findUpstreamTool(ctx context.Context, session *mcp.ClientSession, name string, capture *rawToolCapture) error {
	if bound, ok := ctx.Value(toolRevisionKey{}).(boundToolRevision); ok {
		return findExactUpstreamTool(ctx, session, name, bound, capture)
	}
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
	next          http.RoundTripper
	token         string
	authorization string
	oversized     *atomic.Bool
	scope         context.Context
	action        *MCPActionTrace
	toolCapture   *rawToolCapture
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
	protected := t.action != nil && protectedMCPRequest(r)
	if t.authorization != "" {
		r.Header.Set("Authorization", t.authorization)
	} else if t.token != "" {
		r.Header.Set("Authorization", "Bearer "+t.token)
	}
	response, err := t.next.RoundTrip(r)
	if err != nil {
		if protected {
			t.action.failure("connector_transport_failed")
		}
		safeErr := safeConnectorRequestError(requestCtx, "upstream MCP request failed", err)
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		cleanup()
		return nil, safeErr
	}
	if protected {
		t.action.status(response.StatusCode)
	}
	if response.ContentLength > MaxMCPResponseBytes {
		if protected {
			t.action.failure("connector_response_incomplete")
		}
		if t.oversized != nil {
			t.oversized.Store(true)
		}
		response.Body.Close()
		cleanup()
		return nil, ErrBodyTooLarge
	}
	if t.toolCapture != nil {
		if _, err := t.toolCapture.capture(r, response); err != nil {
			_ = response.Body.Close()
			cleanup()
			return nil, ErrToolRevisionUnavailable
		}
	}
	var observed *MCPActionTrace
	if protected {
		observed = t.action
	}
	response.Body = &mcpLimitedBody{ReadCloser: response.Body, remaining: MaxMCPResponseBytes, oversized: t.oversized, cleanup: cleanup, ctx: requestCtx, action: observed}
	return response, nil
}

// Count streamed bytes as they arrive instead of buffering an unbounded SSE
// response before the normal JSON result cap can run.
type mcpLimitedBody struct {
	io.ReadCloser
	remaining int64
	oversized *atomic.Bool
	cleanup   func()
	ctx       context.Context
	action    *MCPActionTrace
}

func (r *mcpLimitedBody) Close() error {
	if r.cleanup != nil {
		defer r.cleanup()
	}
	if err := r.ReadCloser.Close(); err != nil {
		return safeConnectorRequestError(r.ctx, "close upstream MCP response failed", err)
	}
	return nil
}

func (r *mcpLimitedBody) Read(p []byte) (int, error) {
	if r.remaining < 0 {
		r.action.failure("connector_response_incomplete")
		return 0, ErrBodyTooLarge
	}
	if int64(len(p)) > r.remaining+1 {
		p = p[:r.remaining+1]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		r.action.failure("connector_response_incomplete")
		if r.oversized != nil {
			r.oversized.Store(true)
		}
		return n, ErrBodyTooLarge
	}
	if err != nil && !errors.Is(err, io.EOF) {
		r.action.failure("connector_response_incomplete")
		err = safeConnectorRequestError(r.ctx, "read upstream MCP response failed", err)
	}
	return n, err
}
