package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/getrunkite/runkite/internal/jsonvalue"
)

var (
	// ErrMCPDispatchBlocked means the evidence callback refused dispatch. It is
	// a local authorization failure, not a failed request to the MCP service.
	ErrMCPDispatchBlocked = errors.New("protected MCP dispatch could not be authorized")
	// ErrMCPResponseUnverified does not mean the tool had no effects.
	ErrMCPResponseUnverified = errors.New("protected MCP response could not be verified")
)

// MCPActionObservation contains only bounded protocol facts. It deliberately
// excludes request IDs, arguments, credentials, URLs and provider error text.
type MCPActionObservation struct {
	DispatchStarted  bool
	ResponseObserved bool
	HTTPStatus       *int
	RPCErrorCode     *int64
	ResponseBytes    *int64
	ResponseKind     string
	ReasonCode       string
}

// MCPActionTrace is request-local evidence plumbing, not a downstream execution
// receipt. The callback must commit authorization and intent before returning.
type MCPActionTrace struct {
	mu       sync.Mutex
	before   func(context.Context) error
	ob       MCPActionObservation
	failed   bool
	starting bool
}

func NewMCPActionTrace(before func(context.Context) error) *MCPActionTrace {
	return &MCPActionTrace{before: before, ob: MCPActionObservation{ReasonCode: "connector_preparation_failed"}}
}

type mcpActionTraceKey struct{}

func WithMCPActionTrace(ctx context.Context, trace *MCPActionTrace) context.Context {
	return context.WithValue(ctx, mcpActionTraceKey{}, trace)
}

func actionTrace(ctx context.Context) *MCPActionTrace {
	trace, _ := ctx.Value(mcpActionTraceKey{}).(*MCPActionTrace)
	return trace
}

func (t *MCPActionTrace) Snapshot() MCPActionObservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	ob := t.ob
	if ob.HTTPStatus != nil {
		v := *ob.HTTPStatus
		ob.HTTPStatus = &v
	}
	if ob.RPCErrorCode != nil {
		v := *ob.RPCErrorCode
		ob.RPCErrorCode = &v
	}
	if ob.ResponseBytes != nil {
		v := *ob.ResponseBytes
		ob.ResponseBytes = &v
	}
	return ob
}

func (t *MCPActionTrace) beforeDispatch(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if t.starting || t.ob.DispatchStarted || t.before == nil {
		t.mu.Unlock()
		return ErrMCPDispatchBlocked
	}
	t.starting = true
	t.mu.Unlock()
	err := t.before(ctx)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.ob.ReasonCode = "connector_dispatch_evidence_failed"
		return ErrMCPDispatchBlocked
	}
	t.ob.DispatchStarted = true
	t.ob.ReasonCode = "connector_outcome_unknown"
	return nil
}

func (t *MCPActionTrace) notDispatched(reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ob.DispatchStarted {
		t.ob.ReasonCode = reason
	}
}

func (t *MCPActionTrace) status(status int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ob.HTTPStatus = &status
}

func (t *MCPActionTrace) failure(reason string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failed = true
	t.ob.ReasonCode = reason
}

func (t *MCPActionTrace) observe(body []byte, requestID json.RawMessage, sdk bool) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ob.DispatchStarted {
		return nil
	}
	if t.failed {
		return ErrMCPResponseUnverified
	}
	size := int64(len(body))
	t.ob.ResponseBytes = &size
	kind, code, valid := classifyMCPResponse(body, requestID)
	if !valid {
		t.ob.ResponseKind = "unverified_response"
		t.ob.ReasonCode = "connector_response_unverified"
		return ErrMCPResponseUnverified
	}
	if sdk {
		kind = "sdk_" + kind
	}
	t.ob.ResponseKind = kind
	t.ob.RPCErrorCode = code
	t.ob.ResponseObserved = true
	t.ob.ReasonCode = ""
	return nil
}

func classifyMCPResponse(body []byte, requestID json.RawMessage) (string, *int64, bool) {
	if len(requestID) == 0 {
		return "", nil, false
	}
	if _, err := jsonvalue.DecodeUnique(body); err != nil {
		return "", nil, false
	}
	envelope, err := strictRPCObject(body, []string{"jsonrpc", "id", "result", "error"})
	if err != nil {
		return "", nil, false
	}
	var version string
	if json.Unmarshal(envelope["jsonrpc"], &version) != nil || version != "2.0" {
		return "", nil, false
	}
	gotID, err := jsonvalue.DecodeUnique(envelope["id"])
	wantID, wantErr := jsonvalue.DecodeUnique(requestID)
	if err != nil || wantErr != nil || !jsonvalue.Equal(gotID, wantID) {
		return "", nil, false
	}
	result, hasResult := envelope["result"]
	rpcError, hasError := envelope["error"]
	if hasResult == hasError {
		return "", nil, false
	}
	if hasError {
		fields, err := strictRPCObject(rpcError, []string{"code", "message", "data"})
		if err != nil {
			return "", nil, false
		}
		var code int64
		var message string
		if bytes.Equal(bytes.TrimSpace(fields["code"]), []byte("null")) || bytes.Equal(bytes.TrimSpace(fields["message"]), []byte("null")) || json.Unmarshal(fields["code"], &code) != nil || json.Unmarshal(fields["message"], &message) != nil {
			return "", nil, false
		}
		return "jsonrpc_error", &code, true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil || fields == nil {
		return "", nil, false
	}
	if raw, ok := fields["isError"]; ok {
		var isError bool
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &isError) != nil {
			return "", nil, false
		}
		if isError {
			return "tool_error_reported", nil, true
		}
	}
	return "jsonrpc_result", nil, true
}

// The SDK also sends initialization, discovery and cleanup traffic. Only its
// protected POST contributes HTTP response facts to this action's observation.
func protectedMCPRequest(r *http.Request) bool {
	if r.Method != http.MethodPost || r.GetBody == nil {
		return false
	}
	body, err := r.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	raw, err := readLimitedBody(body, MaxMCPRequestBytes)
	if err != nil {
		return false
	}
	var envelope struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(raw, &envelope) == nil && envelope.Method == "tools/call"
}
