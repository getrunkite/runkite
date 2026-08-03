package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// This file makes IsToolAllowed an actual enforcement gate instead of dead
// code -- previously, anyone who configured a connector's tool whitelist,
// assuming it actually restricted what a compromised or misbehaving agent
// could invoke, was trusting something that didn't do that yet.
//
// Before this file existed, GetSession handed a runner the connector's
// RAW downstream MCP URL and credentials -- the runner's own MCP client
// library (whatever the agent author wired up) then talked directly to
// the real server, completely bypassing Runkite. The advertised, filtered
// Tools list on MCPSession was advisory only: it steered a well-behaved
// agent away from denied tools, but did nothing against one that queried
// the real server directly for its own tools/list and called whatever it
// found.
//
// This proxy closes that gap the only way it structurally can be closed
// without redesigning the whole session contract: GetSession now points
// MCP.URL at THIS proxy (see registry.go) instead of the raw downstream
// URL, so every tools/call request -- regardless of which MCP client
// library the agent's own code uses -- passes through here, where a
// denied tool never reaches the downstream server at all. This also
// fixes a real correctness bug in allowedTools() along the way: a
// deny-only filter (no allow list) couldn't be represented in the
// advisory tools list at session-creation time without knowing the full
// tool universe the downstream server exposes -- this proxy CAN
// correctly apply a deny-only filter, because it filters the downstream
// server's own real tools/list response, not a static guess.
//
// Scope, stated plainly: this covers the two JSON-RPC methods where
// enforcement actually matters (tools/call is gated, tools/list is
// filtered); every other MCP method (initialize, resources/*, prompts/*,
// notifications) is forwarded transparently. Simple POST-in/POST-out
// JSON-RPC only -- an MCP transport that also needs a long-lived SSE
// stream for server-initiated pushes is not covered by this pass.

// jsonRPCRequest / jsonRPCResponse are MCP's JSON-RPC 2.0 envelope --
// only the fields this proxy needs to inspect or construct, not a full
// JSON-RPC or MCP schema implementation. ID is json.RawMessage because
// JSON-RPC ids can be a string, a number, or null -- passing it through
// verbatim (not re-typing it) is what lets a real ID round-trip correctly
// regardless of which shape the caller used.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// toolNotAllowedCode is a custom JSON-RPC error code in the
// implementation-defined server-error range (-32000 to -32099 per the
// JSON-RPC 2.0 spec), distinct from tools/call's own "unknown tool"
// error (which would come from the downstream server, if the request
// ever reached it) -- a caller can tell "denied by policy" apart from
// "the tool doesn't exist" by checking this code.
const toolNotAllowedCode = -32000

// ProxyMCPResult is a successful proxy outcome: a JSON-RPC response body
// (which may itself carry a JSON-RPC-level error, e.g. tool-not-allowed
// or whatever the downstream server returned) and the HTTP status to
// send it with. Distinct from the error return, which is reserved for
// transport-level failures this call couldn't even get a JSON-RPC
// response for (unknown connector, circuit open, downstream unreachable)
// -- the same split GetSession already uses, so handleProxyMCPRequest
// in internal/api/server.go can reuse its exact error-handling pattern.
type ProxyMCPResult struct {
	Body       []byte
	StatusCode int
}

