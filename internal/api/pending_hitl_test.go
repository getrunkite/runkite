package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestPendingHITL_ApproveOneShotRetry(t *testing.T) {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		t.Skip("POSTGRES_DSN not set — skipping pending HITL tests")
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

	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"effect":"pending","reason":"needs human approval","rule_id":"hitl-delete"}`))
	}))
	t.Cleanup(webhook.Close)

	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
		}},
		Webhook: &policy.WebhookConfig{URL: webhook.URL},
	})
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"gh": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: "http://127.0.0.1:1"},
		},
	})
	s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetPolicyEngine(eng)
	s.SetConnectorRegistry(reg)
	admin := httptest.NewServer(s.Handler())
	t.Cleanup(admin.Close)

	binding := &auth.RunBinding{
		RunID: "run-hitl-1", Generation: 3, TenantID: "acme", AgentID: "sales",
		User: &transport.UserContext{Identity: "alice"},
	}
	callMCP := func() (status int, body []byte) {
		reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_repo"}}`
		req := httptest.NewRequest(http.MethodPost, "/internal/connectors/gh/mcp", strings.NewReader(reqBody))
		req.SetPathValue("name", "gh")
		req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
		rec := httptest.NewRecorder()
		s.handleProxyMCPRequest(rec, req)
		return rec.Code, rec.Body.Bytes()
	}

	parseRPCError := func(b []byte) (code int, data map[string]interface{}) {
		var resp struct {
			Error *struct {
				Code int                    `json:"code"`
				Data map[string]interface{} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, b)
		}
		if resp.Error == nil {
			return 0, nil
		}
		return resp.Error.Code, resp.Error.Data
	}

	st, b := callMCP()
	if st != http.StatusOK {
		t.Fatalf("first call status %d body %s", st, b)
	}
	code, data := parseRPCError(b)
	if code != -32000 {
		t.Fatalf("first call want -32000, got code=%d body=%s", code, b)
	}
	if data["reason_code"] != policy.ReasonPolicyPending || data["effect"] != policy.EffectPending {
		t.Fatalf("first call data=%v", data)
	}
	actionID, _ := data["action_id"].(string)
	if actionID == "" {
		t.Fatalf("missing action_id in %v", data)
	}
	t.Cleanup(func() {
		_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusApproved, models.PendingStatusDenied)
		_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusPending, models.PendingStatusDenied)
		_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusConsumed, models.PendingStatusDenied)
	})

	appr, err := http.Post(admin.URL+"/admin-api/pending-actions/"+actionID+"/approve", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer appr.Body.Close()
	if appr.StatusCode != http.StatusOK {
		ab, _ := io.ReadAll(appr.Body)
		t.Fatalf("approve: %d %s", appr.StatusCode, ab)
	}
	var approved models.PendingAction
	if err := json.NewDecoder(appr.Body).Decode(&approved); err != nil {
		t.Fatal(err)
	}
	if approved.Status != models.PendingStatusApproved {
		t.Fatalf("status=%q want approved", approved.Status)
	}

	st, b = callMCP()
	code, data = parseRPCError(b)
	if code == -32000 && data["reason_code"] == policy.ReasonPolicyPending {
		t.Fatalf("approved retry still pending: %s", b)
	}
	if st == http.StatusOK && code == -32000 {
		t.Fatalf("approved retry still policy-denied: %s", b)
	}

	got, err := store.GetPendingAction(ctx, actionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.PendingStatusConsumed {
		t.Fatalf("after retry status=%q want consumed", got.Status)
	}

	st, b = callMCP()
	if st != http.StatusOK {
		t.Fatalf("third call status %d body %s", st, b)
	}
	code, data = parseRPCError(b)
	if code != -32000 || data["reason_code"] != policy.ReasonPolicyPending {
		t.Fatalf("third call want pending again, got code=%d data=%v body=%s", code, data, b)
	}
	newID, _ := data["action_id"].(string)
	if newID == "" || newID == actionID {
		t.Fatalf("third call should allocate a new action_id, got %q (old %q)", newID, actionID)
	}
	t.Cleanup(func() {
		_ = store.SetPendingActionStatus(context.Background(), newID, models.PendingStatusPending, models.PendingStatusDenied)
	})
}
