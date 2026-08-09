package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
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

type govInflight map[string]*transport.RunAssignment

func (m govInflight) LookupInflight(_ context.Context, runID string) (*transport.RunAssignment, error) {
	return m[runID], nil
}

// TestGovernanceAnnounceBar proves security roadmap Phase 0–2 exit criteria
// on Supported Postgres in one live path (invoked by make smoke-governance).
//
//	Phase 0: unbound store/session/vector → run_binding_required; bound
//	         tenant comes from the assignment, not a forged header.
//	Phase 1: tenant B denied on connector Y → wire deny + durable audit row.
//	Phase 2: Admin audit search returns that deny; mandatory HITL pending
//	         → approve → one-shot retry consumes; second call pending again.
func TestGovernanceAnnounceBar(t *testing.T) {
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

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	tenantB := "gov-beta-" + suffix
	tenantA := "gov-acme-" + suffix
	runDeny := "gov-deny-" + suffix
	runHITL := "gov-hitl-" + suffix
	connectorY := "salesforce"

	t.Run("phase0_run_binding", func(t *testing.T) {
		inflight := govInflight{
			"run-bound": {RunID: "run-bound", Generation: 2, TenantID: "acme", GraphID: "echo"},
		}
		var gotTenant string
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotTenant = tenant.FromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})
		handler := auth.MiddlewareWithOpts(nil, nil, nil, auth.MiddlewareOpts{Inflight: inflight}, next)

		for _, path := range []string{
			"/internal/store/items",
			"/internal/connectors/" + connectorY + "/session",
			"/internal/vectors/search",
		} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s unbound: status %d want 401 body=%s", path, rec.Code, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s unmarshal: %v", path, err)
			}
			if body["reason_code"] != auth.ReasonRunBindingRequired {
				t.Fatalf("%s reason_code=%q want %s", path, body["reason_code"], auth.ReasonRunBindingRequired)
			}
		}

		gotTenant = ""
		req := httptest.NewRequest(http.MethodGet, "/internal/store/items", nil)
		req.Header.Set(auth.HeaderRunID, "run-bound")
		req.Header.Set(auth.HeaderGeneration, "2")
		req.Header.Set(auth.HeaderTenantID, "evil")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("bound store: status %d body %s", rec.Code, rec.Body.String())
		}
		if gotTenant != "acme" {
			t.Fatalf("tenant=%q want acme from assignment (forged header ignored)", gotTenant)
		}
	})

	t.Run("phase1_deny_plus_audit", func(t *testing.T) {
		reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
			connectorY: {Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"}},
		})
		eng := policy.New(policy.Config{
			Auditor: policy.NewStoreAuditor(store),
			Grants: []policy.Grant{{
				ID: "grant-acme", TenantID: tenantA, AgentID: "sales", Connector: connectorY,
			}},
		})
		s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
		s.SetConnectorRegistry(reg)
		s.SetPolicyEngine(eng)

		req := httptest.NewRequest(http.MethodPost, "/internal/connectors/"+connectorY+"/session", nil)
		req.SetPathValue("name", connectorY)
		req = req.WithContext(auth.WithRunBinding(context.Background(), &auth.RunBinding{
			RunID: runDeny, Generation: 1, TenantID: tenantB, AgentID: "sales",
			User: &transport.UserContext{Identity: "bob"},
		}))
		rec := httptest.NewRecorder()
		s.handleGetConnectorSession(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("tenant B session: status %d body %s", rec.Code, rec.Body.String())
		}
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["reason_code"] != policy.ReasonPolicyNoGrant {
			t.Fatalf("reason_code=%q want %s body=%v", body["reason_code"], policy.ReasonPolicyNoGrant, body)
		}

		deadline := time.Now().Add(5 * time.Second)
		var found *models.AuditEvent
		for time.Now().Before(deadline) {
			events, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{
				TenantID: tenantB, Decision: policy.EffectDeny, Connector: connectorY, Limit: 20,
			})
			if err != nil {
				t.Fatalf("SearchAuditEvents: %v", err)
			}
			for _, ev := range events {
				if ev.RunID == runDeny && ev.ReasonCode == policy.ReasonPolicyNoGrant &&
					ev.Action == policy.StageConnectorSession {
					found = ev
					break
				}
			}
			if found != nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if found == nil {
			t.Fatal("expected durable audit deny row for tenant B connector session")
		}
	})

	t.Run("phase2_audit_query_and_hitl", func(t *testing.T) {
		adminURL := ""
		reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
			"gh": {
				Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
				MCP:  &connector.MCPConfig{URL: "http://127.0.0.1:1"},
			},
		})
		eng := policy.New(policy.Config{
			Auditor: policy.NewStoreAuditor(store),
			Grants: []policy.Grant{{
				ID: "grant-hitl", TenantID: tenantA, AgentID: "sales", Connector: "gh",
			}},
			MandatoryHITL: []policy.MandatoryHITLRule{{
				ID: "force-delete", TenantID: tenantA, Connector: "gh",
				Tools: []string{"delete_repo"},
			}},
		})
		s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
		s.SetConnectorRegistry(reg)
		s.SetPolicyEngine(eng)
		admin := httptest.NewServer(s.Handler())
		t.Cleanup(admin.Close)
		adminURL = admin.URL

		// Phase 2a: auditor query for Phase 1 deny via Admin API.
		since := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
		listURL := fmt.Sprintf("%s/admin-api/audit-events?tenant_id=%s&decision=deny&since=%s&limit=50",
			adminURL, url.QueryEscape(tenantB), url.QueryEscape(since))
		aresp, err := http.Get(listURL)
		if err != nil {
			t.Fatal(err)
		}
		defer aresp.Body.Close()
		if aresp.StatusCode != http.StatusOK {
			ab, _ := io.ReadAll(aresp.Body)
			t.Fatalf("audit-events: %d %s", aresp.StatusCode, ab)
		}
		var events []models.AuditEvent
		if err := json.NewDecoder(aresp.Body).Decode(&events); err != nil {
			t.Fatal(err)
		}
		foundDeny := false
		for _, ev := range events {
			if ev.RunID == runDeny && ev.ReasonCode == policy.ReasonPolicyNoGrant {
				foundDeny = true
				break
			}
		}
		if !foundDeny {
			t.Fatalf("Admin audit search missing Phase 1 deny (got %d events)", len(events))
		}

		// Phase 2b: mandatory HITL pending → approve → one-shot → pending again.
		binding := &auth.RunBinding{
			RunID: runHITL, Generation: 1, TenantID: tenantA, AgentID: "sales",
			User: &transport.UserContext{Identity: "alice"},
		}
		callMCP := func() (int, []byte) {
			reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_repo"}}`
			req := httptest.NewRequest(http.MethodPost, "/internal/connectors/gh/mcp", strings.NewReader(reqBody))
			req.SetPathValue("name", "gh")
			req = req.WithContext(auth.WithRunBinding(context.Background(), binding))
			attachConnectorSession(t, s, binding, "gh", req)
			rec := httptest.NewRecorder()
			s.handleProxyMCPRequest(rec, req)
			return rec.Code, rec.Body.Bytes()
		}
		parseRPC := func(b []byte) (code int, data map[string]interface{}) {
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
			t.Fatalf("first HITL call status %d body %s", st, b)
		}
		code, data := parseRPC(b)
		if code != -32000 || data["reason_code"] != policy.ReasonMandatoryHITL {
			t.Fatalf("first HITL want mandatory_hitl pending, got code=%d data=%v", code, data)
		}
		actionID, _ := data["action_id"].(string)
		if actionID == "" {
			t.Fatal("missing action_id")
		}
		t.Cleanup(func() {
			_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusApproved, models.PendingStatusDenied)
			_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusPending, models.PendingStatusDenied)
			_ = store.SetPendingActionStatus(context.Background(), actionID, models.PendingStatusConsumed, models.PendingStatusDenied)
		})

		appr, err := http.Post(adminURL+"/admin-api/pending-actions/"+actionID+"/approve", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer appr.Body.Close()
		if appr.StatusCode != http.StatusOK {
			ab, _ := io.ReadAll(appr.Body)
			t.Fatalf("approve: %d %s", appr.StatusCode, ab)
		}

		st2, b2 := callMCP()
		if st2 == http.StatusOK {
			code2, data2 := parseRPC(b2)
			if code2 == -32000 {
				if rc, _ := data2["reason_code"].(string); rc == policy.ReasonMandatoryHITL || rc == policy.ReasonPolicyPending {
					t.Fatalf("approved retry still policy-gated: %s", b2)
				}
			}
		} else if st2 != http.StatusBadGateway {
			t.Fatalf("approved retry: want passthrough (502 from dead MCP ok), got %d %s", st2, b2)
		}
		got, err := store.GetPendingAction(ctx, actionID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != models.PendingStatusConsumed {
			t.Fatalf("after retry status=%q want consumed", got.Status)
		}

		st3, b3 := callMCP()
		if st3 != http.StatusOK {
			t.Fatalf("third call status %d body %s", st3, b3)
		}
		code3, data3 := parseRPC(b3)
		if code3 != -32000 || data3["reason_code"] != policy.ReasonMandatoryHITL {
			t.Fatalf("third call want pending again, got code=%d data=%v", code3, data3)
		}
		newID, _ := data3["action_id"].(string)
		if newID == "" || newID == actionID {
			t.Fatalf("third call should allocate new action_id, got %q (old %q)", newID, actionID)
		}
		t.Cleanup(func() {
			_ = store.SetPendingActionStatus(context.Background(), newID, models.PendingStatusPending, models.PendingStatusDenied)
		})
	})
}
