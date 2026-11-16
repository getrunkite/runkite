// This example is a caller of Runkite, not a runner or a policy engine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var handlePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

type bridgeConfig struct {
	URL, Agent, Profile, Key, State string
	LocalProof                      bool
	Timeout                         time.Duration
}

type invocation struct {
	OperationID string `json:"operation_id"`
	ThreadID    string `json:"thread_id"`
	Message     string `json:"message"`
}

type callHandle struct {
	OperationID string `json:"operation_id"`
	RunID       string `json:"run_id,omitempty"`
	ThreadID    string `json:"thread_id,omitempty"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
}

type bridge struct{ config bridgeConfig }

func main() {
	cfg := bridgeConfig{
		URL: os.Getenv("RUNKITE_BRIDGE_URL"), Agent: os.Getenv("RUNKITE_BRIDGE_AGENT"),
		Profile: os.Getenv("RUNKITE_BRIDGE_PROFILE"), Key: os.Getenv("RUNKITE_API_KEY"),
		State: os.Getenv("RUNKITE_BRIDGE_STATE"), Timeout: 45 * time.Second,
		LocalProof: os.Getenv("RUNKITE_BRIDGE_LOCAL_PROOF") == "1",
	}
	if cfg.URL == "" || cfg.Key == "" || strings.ContainsAny(cfg.Key, "\r\n") ||
		!handlePattern.MatchString(cfg.Agent) || !handlePattern.MatchString(cfg.Profile) {
		fmt.Fprintln(os.Stderr, "Configure the bridge endpoint, fixed agent, profile, private state directory and credential.")
		os.Exit(1)
	}
	if err := prepareDirectory(cfg.State); err != nil {
		fmt.Fprintln(os.Stderr, "The bridge needs an absolute, owner-only state directory.")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := (&bridge{cfg}).server().Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "The specialist MCP connection closed with a protocol error.")
		os.Exit(1)
	}
}

func (b *bridge) server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "runkite-specialist-example", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{
		Name:        "invoke_specialist",
		Description: "Invoke the configured Runkite specialist. Persist and reuse operation_id, thread_id and message after delivery failure. A new operation_id is new work. Only one turn at a time per thread. Delivery cancellation does not cancel the remote agent.",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"required": []string{"operation_id", "thread_id", "message"},
			"properties": map[string]any{
				"operation_id": map[string]any{"type": "string", "pattern": handlePattern.String(), "description": "Stable caller-owned ID for this one intended turn, not a hash of message text."},
				"thread_id":    map[string]any{"type": "string", "pattern": handlePattern.String(), "description": "Caller-owned conversation ID. Reuse for a later turn, with a new operation ID."},
				"message":      map[string]any{"type": "string", "minLength": 1, "maxLength": 8192},
			},
		},
	}, b.invoke)
	return s
}

func outcome(r *receipt, name, reason string, upstream *mcp.CallToolResult) *mcp.CallToolResult {
	h := callHandle{Outcome: name, Reason: reason}
	if r != nil {
		h.OperationID, h.RunID, h.ThreadID = r.OperationID, r.RunID, r.ThreadID
	}
	raw, _ := json.Marshal(h)
	result := &mcp.CallToolResult{IsError: name != "success", StructuredContent: h, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
	if upstream != nil {
		result.Content = append(result.Content, upstream.Content...)
	}
	return result
}

func (b *bridge) invoke(parent context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if request == nil || request.Params == nil || len(request.Params.Arguments) > 32768 {
		return outcome(nil, "rejected", "invalid_arguments", nil), nil
	}
	if parent.Err() != nil {
		return outcome(nil, "rejected", "delivery_cancelled_before_dispatch", nil), nil
	}
	// Parse once, with exact field names, before saving or forwarding anything.
	parsed, parseErr := jsonvalue.DecodeUnique(request.Params.Arguments)
	fields, object := parsed.(map[string]any)
	if parseErr != nil || !object || len(fields) != 3 {
		return outcome(nil, "rejected", "invalid_arguments", nil), nil
	}
	var in invocation
	for name, value := range fields {
		var target *string
		switch name {
		case "operation_id":
			target = &in.OperationID
		case "thread_id":
			target = &in.ThreadID
		case "message":
			target = &in.Message
		default:
			return outcome(nil, "rejected", "invalid_arguments", nil), nil
		}
		text, ok := value.(string)
		if !ok {
			return outcome(nil, "rejected", "invalid_arguments", nil), nil
		}
		*target = text
	}
	if !handlePattern.MatchString(in.OperationID) || !handlePattern.MatchString(in.ThreadID) ||
		!utf8.ValidString(in.Message) || strings.TrimSpace(in.Message) == "" || len(in.Message) > 8192 {
		return outcome(nil, "rejected", "invalid_arguments", nil), nil
	}
	ctx, cancel := context.WithTimeout(parent, b.config.Timeout)
	defer cancel()
	base, client, closeIdle, err := newEndpointClient(ctx, endpointConfig{BaseURL: b.config.URL, AllowLocalProof: b.config.LocalProof})
	if err != nil {
		return outcome(nil, "rejected", "endpoint_not_allowed", nil), nil
	}
	defer closeIdle()
	client.Transport = &credentialTransport{base: client.Transport, origin: base, key: b.config.Key}
	r, err := loadOrSaveReceipt(b.config, base.String(), in)
	if err != nil {
		return outcome(nil, "rejected", "receipt_conflict_or_unavailable", nil), nil
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "runkite-specialist-caller", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: base.String() + "/mcp", HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return outcome(r, "delivery_unknown", "connection_failed_reuse_saved_operation", nil), nil
	}
	defer session.Close()
	if err := supportsRetry(ctx, session, b.config.Agent); err != nil {
		return outcome(r, "rejected", "required_run_id_schema_not_advertised", nil), nil
	}
	first, err := claimAttempt(b.config.State, r)
	if err != nil {
		return outcome(r, "rejected", "attempt_marker_unavailable", nil), nil
	}
	if !first {
		// A missing retained run is ambiguous after a prior send. Never turn
		// pruning, a lost queue acknowledgement or auth failure into new work.
		if err := checkRetainedRun(ctx, client, base, r); err != nil {
			return outcome(r, "delivery_unknown", "retained_run_unconfirmed_reconcile_before_retry", nil), nil
		}
		// Observation is read-only. Pruning between these reads can lose a
		// result, but cannot make this client invoke another tool call.
		upstream, err := observeRetainedRun(ctx, client, base, r)
		if err != nil {
			return outcome(r, "delivery_unknown", "durable_result_unconfirmed_reuse_saved_operation", nil), nil
		}
		if upstream.IsError {
			return outcome(r, "tool_error", "upstream_tool_error_no_automatic_redispatch", upstream), nil
		}
		return outcome(r, "success", "", upstream), nil
	}
	upstream, err := session.CallTool(ctx, &mcp.CallToolParams{Name: b.config.Agent, Arguments: map[string]any{
		"run_id": r.RunID, "thread_id": r.ThreadID, "message": r.Message,
	}})
	if err != nil || upstream == nil {
		return outcome(r, "delivery_unknown", "delivery_failed_reuse_saved_operation", nil), nil
	}
	if upstream.IsError {
		return outcome(r, "tool_error", "upstream_tool_error_no_automatic_redispatch", upstream), nil
	}
	return outcome(r, "success", "", upstream), nil
}

func supportsRetry(ctx context.Context, session *mcp.ClientSession, agent string) error {
	var cursor string
	// Bound discovery even if a remote server emits a non-progressing cursor.
	for range 20 {
		page, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return errors.New("discovery failed")
		}
		for _, tool := range page.Tools {
			if tool.Name != agent {
				continue
			}
			raw, _ := json.Marshal(tool.InputSchema)
			var schema struct {
				Properties map[string]struct{ Type string }
			}
			if json.Unmarshal(raw, &schema) == nil && schema.Properties["run_id"].Type == "string" &&
				schema.Properties["thread_id"].Type == "string" && schema.Properties["message"].Type == "string" {
				return nil
			}
			return errors.New("retry schema unavailable")
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}
	return errors.New("configured agent unavailable")
}

type credentialTransport struct {
	base   http.RoundTripper
	origin *url.URL
	key    string
}

func (t *credentialTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != t.origin.Scheme || request.URL.Host != t.origin.Host {
		return nil, errors.New("destination changed")
	}
	r := request.Clone(request.Context())
	r.Header = request.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.key)
	return t.base.RoundTrip(r)
}

func checkRetainedRun(ctx context.Context, client *http.Client, base *url.URL, r *receipt) error {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/runs/"+url.PathEscape(r.RunID), nil)
	response, err := client.Do(request)
	if err != nil {
		return errors.New("observation failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("retained run unavailable")
	}
	var run struct {
		RunID    string          `json:"run_id"`
		AgentID  string          `json:"agent_id"`
		ThreadID string          `json:"thread_id"`
		Input    json.RawMessage `json:"input"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&run) != nil ||
		run.RunID != r.RunID || run.AgentID != r.AgentID || run.ThreadID != r.ThreadID {
		return errors.New("retained identity mismatch")
	}
	if !receiptInputMatches(run.Input, r) {
		return errors.New("retained input mismatch")
	}
	return nil
}

