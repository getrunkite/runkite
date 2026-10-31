package connector

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/getrunkite/runkite/internal/jsonvalue"
)

// MCPRequest is the single interpretation used for enforcement and forwarding.
// Arguments remain raw JSON so application keys retain their spelling and case.
type MCPRequest struct {
	ID        json.RawMessage
	Method    string
	Tool      string
	Arguments json.RawMessage
	Body      []byte
}

// ParseMCPRequest rejects ambiguous reserved keys before creating the bytes that
// will reach the downstream server. Go struct decoding is case-insensitive and
// must not be used to authorize an unmodified JSON-RPC envelope.
func ParseMCPRequest(body []byte) (*MCPRequest, error) {
	if int64(len(body)) > MaxMCPRequestBytes {
		return nil, ErrBodyTooLarge
	}
	if _, err := jsonvalue.DecodeUnique(body); err != nil {
		return nil, fmt.Errorf("invalid or ambiguous JSON-RPC request: %w", err)
	}
	envelope, err := strictRPCObject(body, []string{"jsonrpc", "id", "method", "params"})
	if err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC request: %w", err)
	}
	var version, method string
	if err := json.Unmarshal(envelope["jsonrpc"], &version); err != nil || version != "2.0" {
		return nil, fmt.Errorf("invalid JSON-RPC version")
	}
	if err := json.Unmarshal(envelope["method"], &method); err != nil || method == "" {
		return nil, fmt.Errorf("invalid JSON-RPC method")
	}
	req := &MCPRequest{ID: envelope["id"], Method: method}
	if raw, ok := envelope["params"]; ok {
		params, err := strictRPCObject(raw, []string{"name", "arguments"})
		if err != nil {
			return nil, fmt.Errorf("invalid JSON-RPC params: %w", err)
		}
		if method == "tools/call" {
			if raw, ok := params["name"]; ok {
				if err := json.Unmarshal(raw, &req.Tool); err != nil {
					return nil, fmt.Errorf("invalid tools/call name")
				}
			}
			req.Arguments = params["arguments"]
			if len(req.Arguments) > 0 && bytes.TrimSpace(req.Arguments)[0] != '{' {
				return nil, fmt.Errorf("tools/call arguments must be an object")
			}
		}
		envelope["params"], err = json.Marshal(params)
		if err != nil {
			return nil, err
		}
	} else if method == "tools/call" {
		return nil, fmt.Errorf("tools/call requires params")
	}
	req.Body, err = json.Marshal(envelope)
	return req, err
}

func strictRPCObject(raw []byte, reserved []string) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected one object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("invalid object key")
		}
		for _, name := range reserved {
			if key != name && strings.EqualFold(key, name) {
				return nil, fmt.Errorf("reserved key %q must use exact spelling", name)
			}
		}
		if _, exists := out[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		out[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("expected exactly one object")
	}
	return out, nil
}
