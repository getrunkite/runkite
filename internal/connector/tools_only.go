package connector

import (
	"encoding/json"
	"errors"
	"net/http"
)

var (
	ErrMCPInitializationUnverified = errors.New("connector initialization response is unverified")
	ErrMCPInitializationInvalid    = errors.New("invalid tools-only initialization request")
)

func toolsOnlyMethodAllowed(method string) bool {
	switch method {
	case "initialize", "notifications/initialized", "ping", "tools/list", "tools/call":
		return true
	default:
		return false
	}
}

func deniedMCPMethodResult(id json.RawMessage) (*ProxyMCPResult, error) {
	// Notifications never get a JSON-RPC response. In particular, do not
	// fabricate an ID for a denied notification or forward it upstream.
	if len(id) == 0 {
		return &ProxyMCPResult{StatusCode: http.StatusAccepted}, nil
	}
	body, err := json.Marshal(jsonRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: &jsonRPCError{Code: -32601, Message: "method unavailable on tools-only connector",
			Data: json.RawMessage(`{"reason_code":"connector_mcp_method_denied"}`)},
	})
	return &ProxyMCPResult{Body: body, StatusCode: http.StatusOK}, err
}

func clearInitializationClientCapabilities(req *MCPRequest) error {
	// ParseMCPRequest already rejects duplicate keys throughout the message.
	// Check the additional initialization names before replacing capabilities,
	// since an upstream Go decoder could accept a case alias as a second value.
	envelope, err := strictRPCObject(req.Body, []string{"jsonrpc", "id", "method", "params"})
	if err != nil {
		return ErrMCPInitializationInvalid
	}
	params, err := strictRPCObject(envelope["params"], []string{"protocolVersion", "clientInfo", "capabilities"})
	if err != nil {
		return ErrMCPInitializationInvalid
	}
	if raw, exists := params["capabilities"]; exists {
		if _, err := strictRPCObject(raw, nil); err != nil {
			return ErrMCPInitializationInvalid
		}
	}
	params["capabilities"] = json.RawMessage(`{}`)
	envelope["params"], err = json.Marshal(params)
	if err != nil {
		return ErrMCPInitializationInvalid
	}
	req.Body, err = json.Marshal(envelope)
	if err != nil {
		return ErrMCPInitializationInvalid
	}
	return nil
}

type initializationResponse struct {
	envelope     map[string]json.RawMessage
	result       map[string]json.RawMessage
	capabilities map[string]json.RawMessage
	rpcError     bool
}

func parseInitializationResponse(body []byte, id json.RawMessage) (*initializationResponse, error) {
	kind, _, valid := classifyMCPResponse(body, id)
	if !valid {
		return nil, ErrMCPInitializationUnverified
	}
	envelope, err := strictRPCObject(body, []string{"jsonrpc", "id", "result", "error", "method", "params"})
	if err != nil || envelope["method"] != nil || envelope["params"] != nil {
		return nil, ErrMCPInitializationUnverified
	}
	parsed := &initializationResponse{envelope: envelope, rpcError: kind == "jsonrpc_error"}
	if parsed.rpcError {
		return parsed, nil
	}
	result, err := strictRPCObject(envelope["result"], []string{"capabilities"})
	if err != nil {
		return nil, ErrMCPInitializationUnverified
	}
	capabilities, err := strictRPCObject(result["capabilities"], []string{"tools"})
	if err != nil {
		return nil, ErrMCPInitializationUnverified
	}
	if tools, exists := capabilities["tools"]; exists {
		if _, err := strictRPCObject(tools, nil); err != nil {
			return nil, ErrMCPInitializationUnverified
		}
	}

	parsed.result, parsed.capabilities = result, capabilities
	return parsed, nil
}

func toolsOnlyInitializationResponse(body []byte, id json.RawMessage) ([]byte, error) {
	parsed, err := parseInitializationResponse(body, id)
	if err != nil {
		return nil, err
	}
	if parsed.rpcError {
		return body, nil
	}
	capabilities := map[string]json.RawMessage{}
	if _, exists := parsed.capabilities["tools"]; exists {
		capabilities["tools"] = json.RawMessage(`{}`)
	}
	parsed.result["capabilities"], _ = json.Marshal(capabilities)
	parsed.envelope["result"], _ = json.Marshal(parsed.result)
	return json.Marshal(parsed.envelope)
}
