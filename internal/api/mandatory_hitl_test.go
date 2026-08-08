package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestMandatoryHITL_MCPPendingThenApprove(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	// Downstream MCP is never reached on first call (pending). After
	// approve, ProxyMCPRequest hits 127.0.0.1:1 — that's fine; we only
	// assert the policy gate no longer returns pending/deny.
	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"gh": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: "http://127.0.0.1:1"},
		},
	})
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
		}},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			ID: "force-delete", TenantID: "acme", Connector: "gh",
			Tools: []string{"delete_repo"},
		}},
	})
	s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetPolicyEngine(eng)
	s.SetConnectorRegistry(reg)
	admin := httptest.NewServer(s.Handler())
	t.Cleanup(admin.Close)

	binding := &auth.RunBinding{
		RunID: "run-mhitl-1", Generation: 1, TenantID: "acme", AgentID: "sales",
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

	st, b := callMCP()
	if st != http.StatusOK {
		t.Fatalf("first call status %d body %s", st, b)
	}
	var resp struct {
		Error *struct {
			Code int                    `json:"code"`
			Data map[string]interface{} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &resp); err != nil || resp.Error == nil {
		t.Fatalf("unmarshal: %v body=%s", err, b)
	}
	if resp.Error.Code != -32000 {
		t.Fatalf("code=%d", resp.Error.Code)
	}
	if resp.Error.Data["reason_code"] != policy.ReasonMandatoryHITL ||
		resp.Error.Data["effect"] != policy.EffectPending {
		t.Fatalf("data=%v", resp.Error.Data)
	}
	actionID, _ := resp.Error.Data["action_id"].(string)
	if actionID == "" {
		t.Fatal("missing action_id")
	}

	rows, err := store.SearchPendingActions(tenant.SystemContext(ctx), &models.PendingActionSearchRequest{
		TenantID: "acme", Limit: 10,
	})
	if err != nil || len(rows) == 0 {
		t.Fatalf("pending row: %#v err=%v", rows, err)
	}

	appr, err := http.Post(admin.URL+"/admin-api/pending-actions/"+actionID+"/approve", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer appr.Body.Close()
	if appr.StatusCode != http.StatusOK {
		ab, _ := io.ReadAll(appr.Body)
		t.Fatalf("approve: %d %s", appr.StatusCode, ab)
	}

	st2, b2 := callMCP()
	// Downstream MCP is intentionally dead — we only care that the
	// policy gate let the call through (not another pending/deny).
	if st2 == http.StatusOK {
		var resp2 struct {
			Error *struct {
				Code int                    `json:"code"`
				Data map[string]interface{} `json:"data"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b2, &resp2)
		if resp2.Error != nil {
			if rc, _ := resp2.Error.Data["reason_code"].(string); rc == policy.ReasonMandatoryHITL ||
				rc == policy.ReasonPolicyPending {
				t.Fatalf("approved retry still policy-gated: %s", b2)
			}
		}
	} else if st2 != http.StatusBadGateway {
		t.Fatalf("retry: want policy passthrough (502 from dead MCP ok), got %d %s", st2, b2)
	}

	got, err := store.GetPendingAction(ctx, actionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != models.PendingStatusConsumed {
		t.Fatalf("after retry status=%q want consumed", got.Status)
	}
}
