package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestHandleProxyMCP_PolicyDeny(t *testing.T) {
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"sf": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: "http://127.0.0.1:1"},
		},
	})

	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{
			TenantID: "acme", AgentID: "sales", Connector: "sf",
			Tools: &policy.ToolFilter{Allow: []string{"query"}},
		}},
	}))

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"updateRecord"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/mcp", strings.NewReader(body))
	req.SetPathValue("name", "sf")
	ctx := auth.WithRunBinding(req.Context(), &auth.RunBinding{
		RunID: "run-1", Generation: 1, TenantID: "acme", AgentID: "sales",
		User: &transport.UserContext{Identity: "alice"},
	})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Fatalf("want JSON-RPC -32000, got %+v", resp.Error)
	}
	var data map[string]string
	_ = json.Unmarshal(resp.Error.Data, &data)
	if data["reason_code"] != policy.ReasonPolicyToolDenied {
		t.Fatalf("reason_code=%q data=%v", data["reason_code"], data)
	}
}

func TestHandleGetSession_PolicyDeny(t *testing.T) {
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"sf": {Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"}},
	})
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{TenantID: "acme", AgentID: "sales", Connector: "other"}},
	}))

	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/session", nil)
	req.SetPathValue("name", "sf")
	req = req.WithContext(auth.WithRunBinding(context.Background(), &auth.RunBinding{
		RunID: "r", Generation: 1, TenantID: "acme", AgentID: "sales",
	}))
	rec := httptest.NewRecorder()
	s.handleGetConnectorSession(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["reason_code"] != policy.ReasonPolicyNoGrant {
		t.Fatalf("%v", body)
	}
}
