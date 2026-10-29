package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestMCPHandlerRejectsAmbiguityAndOversizeBeforeForwarding(t *testing.T) {
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{"files": {
		Auth: connector.AuthConfig{Type: "api_key", APIKey: "synthetic"}, MCP: &connector.MCPConfig{URL: "https://example.invalid/mcp"},
	}}))
	binding := &auth.RunBinding{RunID: "synthetic-run", Generation: 1, TenantID: "acme", AgentID: "files-agent"}
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"method alias", `{"jsonrpc":"2.0","method":"tools/call","METHOD":"ping","params":{"name":"delete"}}`, http.StatusBadRequest},
		{"name alias", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"delete","NAME":"read"}}`, http.StatusBadRequest},
		{"oversize", strings.Repeat("x", int(connector.MaxMCPRequestBytes)+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/internal/connectors/files/mcp", strings.NewReader(tc.body))
			req.SetPathValue("name", "files")
			req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
			attachConnectorSession(t, s, binding, "files", req)
			rec := httptest.NewRecorder()
			s.handleProxyMCPRequest(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
