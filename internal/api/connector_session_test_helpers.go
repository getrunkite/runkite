package api

import (
	"net/http"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
)

// ensureConnectorSessions wires a memory store when tests omit one.
func ensureConnectorSessions(s *Server) {
	if s.connectorSessions == nil {
		s.SetConnectorSessionStore(connector.NewMemoryConnectorSessionStore(0))
	}
}

// attachConnectorSession mints a capability token for binding+name and
// sets X-Runkite-Connector-Session on req.
func attachConnectorSession(t *testing.T, s *Server, binding *auth.RunBinding, name string, req *http.Request) {
	t.Helper()
	ensureConnectorSessions(s)
	cap, err := s.connectorSessions.Create(name, binding.RunID, binding.Generation, binding.TenantID, binding.AgentID)
	if err != nil {
		t.Fatalf("mint connector session: %v", err)
	}
	req.Header.Set(connector.HeaderConnectorSession, cap.Token)
}
