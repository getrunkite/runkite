// Package api: MCP-server support -- the reverse direction of the
// Connectors feature. Connectors let a Runkite AGENT consume external
// MCP tool servers (MCP-client direction); this file exposes Runkite's
// OWN configured agents AS MCP tools, so an external MCP client (Claude
// Desktop, Cursor, or any other MCP-speaking application) can call them
// directly.
//
// Mounted as a normal client-facing route (POST/GET/DELETE /mcp, not
// under /internal/*), so it goes through the exact same auth middleware
// (API key/JWT) every other client-facing endpoint does -- no separate
// auth mechanism to build or maintain. Each configured agent becomes one
// MCP tool; calling it dispatches a real run through the same
// createRunCtx + waitForRunResult path every client-facing
// create-and-wait endpoint already uses (the same pattern the internal
// A2A endpoint reuses for agent-to-agent calls), and waits for the
// result before responding -- MCP tool calls are inherently
// request/response, there's no streaming variant in the protocol to
// map onto Runkite's own SSE/WebSocket paths.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sharanharsoor/runkite/internal/auth"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// mcpToolArgs is the generic input shape every Runkite-agent-as-MCP-tool
// accepts. "message" (the common case) is wrapped into a single-user-
// turn LangGraph-style input; "input" is an escape hatch for a caller
// that already knows the target agent's own expected input shape, and
// wins if both are given.
type mcpToolArgs struct {
	Message  string          `json:"message,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	ThreadID string          `json:"thread_id,omitempty"`
}

// mcpToolInputSchema is a plain, hand-written JSON Schema object, the
// SAME shape for every agent -- not inferred via reflection the way the
// SDK's generic AddTool does for a fixed Go struct type. Agents are
// configured at runtime (loaded from langgraph.json, or created via the
// registry API), not known at compile time, so Server.AddTool's
// low-level form (a raw *Tool plus a ToolHandler working off raw JSON)
// is what's used here, not the generic one.
var mcpToolInputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"message": map[string]any{
			"type":        "string",
			"description": "Natural-language message to send to the agent, as a single user turn.",
		},
		"input": map[string]any{
			"type":        "object",
			"description": "Raw input object, for callers that already know this agent's own expected input shape. Overrides message if both are given.",
		},
		"thread_id": map[string]any{
			"type":        "string",
			"description": "Continue an existing conversation thread instead of starting a new one.",
		},
	},
}

// newMCPServer builds an *mcp.Server exposing every agent visible in ctx
// (SearchAgents is already tenant-scoped, the same as every other
// agent-listing call in this package) as one MCP tool each.
//
// ctx's tenant/identity are captured here, at MCP session-establishment
// time, and reused for every tool call made through the *mcp.Server this
// returns -- NOT ctx itself, which is tied to the single HTTP request
// that's establishing the session and will be cancelled once that
// request/response cycle ends, well before the session (and any later
// tools/call requests against it) is actually done. A stale, already-
// cancelled context would break every subsequent call on a long-lived
// session. This does mean a session's tenant is fixed for its lifetime
// -- the natural, expected behavior for how MCP clients are actually
// configured in practice (one static API key per configured MCP server
// entry, e.g. in Claude Desktop's config), not a limitation being worked
// around.
func (s *Server) newMCPServer(ctx context.Context) (*mcp.Server, error) {
	// Limit 1000: every configured agent becomes a tool, and this is a
	// SINGLE page, not paginated against SearchAgents -- a tenant with
	// more than 1000 registered agents (far beyond any real deployment
	// this project has been tested against) would silently see only
	// the first 1000 as MCP tools rather than an error. Worth revisiting
	// with real pagination if that ever becomes a real deployment's
	// actual scale, not a hypothetical one.
	agents, err := s.store.SearchAgents(ctx, &models.AgentSearchRequest{Limit: 1000})
	if err != nil {
		return nil, err
	}

	tenantID := tenant.FromContext(ctx)
	identity := auth.FromContext(ctx)

	server := mcp.NewServer(&mcp.Implementation{Name: "runkite", Version: "1.0.0"}, &mcp.ServerOptions{
		Instructions: "Each tool invokes one Runkite-configured agent. Pass \"message\" for a plain natural-language turn, or \"input\" if you already know the agent's own expected input shape. Pass \"thread_id\" to continue an existing conversation instead of starting a new one.",
	})
	for _, agent := range agents {
		description := agent.Description
		if description == "" {
			description = fmt.Sprintf("Invoke the %q agent.", agent.AgentID)
		}
		server.AddTool(&mcp.Tool{
			Name:        agent.AgentID,
			Description: description,
			InputSchema: mcpToolInputSchema,
		}, s.mcpToolHandler(agent.AgentID, tenantID, identity))
	}
	return server, nil
}

// mcpToolHandler dispatches one MCP tools/call to a real Runkite run
// against agentID, using the same createRunCtx + waitForRunResult path
// every client-facing create-and-wait endpoint already uses, and waits
// for the result -- MCP's tools/call is inherently request/response.
func (s *Server) mcpToolHandler(agentID, tenantID string, identity *auth.AuthResult) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args mcpToolArgs
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return mcpErrorResult(fmt.Sprintf("invalid arguments: %v", err)), nil
			}
		}
		if len(args.Input) == 0 && args.Message == "" {
			return mcpErrorResult(`either "message" or "input" is required`), nil
		}

		input := args.Input
		if len(input) == 0 {
			input, _ = json.Marshal(map[string]any{
				"messages": []map[string]any{{"role": "user", "content": args.Message}},
			})
		}

		// Rebuild tenant/identity onto the CURRENT call's own ctx (see
		// newMCPServer's doc comment for why the captured values, not
		// the captured context, are what's reused here).
		runCtx := tenant.WithContext(ctx, tenantID)
		if identity != nil {
			runCtx = auth.WithContext(runCtx, identity)
		}

		threadID := args.ThreadID
		if threadID == "" {
			threadID = "mcp-" + uuid.New().String()
		}

		run, assignment, err := s.createRunCtx(runCtx, threadID, &models.RunCreate{
			AgentID: agentID,
			Input:   input,
		})
		if err != nil {
			return mcpErrorResult(fmt.Sprintf("failed to start agent %q: %v", agentID, err)), nil
		}

		resp, err := s.waitForRunResult(runCtx, run, assignment)
		if err != nil {
			return mcpErrorResult(fmt.Sprintf("agent %q run failed: %v", agentID, err)), nil
		}
		if resp.Run.Status == models.RunStatusError {
			return mcpErrorResult(fmt.Sprintf("agent %q run errored: %s", agentID, resp.Run.Error)), nil
		}
		if resp.Run.Status == models.RunStatusInterrupted {
			return mcpErrorResult(fmt.Sprintf("agent %q run was interrupted before completing", agentID)), nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: extractMCPResponseText(resp.Values)}},
		}, nil
	}
}

// mcpErrorResult reports a tool-level failure -- per Tool.IsError's own
// doc comment, this belongs in Content with IsError set, NOT as a
// protocol-level (Go) error return, so the calling LLM can actually see
// what went wrong and self-correct instead of the MCP client just
// surfacing an opaque RPC failure.
func mcpErrorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// extractMCPResponseText pulls a clean, displayable string out of a
// run's final Values -- the LAST message's content, for the common
// LangGraph messages-shaped output every built-in example agent
// produces. Falls back to the raw JSON of whatever Values actually
// contains for an agent with a different output shape, rather than
// guessing wrong or returning nothing.
func extractMCPResponseText(values map[string]interface{}) string {
	if values == nil {
		return ""
	}
	msgs, ok := values["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		b, _ := json.Marshal(values)
		return string(b)
	}
	last, ok := msgs[len(msgs)-1].(map[string]interface{})
	if !ok {
		b, _ := json.Marshal(values)
		return string(b)
	}
	if content, ok := last["content"].(string); ok {
		return content
	}
	b, _ := json.Marshal(last)
	return string(b)
}

// mcpHTTPHandler serves the Streamable HTTP MCP transport at /mcp,
// backward-compatible across every MCP protocol revision the SDK
// supports (2024-11-05 through 2026-07-28) rather than the newest
// stateless-only mode, since real-world MCP clients (Claude Desktop,
// Cursor, etc.) predominantly still speak the older, session-based
// protocol -- see this file's own package doc comment for why /mcp is
// mounted as a normal client-facing route rather than under /internal/*.
//
// Wrapped in mcpSessionOwner's own middleware -- see its doc comment
// for a real, confirmed cross-tenant hijack this closes that the SDK's
// own built-in protection does not. SessionTimeout is set explicitly
// (not left at the SDK's own default of "never") -- see
// mcpSDKSessionTimeout's own doc comment for why that specific value,
// not just any positive one, matters for this middleware's own
// correctness.
func (s *Server) mcpHTTPHandler() http.Handler {
	base := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		server, err := s.newMCPServer(r.Context())
		if err != nil {
			slog.Error("mcp: failed to build server", "error", err)
			return nil
		}
		return server
	}, &mcp.StreamableHTTPOptions{SessionTimeout: mcpSDKSessionTimeout})
	return newMCPSessionOwner(mcpSessionTTL, mcpSessionSweepInterval).middleware(base)
}

// --------------------------------------------------------------------------
// Session-to-caller binding
// --------------------------------------------------------------------------

// mcpSessionIDHeader is the header name the SDK itself uses to carry a
// session ID once one is established -- see the SDK's own
// streamable_headers.go. Read here (not exported by the SDK) so this
// middleware can inspect the same value the SDK's internal session
// routing keys off of.
const mcpSessionIDHeader = "Mcp-Session-Id"

const (
	// mcpSessionTTL bounds how long an established session's ownership
	// record is kept without being touched again before this process's
	// own sweep evicts it. Deliberately NOT independent of the SDK's
	// own idle-session timeout below -- see mcpSDKSessionTimeout's own
	// doc comment for why the two must stay in this specific order.
	mcpSessionTTL = 30 * time.Minute

	mcpSessionSweepInterval = 5 * time.Minute

	// mcpSDKSessionTimeout is passed as the MCP SDK's own
	// StreamableHTTPOptions.SessionTimeout, so the underlying SDK
	// session itself closes strictly BEFORE this process's ownership
	// record for it could ever be swept away.
	//
	// This isn't just "pick some positive value" -- a real,
	// live-confirmed gap: leaving this at the SDK's own default (zero
	// = idle sessions never close) meant a session could still be
	// alive and answering requests long after mcpSessionOwner's own
	// ownership record for it had been swept at mcpSessionTTL. Once
	// that happens, an unrecognized (evicted) session ID is
	// deliberately treated as "pass through, let the SDK's routing
	// decide" (see the middleware's own doc comment on why an unknown
	// ID isn't outright rejected) -- which reopens the exact
	// cross-tenant hijack window this middleware exists to close,
	// just after mcpSessionTTL of activity instead of never.
	//
	// The margin below mcpSessionTTL (a full sweep interval, not just
	// one tick less) matters too: an ownership record idle for exactly
	// mcpSessionTTL isn't necessarily evicted the instant it crosses
	// that threshold, only at the sweep's next tick, up to
	// mcpSessionSweepInterval later -- the SDK session needs to already
	// be closed by THAT point, not merely by mcpSessionTTL itself.
	mcpSDKSessionTimeout = mcpSessionTTL - mcpSessionSweepInterval
)

// mcpSessionOwner records which caller (tenant + identity) established
// each active MCP session, so a later request presenting a DIFFERENT
// caller's otherwise-valid credentials but the SAME session ID is
// rejected instead of silently continuing the original caller's
// session.
//
// This closes a real, live-confirmed gap: the MCP Go SDK has its own
// session-hijack protection, but it keys off an OAuth-specific
// TokenInfo.UserID field that none of Runkite's own auth providers
// (API key, JWT) ever populate -- so that check is silently a no-op
// here. Confirmed live: establish a session with one tenant's API key,
// then call tools/call presenting a SECOND, entirely different (but
// otherwise perfectly valid) tenant's API key alongside the FIRST
// session's leaked or guessed ID -- without this middleware, the call
// succeeds and dispatches a run under the ORIGINAL (wrong) tenant, not
// the caller who actually made this specific request.
type mcpSessionOwner struct {
	mu       sync.Mutex
	sessions map[string]mcpSessionEntry
	ttl      time.Duration
}

type mcpSessionEntry struct {
	caller   string
	lastSeen time.Time
}

// newMCPSessionOwner takes ttl/sweepInterval as parameters (rather than
// reading the package constants directly) so a test can exercise the
// sweep/timeout interaction on a realistic timescale instead of either
// waiting on mcpSessionTTL for real or only checking the constants'
// arithmetic in isolation.
func newMCPSessionOwner(ttl, sweepInterval time.Duration) *mcpSessionOwner {
	o := &mcpSessionOwner{sessions: make(map[string]mcpSessionEntry), ttl: ttl}
	go o.sweepLoop(sweepInterval)
	return o
}

// mcpCallerKey identifies "who is making this specific HTTP request,"
// derived the same way every tool call's own tenant/identity capture
// is (see newMCPServer's doc comment) -- tenant alone would still let
// two DIFFERENT identities within the same tenant hijack each other's
// sessions, so identity is part of the binding too.
func mcpCallerKey(ctx context.Context) string {
	identity := ""
	if a := auth.FromContext(ctx); a != nil {
		identity = a.Identity
	}
	return tenant.FromContext(ctx) + "|" + identity
}

// middleware checks the incoming request's own caller against whoever
// established the session named in its Mcp-Session-Id header (if any),
// BEFORE the wrapped MCP handler ever runs -- this must happen at the
// HTTP layer, not inside a tool handler, because a tool handler only
// ever sees the caller who created the session (see newMCPServer's own
// doc comment for why), never the CURRENT request's actual caller.
func (o *mcpSessionOwner) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller := mcpCallerKey(r.Context())
		sessionID := r.Header.Get(mcpSessionIDHeader)

		if sessionID != "" {
			o.mu.Lock()
			entry, exists := o.sessions[sessionID]
			o.mu.Unlock()
			if exists && entry.caller != caller {
				writeError(w, http.StatusForbidden, "mcp session belongs to a different caller")
				return
			}
		}

		next.ServeHTTP(w, r)

		// A brand-new session's ID is only known AFTER the wrapped
		// handler runs -- the SDK assigns it and sets this same
		// response header on the "initialize" call. Record ownership
		// then; for an existing session's later request, this just
		// refreshes lastSeen for the sweep below.
		if newID := w.Header().Get(mcpSessionIDHeader); newID != "" {
			o.mu.Lock()
			o.sessions[newID] = mcpSessionEntry{caller: caller, lastSeen: time.Now()}
			o.mu.Unlock()
		} else if sessionID != "" {
			o.mu.Lock()
			if entry, exists := o.sessions[sessionID]; exists {
				entry.lastSeen = time.Now()
				o.sessions[sessionID] = entry
			}
			o.mu.Unlock()
		}

		// DELETE is how the Streamable HTTP transport's client
		// explicitly closes a session -- release the ownership record
		// with it rather than waiting for the sweep below.
		if r.Method == http.MethodDelete && sessionID != "" {
			o.mu.Lock()
			delete(o.sessions, sessionID)
			o.mu.Unlock()
		}
	})
}

// sweepOnce evicts ownership records untouched since before cutoff,
// split out from sweepLoop so a test can call it directly with an
// artificial "now" instead of waiting on the real ticker.
func (o *mcpSessionOwner) sweepOnce(now time.Time) {
	cutoff := now.Add(-o.ttl)
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, entry := range o.sessions {
		if entry.lastSeen.Before(cutoff) {
			delete(o.sessions, id)
		}
	}
}

func (o *mcpSessionOwner) sweepLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		o.sweepOnce(time.Now())
	}
}
