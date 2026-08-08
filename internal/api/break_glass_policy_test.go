package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestBreakGlass_BypassesConnectorPolicy(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"sf": {Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"}},
	})
	s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{TenantID: "acme", AgentID: "sales", Connector: "other"}},
	}))

	mk := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/internal/connectors/sf/session", nil)
		req.SetPathValue("name", "sf")
		return req.WithContext(auth.WithRunBinding(context.Background(), &auth.RunBinding{
			RunID: "r1", Generation: 1, TenantID: "acme", AgentID: "sales",
			User: &transport.UserContext{Identity: "alice"},
		}))
	}

	rec := httptest.NewRecorder()
	s.handleGetConnectorSession(rec, mk())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("before break-glass want 403, got %d %s", rec.Code, rec.Body.String())
	}

	if err := store.CreateBreakGlassWindow(ctx, &models.BreakGlassWindow{
		ID: "bg-1", TenantID: "acme", AgentID: "sales", Reason: "drill",
		StartsAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	rec2 := httptest.NewRecorder()
	s.handleGetConnectorSession(rec2, mk())
	if rec2.Code != http.StatusOK {
		t.Fatalf("under break-glass want 200, got %d %s", rec2.Code, rec2.Body.String())
	}

	audits, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{
		TenantID: "acme", Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	foundUse := false
	for _, ev := range audits {
		if ev.ReasonCode == policy.ReasonBreakGlass && ev.Decision == policy.EffectAllow &&
			ev.Action == policy.StageConnectorSession {
			foundUse = true
			if ev.RuleID != "bg-1" {
				t.Fatalf("audit rule_id=%q", ev.RuleID)
			}
		}
	}
	if !foundUse {
		t.Fatalf("expected break_glass use audit; body was OK, events=%d", len(audits))
	}
}

func TestBreakGlass_AuditNotesMandatoryHITLBypass(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	reg := connector.NewRegistry(map[string]connector.ConnectorConfig{
		"gh": {
			Auth: connector.AuthConfig{Type: "bearer", BearerToken: "tok"},
			MCP:  &connector.MCPConfig{URL: "http://127.0.0.1:1"},
		},
	})
	s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	s.SetConnectorRegistry(reg)
	s.SetPolicyEngine(policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
		}},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			ID: "force-delete", TenantID: "acme", Connector: "gh", Tools: []string{"delete_repo"},
		}},
	}))
	if err := store.CreateBreakGlassWindow(ctx, &models.BreakGlassWindow{
		ID: "bg-m", TenantID: "acme", Reason: "sev1",
		StartsAt: time.Now().UTC().Add(-time.Minute), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	reqBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_repo"}}`
	req := httptest.NewRequest(http.MethodPost, "/internal/connectors/gh/mcp", strings.NewReader(reqBody))
	req.SetPathValue("name", "gh")
	req = req.WithContext(auth.WithRunBinding(context.Background(), &auth.RunBinding{
		RunID: "r-bg-m", Generation: 1, TenantID: "acme", AgentID: "sales",
		User: &transport.UserContext{Identity: "alice"},
	}))
	rec := httptest.NewRecorder()
	s.handleProxyMCPRequest(rec, req)
	// Downstream may 502; we only need the break-glass audit attrs.
	_ = rec

	audits, err := store.SearchAuditEvents(tenant.SystemContext(ctx), &models.AuditSearchRequest{
		TenantID: "acme", Action: policy.StageToolCall, Limit: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range audits {
		if ev.ReasonCode != policy.ReasonBreakGlass {
			continue
		}
		if ev.Attrs["mandatory_hitl_bypassed"] != true {
			t.Fatalf("want mandatory_hitl_bypassed attr, attrs=%v", ev.Attrs)
		}
		if ev.Attrs["mandatory_hitl_rule_id"] != "force-delete" {
			t.Fatalf("rule_id attr=%v", ev.Attrs["mandatory_hitl_rule_id"])
		}
		return
	}
	t.Fatal("no break_glass tool.call audit found")
}
