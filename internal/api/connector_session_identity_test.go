package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestConnectorSessionChecksTenantAndAgentWithOtherwiseMatchingBinding(t *testing.T) {
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorSessionStore(connector.NewMemoryConnectorSessionStore(0))
	capability, err := s.connectorSessions.Create("fixture", "same-run", 2, "acme", "agent")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, tenant, agent string
		allowed             bool
	}{
		{"matching", "acme", "agent", true},
		{"other_tenant", "beta", "agent", false},
		{"other_agent", "acme", "replacement-agent", false},
		{"other_identity", "beta", "replacement-agent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auth.WithRunBinding(context.Background(), &auth.RunBinding{RunID: "same-run", Generation: 2, TenantID: tc.tenant, AgentID: tc.agent})
			r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/mcp", nil).WithContext(ctx)
			r.Header.Set(connector.HeaderConnectorSession, capability.Token)
			w := httptest.NewRecorder()
			err := s.requireConnectorSession(w, r, "fixture")
			if tc.allowed {
				if err != nil {
					t.Fatalf("matching capability rejected: %v", err)
				}
			} else if err == nil || w.Code != http.StatusForbidden {
				t.Fatalf("capability crossed identity boundary: error=%v code=%d", err, w.Code)
			}
		})
	}
}