// ProxyMCPRequest forwards one JSON-RPC request to a connector's
// configured downstream MCP server, enforcing its tool allow/deny list
// at the two points where it actually matters. See this file's own doc
// comment for the full rationale.
func (r *Registry) ProxyMCPRequest(ctx context.Context, name string, userCtx map[string]interface{}, body []byte) (*ProxyMCPResult, error) {
	c, err := r.Get(name)
	if err != nil {
		return nil, err
	}
	if c.Config.MCP == nil {
		return nil, fmt.Errorf("connector %s has no MCP endpoint configured", name)
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC request: %w", err)
	}

	if req.Method == "tools/call" {
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &params)
		// A tools/call with no name is never a legitimate request,
		// REGARDLESS of the connector's allow/deny configuration -- so
		// this is checked as its own structural rule, before consulting
		// the filter at all, not folded into isAllowed. Found live on
		// review, the hard way: an
		// earlier version of this fix just called isAllowed(filter, "")
		// unconditionally, which is correct FOR isAllowed's OWN
		// semantics with an allow-list (empty string matches no allow
		// entry, denied) but wrong for a deny-only filter -- deny-only
		// means "allow everything except these," and "" isn't on the
		// deny list, so isAllowed correctly-per-its-own-contract
		// returned true. Confirmed live: a deny-only connector forwarded
		// an empty-name tools/call straight to the downstream server.
		// An empty name should never reach a real MCP server no matter
		// what the filter shape is, so it's rejected here unconditionally.
		if params.Name == "" || !isAllowed(c.Config.Tools, params.Name) {
			return deniedToolResult(req.ID, name, params.Name)
		}
	}

	if !c.breaker.Allow() {
		return nil, &ErrCircuitOpen{Connector: c.Name}
	}

	token, err := c.getToken(ctx, userCtx)
	if err != nil {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("connector %s: %w", name, err)
	}

	respBody, statusCode, err := forwardMCPRequest(ctx, c.Config.MCP.URL, token.AccessToken, body)
	if err != nil {
		c.breaker.RecordFailure()
		return nil, fmt.Errorf("connector %s: mcp request failed: %w", name, err)
	}
	c.breaker.RecordSuccess()

	if req.Method == "tools/list" {
		respBody = filterToolsListResponse(respBody, c.Config.Tools)
	}

	return &ProxyMCPResult{Body: respBody, StatusCode: statusCode}, nil
}

// deniedToolResult builds a JSON-RPC error response for a denied tool
// call. This is NOT a transport failure -- it's a normal, successful
// proxy outcome whose body happens to carry a JSON-RPC-level error, the
// same way a downstream server's own "unknown tool" response would be.
// The downstream server is never contacted for a denied call.
func deniedToolResult(id json.RawMessage, connectorName, toolName string) (*ProxyMCPResult, error) {
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &jsonRPCError{
			Code:    toolNotAllowedCode,
			Message: fmt.Sprintf("tool %q is not allowed by connector %q's tool filter", toolName, connectorName),
		},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshal denied-tool response: %w", err)
	}
	return &ProxyMCPResult{Body: out, StatusCode: http.StatusOK}, nil
}

// forwardMCPRequest sends body to the downstream MCP server as-is,
// injecting the connector's own access token as a Bearer credential so
// the runner never needs to see or forward raw downstream credentials
// for MCP traffic -- it only ever talks to this proxy.
func forwardMCPRequest(ctx context.Context, url, accessToken string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read response: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

// filterToolsListResponse filters a tools/list response's tool array
// against filter, using the DOWNSTREAM SERVER'S OWN real tool list --
// unlike allowedTools() (registry.go), which only has a static allow
// list to work with at session-creation time and can't correctly
// represent a deny-only filter without knowing the full tool universe.
// Passes the response through unchanged if it doesn't parse as expected
// (e.g. the downstream server returned a JSON-RPC error instead of a
// tools/list result) -- this proxy forwards a downstream failure
// verbatim rather than masking it.
func filterToolsListResponse(body []byte, filter *ToolFilter) []byte {
	if filter == nil {
		return body
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Result  *struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result,omitempty"`
		Error json.RawMessage `json:"error,omitempty"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Result == nil {
		return body
	}

	filtered := make([]json.RawMessage, 0, len(resp.Result.Tools))
	for _, raw := range resp.Result.Tools {
		var tool struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &tool) == nil && isAllowed(filter, tool.Name) {
			filtered = append(filtered, raw)
		}
	}
	resp.Result.Tools = filtered

	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}
