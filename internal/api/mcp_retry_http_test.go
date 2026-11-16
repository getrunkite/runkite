package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/getrunkite/runkite/internal/auth"
)

type mcpRetryHTTPAuthProvider struct{ revokeWrite atomic.Bool }

func (p *mcpRetryHTTPAuthProvider) Authenticate(_ *http.Request) (*auth.AuthResult, error) {
	permissions := []string{"read", "write"}
	if p.revokeWrite.Load() {
		permissions = []string{"read"}
	}
	return &auth.AuthResult{Identity: "same-client", TenantID: "default", Permissions: permissions}, nil
}

func TestMCPRetryHTTPGateRejectsRevokedWriteOnExistingSession(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "mcp-http-retry", "mcp-http-retry", nil)
	provider := &mcpRetryHTTPAuthProvider{}
	srv := httptest.NewServer(auth.Middleware(provider, nil, nil, env.apiServer.Handler()))
	t.Cleanup(srv.Close)
	session := mcpConnect(t, srv.URL, nil)
	runMCPAgentInBackground(t, env, "durable result")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	call := &mcp.CallToolParams{Name: "mcp-http-retry", Arguments: map[string]any{"run_id": "mcp-http-authority", "message": "same"}}
	first, err := session.CallTool(ctx, call)
	if err != nil || first.IsError {
		t.Fatalf("initial authorized call failed: %+v %v", first, err)
	}
	// The SDK can retain its initialization context. Revocation is therefore
	// checked at the real HTTP boundary, not inferred from a direct handler.
	provider.revokeWrite.Store(true)
	if result, err := session.CallTool(ctx, call); err == nil {
		t.Fatalf("revoked POST permission reached an established MCP session: %+v", result)
	}
	if duplicate, err := env.queue.Dequeue(context.Background(), "python-langgraph", 25*time.Millisecond); err != nil || duplicate != nil {
		t.Fatalf("revoked retry enqueued work: %+v %v", duplicate, err)
	}
}