func observeRetainedRun(ctx context.Context, client *http.Client, base *url.URL, r *receipt) (*mcp.CallToolResult, error) {
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, base.String()+"/runs/"+url.PathEscape(r.RunID)+"/wait", nil)
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("observation failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("durable result unavailable")
	}
	var result struct {
		Run struct {
			RunID    string          `json:"run_id"`
			AgentID  string          `json:"agent_id"`
			ThreadID string          `json:"thread_id"`
			Status   string          `json:"status"`
			Error    string          `json:"error"`
			Input    json.RawMessage `json:"input"`
		} `json:"run"`
		Values json.RawMessage `json:"values"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) != nil || result.Run.RunID != r.RunID || result.Run.AgentID != r.AgentID || result.Run.ThreadID != r.ThreadID || !receiptInputMatches(result.Run.Input, r) {
		return nil, errors.New("durable result identity mismatch")
	}
	switch result.Run.Status {
	case "success":
		text, err := observedResultText(result.Values)
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	case "error", "interrupted", "timeout":
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Specialist run ended with status " + result.Run.Status + ". " + result.Run.Error}}}, nil
	default:
		return nil, errors.New("run has no durable terminal result")
	}
}

func receiptInputMatches(raw json.RawMessage, r *receipt) bool {
	input, err := jsonvalue.DecodeUnique(raw)
	expected := map[string]any{"messages": []any{map[string]any{"role": "user", "content": r.Message}}}
	return err == nil && jsonvalue.Equal(input, expected)
}

// Mirror internal/api/mcpserver.go's extractMCPResponseText contract for a
// read-only REST observation, without importing the control-plane server.
func observedResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var values map[string]interface{}
	if err := jsonvalue.Unmarshal(raw, &values); err != nil {
		return "", errors.New("invalid durable values")
	}
	if values == nil {
		return "", nil
	}
	if messages, ok := values["messages"].([]interface{}); ok && len(messages) > 0 {
		if last, ok := messages[len(messages)-1].(map[string]interface{}); ok {
			if text, ok := last["content"].(string); ok {
				return text, nil
			}
			encoded, err := json.Marshal(last)
			return string(encoded), err
		}
	}
	encoded, err := json.Marshal(values)
	return string(encoded), err
}
