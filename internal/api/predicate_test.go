package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestHandleProxyMCP_PredicateAmountAllow(t *testing.T) {
	var hits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(down.Close)

	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"bank": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: down.URL},
		},
	})
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank",
		}},
		Predicates: []policy.Predicate{{
			ID: "transfer-over-100", TenantID: "acme", AgentID: "payroll",
			Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(100),
			Effect: policy.EffectPending, ReasonCode: "predicate_amount",
		}},
	}))

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"transfer","arguments":{"amount":50}}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/bank/mcp", strings.NewReader(body))
	req.SetPathValue("name", "bank")
	binding := &auth.RunBinding{
		RunID: "run-pred-low", Generation: 1, TenantID: "acme", AgentID: "payroll",
		User: &transport.UserContext{Identity: "alice"},
	}
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	attachConnectorSession(t, s, binding, "bank", req)
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error  *json.RawMessage `json:"error"`
		Result json.RawMessage  `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("amount 50 should reach downstream, got error %s", rec.Body.String())
	}
	if hits.Load() != 1 {
		t.Fatalf("downstream hits=%d want 1", hits.Load())
	}
}

func TestPolicyPredicate_AmountPending(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — run via make smoke-governance")
	}
	ctx := context.Background()
	store, err := pgstore.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var hits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(down.Close)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	runID := "run-pred-high-" + suffix
	tenantID := "acme-pred-" + suffix

	eng := policy.New(policy.Config{
		Auditor: policy.NewStoreAuditor(store),
		Grants: []policy.Grant{{
			ID: "g", TenantID: tenantID, AgentID: "payroll", Connector: "bank",
		}},
		Predicates: []policy.Predicate{{
			ID: "transfer-over-100", TenantID: tenantID, AgentID: "payroll",
			Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(100),
			Effect:     policy.EffectPending,
			Reason:     "approval required for transfers over 100",
			ReasonCode: "predicate_amount",
		}},
	})
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"bank": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: down.URL},
		},
	})
	s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetPolicyEngine(eng)
	s.SetConnectorRegistry(reg)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"transfer","arguments":{"amount":250}}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/bank/mcp", strings.NewReader(body))
	req.SetPathValue("name", "bank")
	binding := &auth.RunBinding{
		RunID: runID, Generation: 1, TenantID: tenantID, AgentID: "payroll",
		User: &transport.UserContext{Identity: "alice"},
	}
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	attachConnectorSession(t, s, binding, "bank", req)
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error *struct {
			Code int                    `json:"code"`
			Data map[string]interface{} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != -32000 {
		t.Fatalf("want JSON-RPC -32000, got %s", rec.Body.String())
	}
	if resp.Error.Data["reason_code"] != "predicate_amount" || resp.Error.Data["effect"] != policy.EffectPending {
		t.Fatalf("data=%v", resp.Error.Data)
	}
	actionID, _ := resp.Error.Data["action_id"].(string)
	if actionID == "" {
		t.Fatalf("missing action_id in %v", resp.Error.Data)
	}
	t.Cleanup(func() {
		_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusApproved, models.PendingStatusDenied)
		_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusPending, models.PendingStatusDenied)
		_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusConsumed, models.PendingStatusDenied)
	})
	if hits.Load() != 0 {
		t.Fatalf("downstream must not be called on pending, hits=%d", hits.Load())
	}

	got, err := store.GetPendingAction(ctx, actionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.PendingStatusPending || got.Tool != "transfer" {
		t.Fatalf("pending row: %+v", got)
	}
	if got.ArgsDigest == "" {
		t.Fatalf("pending row missing args_digest: %+v", got)
	}
	if got.Args["amount"] != float64(250) {
		t.Fatalf("pending args=%#v", got.Args)
	}

	deadline := time.Now().Add(5 * time.Second)
	var found *models.AuditEvent
	for time.Now().Before(deadline) {
		events, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{
			TenantID: tenantID, RunID: runID, Limit: 20,
		})
		if err != nil {
			t.Fatalf("SearchAuditEvents: %v", err)
		}
		for _, ev := range events {
			if ev != nil && ev.ReasonCode == "predicate_amount" {
				found = ev
				break
			}
		}
		if found != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if found == nil {
		t.Fatal("missing audit row for predicate pending")
	}
	digest, _ := found.Attrs["args_digest"].(string)
	if digest == "" {
		t.Fatalf("audit attrs missing args_digest: %#v", found.Attrs)
	}
	args, _ := found.Attrs["args"].(map[string]interface{})
	if args["amount"] != float64(250) {
		t.Fatalf("audit args=%#v", found.Attrs["args"])
	}
}

func TestHandleProxyMCP_GrantDenyWinsOverPredicate(t *testing.T) {
	var hits atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(down.Close)

	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"bank": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: down.URL},
		},
	})
	s := NewServer(nil, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank",
			Tools: &policy.ToolFilter{Deny: []string{"transfer"}},
		}},
		Predicates: []policy.Predicate{{
			ID: "p", TenantID: "acme", Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(1), Effect: policy.EffectPending,
		}},
	}))

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"transfer","arguments":{"amount":250}}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/bank/mcp", strings.NewReader(body))
	req.SetPathValue("name", "bank")
	binding := &auth.RunBinding{
		RunID: "run-pred-deny", Generation: 1, TenantID: "acme", AgentID: "payroll",
	}
	req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
	attachConnectorSession(t, s, binding, "bank", req)
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	var resp struct {
		Error *struct {
			Code int                    `json:"code"`
			Data map[string]interface{} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Data["reason_code"] != policy.ReasonPolicyToolDenied {
		t.Fatalf("want grant deny, got %s", rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("downstream hits=%d", hits.Load())
	}
}
