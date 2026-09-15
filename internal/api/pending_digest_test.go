package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"github.com/getrunkite/runkite/internal/state"
	pgstore "github.com/getrunkite/runkite/internal/state/postgres"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestPendingHITL_ApprovedDigestMismatch_SQLite(t *testing.T) {
	store, err := sqlitestore.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	runApprovedDigestMismatch(t, store)
}

func TestPendingHITL_ApprovedDigestMismatch(t *testing.T) {
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
	runApprovedDigestMismatch(t, store)
}

func TestPendingHITL_ApproveGrantDenyStillRefuses(t *testing.T) {
	store, err := sqlitestore.New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int32
	s, admin, binding, call, parse := newBankTransferHITL(t, store, "acme-deny", "run-deny", &hits)
	st, b := call("500")
	if st != http.StatusOK {
		t.Fatalf("status %d body %s", st, b)
	}
	code, data := parse(b)
	if code != -32000 || data["effect"] != policy.EffectPending {
		t.Fatalf("want pending, got %s", b)
	}
	actionID, _ := data["action_id"].(string)
	if actionID == "" {
		t.Fatal("missing action_id")
	}

	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g", TenantID: binding.TenantID, AgentID: binding.AgentID, Connector: "bank",
			Tools: &policy.ToolFilter{Deny: []string{"transfer"}},
		}},
		Predicates: []policy.Predicate{{
			ID: "transfer-over-100", TenantID: binding.TenantID, AgentID: binding.AgentID,
			Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(100),
			Effect: policy.EffectPending, ReasonCode: "predicate_amount",
		}},
	}))

	appr, err := http.Post(admin.URL+"/admin-api/pending-actions/"+actionID+"/approve", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer appr.Body.Close()
	if appr.StatusCode != http.StatusConflict {
		ab, _ := io.ReadAll(appr.Body)
		t.Fatalf("approve want 409, got %d %s", appr.StatusCode, ab)
	}
	got, err := store.GetPendingAction(context.Background(), actionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.PendingStatusPending {
		t.Fatalf("status=%q want pending", got.Status)
	}
	if hits.Load() != 0 {
		t.Fatalf("downstream hits=%d", hits.Load())
	}
}

func runApprovedDigestMismatch(t *testing.T, store state.Store) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var hits atomic.Int32
	_, admin, _, call, parse := newBankTransferHITL(t, store, "acme-digest-"+suffix, "run-digest-"+suffix, &hits)

	pendingStore, ok := store.(pendingActionStore)
	if !ok {
		t.Fatal("store does not implement pending actions")
	}
	ctx := context.Background()

	st, b := call("500")
	if st != http.StatusOK {
		t.Fatalf("first call status %d body %s", st, b)
	}
	code, data := parse(b)
	if code != -32000 || data["effect"] != policy.EffectPending {
		t.Fatalf("first call want pending, got %s", b)
	}
	action500, _ := data["action_id"].(string)
	if action500 == "" {
		t.Fatal("missing action_id for $500")
	}
	t.Cleanup(func() {
		_ = pendingStore.SetPendingActionStatus(ctx, action500, models.PendingStatusApproved, models.PendingStatusDenied)
		_ = pendingStore.SetPendingActionStatus(ctx, action500, models.PendingStatusPending, models.PendingStatusDenied)
		_ = pendingStore.SetPendingActionStatus(ctx, action500, models.PendingStatusConsumed, models.PendingStatusDenied)
	})

	_, b = call("500")
	_, data = parse(b)
	reuse, _ := data["action_id"].(string)
	if reuse != action500 {
		t.Fatalf("same digest should reuse pending row, got %q want %q", reuse, action500)
	}

	_, b = call("5000000")
	code, data = parse(b)
	if code != -32000 || data["effect"] != policy.EffectPending {
		t.Fatalf("$5M call want pending, got %s", b)
	}
	action5m, _ := data["action_id"].(string)
	if action5m == "" || action5m == action500 {
		t.Fatalf("$5M must be its own pending row, got %q (500=%q)", action5m, action500)
	}
	t.Cleanup(func() {
		_ = pendingStore.SetPendingActionStatus(ctx, action5m, models.PendingStatusApproved, models.PendingStatusDenied)
		_ = pendingStore.SetPendingActionStatus(ctx, action5m, models.PendingStatusPending, models.PendingStatusDenied)
		_ = pendingStore.SetPendingActionStatus(ctx, action5m, models.PendingStatusConsumed, models.PendingStatusDenied)
	})

	row500, err := pendingStore.GetPendingAction(ctx, action500)
	if err != nil {
		t.Fatal(err)
	}
	if row500.ArgsDigest == "" || row500.Args["amount"] != float64(500) {
		t.Fatalf("$500 pending args: %+v", row500)
	}

	appr, err := http.Post(admin.URL+"/admin-api/pending-actions/"+action500+"/approve", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer appr.Body.Close()
	if appr.StatusCode != http.StatusOK {
		ab, _ := io.ReadAll(appr.Body)
		t.Fatalf("approve: %d %s", appr.StatusCode, ab)
	}

	_, b = call("5000000")
	code, data = parse(b)
	if code != -32000 || data["effect"] != policy.EffectPending {
		t.Fatalf("$5M after $500 approve must not forward, got %s", b)
	}
	if hits.Load() != 0 {
		t.Fatalf("downstream must not see $5M retry, hits=%d", hits.Load())
	}
	still, err := pendingStore.GetPendingAction(ctx, action500)
	if err != nil {
		t.Fatal(err)
	}
	if still.Status != models.PendingStatusApproved {
		t.Fatalf("$500 capability burned by $5M retry: status=%q", still.Status)
	}
	reused5m, _ := data["action_id"].(string)
	if reused5m != action5m {
		t.Fatalf("$5M should reuse its pending row, got %q want %q", reused5m, action5m)
	}

	_, b = call("500")
	code, data = parse(b)
	if code == -32000 && data["reason_code"] == "predicate_amount" {
		t.Fatalf("matching $500 retry still pending: %s", b)
	}
	if hits.Load() != 1 {
		t.Fatalf("matching retry should reach downstream once, hits=%d", hits.Load())
	}
	consumed, err := pendingStore.GetPendingAction(ctx, action500)
	if err != nil {
		t.Fatal(err)
	}
	if consumed.Status != models.PendingStatusConsumed {
		t.Fatalf("after matching retry status=%q want consumed", consumed.Status)
	}
	high, err := pendingStore.GetPendingAction(ctx, action5m)
	if err != nil {
		t.Fatal(err)
	}
	if high.Status != models.PendingStatusPending {
		t.Fatalf("$5M row status=%q want pending", high.Status)
	}
}

func newBankTransferHITL(t *testing.T, store state.Store, tenantID, runID string, hits *atomic.Int32) (
	s *Server, admin *httptest.Server, binding *auth.RunBinding,
	call func(amount string) (int, []byte),
	parse func([]byte) (int, map[string]interface{}),
) {
	t.Helper()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	t.Cleanup(down.Close)

	eng := policy.New(policy.Config{
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
	s = NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetPolicyEngine(eng)
	s.SetConnectorRegistry(reg)
	admin = httptest.NewServer(s.Handler())
	t.Cleanup(admin.Close)

	binding = &auth.RunBinding{
		RunID: runID, Generation: 1, TenantID: tenantID, AgentID: "payroll",
		User: &transport.UserContext{Identity: "alice"},
	}
	call = func(amount string) (int, []byte) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"transfer","arguments":{"amount":%s}}}`, amount)
		req := httptest.NewRequest(http.MethodPost, "/internal/connectors/bank/mcp", strings.NewReader(body))
		req.SetPathValue("name", "bank")
		req = req.WithContext(auth.WithRunBinding(req.Context(), binding))
		attachConnectorSession(t, s, binding, "bank", req)
		rec := httptest.NewRecorder()
		s.handleProxyMCPRequest(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	parse = func(b []byte) (int, map[string]interface{}) {
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
	return s, admin, binding, call, parse
}
