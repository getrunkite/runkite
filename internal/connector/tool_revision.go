package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getrunkite/runkite/internal/jsonvalue"
)

var ErrToolRevisionUnavailable = errors.New("tool definition is unavailable")
var ErrToolRevisionChanged = errors.New("tool definition changed; fresh review is required")

type toolRevisionKey struct{}
type boundToolRevision struct{ name, revision string }

// BindMCPToolRevision reads the advertised definition only for an opted-in
// connector. It does not attest the server implementation or execute a tool.
func (r *Registry) BindMCPToolRevision(ctx context.Context, name, tool string, user map[string]interface{}) (context.Context, string, error) {
	target, err := r.requestTarget(ctx, name)
	if err != nil {
		return ctx, "", err
	}
	if !target.config.MCP.PinTools {
		return ctx, "", nil
	}
	revision, err := r.discoverToolRevision(ctx, target, tool, user)
	if err != nil {
		return ctx, "", err
	}
	return context.WithValue(ctx, toolRevisionKey{}, boundToolRevision{tool, revision}), revision, nil
}

func (r *Registry) recheckToolRevision(ctx context.Context, target *mcpRequestTarget, tool string, user map[string]interface{}) error {
	if !target.config.MCP.PinTools {
		return nil
	}
	bound, ok := ctx.Value(toolRevisionKey{}).(boundToolRevision)
	if !ok || bound.name != tool || bound.revision == "" {
		return ErrToolRevisionUnavailable
	}
	revision, err := r.discoverToolRevision(ctx, target, tool, user)
	if err != nil {
		return err
	}
	if revision != bound.revision {
		return ErrToolRevisionChanged
	}
	return nil
}

func (r *Registry) discoverToolRevision(ctx context.Context, target *mcpRequestTarget, tool string, user map[string]interface{}) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Discovery never consumes approval or records a protected tools/call send.
	ctx = WithMCPActionTrace(ctx, nil)
	if target.config.MCP.Transport == "streamable_http" {
		ctx = context.WithValue(ctx, rawToolCaptureKey{}, newRawToolCapture())
	}
	authorization, err := mcpAuthorization(ctx, target.connector, target.config, user, "tools/list")
	if err != nil {
		return "", ErrToolRevisionUnavailable
	}
	cursor := ""
	seenCursors := map[string]bool{"": true}
	seenTools := map[string]bool{}
	total := 0
	totalBytes := 0
	var selected map[string]any
	for range 32 {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "runkite-schema", "method": "tools/list", "params": params})
		req, err := ParseMCPRequest(body)
		if err != nil {
			return "", ErrToolRevisionUnavailable
		}
		var raw []byte
		var status int
		if target.config.MCP.Transport == "streamable_http" {
			raw, status, err = forwardStreamableMCPAuthorization(ctx, target.config.MCP.URL, authorization, req)
		} else {
			raw, status, err = forwardMCPRequestAuthorization(ctx, target.config.MCP.URL, authorization, body)
		}
		totalBytes += len(raw)
		if err != nil || status != 200 || int64(totalBytes) > MaxMCPResponseBytes {
			return "", ErrToolRevisionUnavailable
		}
		page, matched, err := parseRawToolPage(raw, req.ID)
		if err != nil || !matched {
			return "", ErrToolRevisionUnavailable
		}
		total += len(page.tools)
		if total > 4096 {
			return "", ErrToolRevisionUnavailable
		}
		for _, definition := range page.tools {
			name := definition["name"].(string)
			if seenTools[name] {
				return "", ErrToolRevisionUnavailable
			}
			seenTools[name] = true
			if name == tool {
				selected = definition
			}
		}
		if page.next == "" {
			if selected == nil {
				return "", ErrToolRevisionUnavailable
			}
			return toolDefinitionRevision(selected)
		}
		if seenCursors[page.next] {
			return "", ErrToolRevisionUnavailable
		}
		seenCursors[page.next] = true
		cursor = page.next
	}
	return "", ErrToolRevisionUnavailable
}

// findExactUpstreamTool reads every bounded page through the actual SDK session
// which will call the tool. Authority uses the matching raw response, including
// unknown annotations and exact numbers, rather than the SDK's lossy Tool value.
func findExactUpstreamTool(ctx context.Context, session *mcp.ClientSession, name string, bound boundToolRevision, capture *rawToolCapture) error {
	if capture == nil || bound.name != name || bound.revision == "" {
		return ErrToolRevisionUnavailable
	}
	cursor := ""
	seenCursors := map[string]bool{"": true}
	seenTools := map[string]bool{}
	total := 0
	var selected map[string]any
	for range 32 {
		typed, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return ErrToolRevisionUnavailable
		}
		page, err := capture.page(cursor)
		if err != nil || typed.NextCursor != page.next || len(typed.Tools) != len(page.tools) {
			return ErrToolRevisionUnavailable
		}
		total += len(page.tools)
		if total > 4096 {
			return ErrToolRevisionUnavailable
		}
		for i, definition := range page.tools {
			toolName := definition["name"].(string)
			if typed.Tools[i] == nil || typed.Tools[i].Name != toolName || seenTools[toolName] {
				return ErrToolRevisionUnavailable
			}
			seenTools[toolName] = true
			if toolName == name {
				selected = definition
			}
		}
		if page.next == "" {
			if selected == nil {
				return ErrToolRevisionUnavailable
			}
			revision, err := toolDefinitionRevision(selected)
			if err != nil {
				return err
			}
			if revision != bound.revision {
				return ErrToolRevisionChanged
			}
			return nil
		}
		if seenCursors[page.next] {
			return ErrToolRevisionUnavailable
		}
		seenCursors[page.next] = true
		cursor = page.next
	}
	return ErrToolRevisionUnavailable
}

func toolDefinitionRevision(definition map[string]any) (string, error) {
	raw, err := json.Marshal(canonicalToolDefinition(definition))
	if err != nil {
		return "", ErrToolRevisionUnavailable
	}
	digest := sha256.Sum256(raw)
	return "tool-v1-" + hex.EncodeToString(digest[:]), nil
}

func canonicalToolDefinition(v any) any {
	switch n := v.(type) {
	case json.Number:
		c, err := jsonvalue.CanonicalNumber(n)
		if err == nil {
			return c
		}
	case map[string]any:
		out := map[string]any{}
		for k, item := range n {
			out[k] = canonicalToolDefinition(item)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, item := range n {
			out[i] = canonicalToolDefinition(item)
		}
		return out
	}
	return v
}
