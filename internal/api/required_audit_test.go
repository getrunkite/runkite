package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/hooks"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

type switchablePolicyAuditor struct {
	failed atomic.Bool
	store  policy.Auditor
}

func (a *switchablePolicyAuditor) WritePolicyDecision(ctx context.Context, in policy.PolicyInput, dec policy.PolicyDecision) error {
	if a.failed.Load() {
		return errors.New("fixture audit unavailable")
	}
	return a.store.WritePolicyDecision(ctx, in, dec)
}

func TestRequiredAuditBlocksMCPBeforeEffectsAndApprovalConsumption(t *testing.T) {
	for _, mode := range []string{"allow", "approved", "break-glass"} {
		t.Run(mode, func(t *testing.T) {
			store, err := sqlite.New("")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if err := store.Init(context.Background()); err != nil {
				t.Fatal(err)
			}
			var effects atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				effects.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[]}}`))
			}))
			t.Cleanup(target.Close)
			s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
			s.SetConnectorRegistry(connector.NewRegistry(map[string]connector.ConnectorConfig{
				"fixture": {Auth: connector.AuthConfig{Type: "bearer", BearerToken: "synthetic-target-credential"}, MCP: &connector.MCPConfig{URL: target.URL}},
			}))
			auditor := &switchablePolicyAuditor{store: policy.NewStoreAuditor(store)}
			cfg := policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: auditor}
			if mode == "approved" {
				cfg.MandatoryHITL = []policy.MandatoryHITLRule{{ID: "review", TenantID: "acme", Connector: "fixture", Tools: []string{"write"}}}
			}
			if mode == "break-glass" {
				cfg.DefaultEffect = policy.EffectDeny
				if err := store.CreateBreakGlassWindow(context.Background(), &models.BreakGlassWindow{
					ID: "fixture-window", TenantID: "acme", Reason: "fixture", StartsAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			}
			s.SetPolicyEngine(policy.New(cfg))
			binding := &auth.RunBinding{RunID: "fixture-run", Generation: 2, TenantID: "acme", AgentID: "agent", User: &transport.UserContext{Identity: "alice"}}
			ctx := auth.WithRunBinding(tenant.WithContext(context.Background(), "acme"), binding)
			pendingID := ""
			if mode == "approved" {
				in := s.policyInput(ctx, policy.StageToolCall, "fixture", "write")
				in.Args, in.ArgsDigest, in.ArgsMeta, err = policy.BindArgsStrict([]byte(`{"amount":1}`))
				if err != nil {
					t.Fatal(err)
				}
				pendingID, err = s.persistPendingAction(ctx, in, "fixture", "write", policy.PolicyDecision{Effect: policy.EffectPending}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetPendingActionApproved(ctx, pendingID, "reviewer"); err != nil {
					t.Fatal(err)
				}
			}
			call := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write","arguments":{"amount":1}}}`)).WithContext(ctx)
				r.SetPathValue("name", "fixture")
				attachConnectorSession(t, s, binding, "fixture", r)
				w := httptest.NewRecorder()
				s.handleProxyMCPRequest(w, r)
				return w
			}
			auditor.failed.Store(true)
			blocked := call()
			if effects.Load() != 0 || !strings.Contains(blocked.Body.String(), `"effect":"deny"`) {
				t.Fatalf("audit outage allowed effect: hits=%d body=%s", effects.Load(), blocked.Body.String())
			}
			if pendingID != "" {
				row, err := store.GetPendingAction(ctx, pendingID)
				if err != nil || row.Status != models.PendingStatusApproved {
					t.Fatalf("audit outage consumed approval: row=%+v err=%v", row, err)
				}
			}
			auditor.failed.Store(false)
			allowed := call()
			if allowed.Code != http.StatusOK || effects.Load() != 1 || strings.Contains(allowed.Body.String(), `"error"`) {
				t.Fatalf("audit recovery failed: hits=%d body=%s", effects.Load(), allowed.Body.String())
			}
			if pendingID != "" {
				row, err := store.GetPendingAction(ctx, pendingID)
				if err != nil || row.Status != models.PendingStatusConsumed {
					t.Fatalf("recovered call did not consume approval: row=%+v err=%v", row, err)
				}
				_ = call()
				if effects.Load() != 1 {
					t.Fatal("consumed approval authorized a second effect")
				}
			}
		})
	}
}

func TestRequiredAuditBlocksAdmissionEvenWithBreakGlass(t *testing.T) {
	store, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateBreakGlassWindow(context.Background(), &models.BreakGlassWindow{
		ID: "admission-window", TenantID: "acme", Reason: "fixture", StartsAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	auditor := &switchablePolicyAuditor{store: policy.NewStoreAuditor(store)}
	auditor.failed.Store(true)
	s.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: auditor}))
	gate := &admissionGate{server: s}
	ctx := tenant.WithContext(context.Background(), "acme")
	event := hooks.Event{TenantID: "acme", AgentID: "agent", RunID: "run"}
	if dec := gate.Decide(ctx, event); dec.Allow {
		t.Fatal("break-glass bypassed required admission audit")
	}
	auditor.failed.Store(false)
	if dec := gate.Decide(ctx, event); !dec.Allow {
		t.Fatalf("admission did not recover: %+v", dec)
	}
}
