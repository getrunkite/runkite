package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

	broker := inprocess.NewBroker()
	s := NewServer(nil, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{
			TenantID: "acme", AgentID: "sales", Connector: "sf",
			Tools: &policy.ToolFilter{Allow: []string{"query"}},
		}},
	}))
	s.SetPolicyRunEvents(true)

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := broker.Subscribe(subCtx, "run-1")
	if err != nil {
		t.Fatal(err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"updateRecord"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/mcp", strings.NewReader(body))
	req.SetPathValue("name", "sf")
	binding := &auth.RunBinding{
		RunID: "run-1", Generation: 1, TenantID: "acme", AgentID: "sales",
		User: &transport.UserContext{Identity: "alice"},
	}
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	attachConnectorSession(t, s, binding, "sf", req)
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

	select {
	case ev := <-ch:
		if ev.Method != "tool_auth" {
			t.Fatalf("method = %q, want tool_auth", ev.Method)
		}
		if !strings.HasPrefix(ev.EventID, "run-1_tool_auth_") {
			t.Fatalf("event_id = %q", ev.EventID)
		}
		var payload map[string]interface{}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["reason_code"] != policy.ReasonPolicyToolDenied || payload["tool"] != "updateRecord" || payload["connector"] != "sf" {
			t.Fatalf("payload = %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tool_auth RunEvent")
	}
}

func TestHandleGetSession_PolicyDeny(t *testing.T) {
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"sf": {Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"}},
	})
	broker := inprocess.NewBroker()
	s := NewServer(nil, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{TenantID: "acme", AgentID: "sales", Connector: "other"}},
	}))
	s.SetPolicyRunEvents(true)

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := broker.Subscribe(subCtx, "r")
	if err != nil {
		t.Fatal(err)
	}

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

	select {
	case ev := <-ch:
		if ev.Method != "tool_auth" {
			t.Fatalf("method = %q, want tool_auth", ev.Method)
		}
		var payload map[string]interface{}
		_ = json.Unmarshal(ev.Data, &payload)
		if payload["stage"] != policy.StageConnectorSession || payload["reason_code"] != policy.ReasonPolicyNoGrant {
			t.Fatalf("payload = %#v", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for tool_auth RunEvent")
	}
}

func TestHandleProxyMCP_PolicyDeny_RunEventsOff(t *testing.T) {
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"sf": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: "http://127.0.0.1:1"},
		},
	})
	broker := inprocess.NewBroker()
	s := NewServer(nil, inprocess.NewQueue(), broker, inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{
			TenantID: "acme", AgentID: "sales", Connector: "sf",
			Tools: &policy.ToolFilter{Allow: []string{"query"}},
		}},
	}))
	// policyRunEvents left false

	subCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := broker.Subscribe(subCtx, "run-off")
	if err != nil {
		t.Fatal(err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"updateRecord"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/mcp", strings.NewReader(body))
	req.SetPathValue("name", "sf")
	binding := &auth.RunBinding{
		RunID: "run-off", Generation: 1, TenantID: "acme", AgentID: "sales",
	}
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	attachConnectorSession(t, s, binding, "sf", req)
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	select {
	case ev := <-ch:
		t.Fatalf("unexpected RunEvent when run_events off: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}
