package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/connector"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func fixtureTargetRegistry(t *testing.T, name string) (*connector.Registry, string) {
	t.Helper()
	registry := connector.NewRegistry(map[string]connector.ConnectorConfig{name: {
		Auth: connector.AuthConfig{Type: "bearer", BearerToken: "synthetic-fixture-only"},
		MCP:  &connector.MCPConfig{URL: "http://127.0.0.1:1/fixture-mcp"},
	}})
	revision, err := registry.TargetRevision(name)
	if err != nil || !models.ValidConnectorTargetRevision(revision) {
		t.Fatalf("fixture target revision: %q %v", revision, err)
	}
	return registry, revision
}

func currentFixtureTargetRevision(t *testing.T, server *Server, name string) string {
	t.Helper()
	if server.connectors == nil {
		t.Fatal("fixture requires a connector registry")
	}
	revision, err := server.connectors.TargetRevision(name)
	if err != nil || !models.ValidConnectorTargetRevision(revision) {
		t.Fatalf("fixture target revision: %q %v", revision, err)
	}
	return revision
}

func callTargetFixture(t *testing.T, env *actionAPIEnv, server *Server) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/internal/connectors/fixture/mcp", strings.NewReader(recordedToolCall)).WithContext(env.ctx)
	r.SetPathValue("name", "fixture")
	attachConnectorSession(t, server, env.binding, "fixture", r)
	w := httptest.NewRecorder()
	server.handleProxyMCPRequest(w, r)
	return w
}

func pendingTargetFixture(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Data struct {
				Effect string `json:"effect"`
				ID     string `json:"action_id"`
			} `json:"data"`
		} `json:"error"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Data.Effect != policy.EffectPending || body.Error.Data.ID == "" {
		t.Fatalf("expected pending: %d %s", response.Code, response.Body.String())
	}
	return body.Error.Data.ID
}

func approveTargetFixture(t *testing.T, env *actionAPIEnv, server *Server, id, surface string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/"+surface+"-api/pending-actions/"+id+"/approve", nil).WithContext(env.ctx)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	if surface == "review" {
		r = r.WithContext(auth.WithContext(r.Context(), &auth.AuthResult{Identity: "reviewer", TenantID: env.binding.TenantID, Permissions: []string{auth.GovernanceApprovePermission}}))
		server.handleReviewApprovePendingAction(w, r)
	} else {
		server.handleAdminApprovePendingAction(w, r)
	}
	return w
}

func changedTargetFixture(t *testing.T, original *connector.Registry) (*connector.Registry, *atomic.Int32) {
	t.Helper()
	effects := &atomic.Int32{}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		if rpc.Method == "tools/call" {
			effects.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": map[string]any{"content": []any{}}})
	}))
	t.Cleanup(target.Close)
	entry, err := original.Get("fixture")
	if err != nil {
		t.Fatal(err)
	}
	cfg := entry.Config
	mcp := *cfg.MCP
	mcp.URL = target.URL
	cfg.MCP = &mcp
	return connector.NewRegistry(map[string]connector.ConnectorConfig{"fixture": cfg}), effects
}

func TestManagedTargetRevisionChangeRequiresFreshReview(t *testing.T) {
	for _, capture := range []bool{false, true} {
		for _, surface := range []string{"admin", "review"} {
			t.Run(fmt.Sprintf("capture_%v/%s", capture, surface), func(t *testing.T) {
				env := newActionAPIEnv(t, "json", "approved")
				if !capture {
					env.s.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: policy.NewStoreAuditor(env.store), MandatoryHITL: []policy.MandatoryHITLRule{{ID: "review", TenantID: "acme", Connector: "fixture", Tools: []string{"write"}}}}))
				}
				original := env.s.connectors
				id := pendingTargetFixture(t, callTargetFixture(t, env, env.s))
				row, err := env.store.GetPendingAction(env.ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if row.TargetRevision != currentFixtureTargetRevision(t, env.s, "fixture") {
					t.Fatal("pending row did not capture trusted target")
				}
				changed, effects := changedTargetFixture(t, original)
				env.s.SetConnectorRegistry(changed)
				refused := approveTargetFixture(t, env, env.s, id, surface)
				if refused.Code != http.StatusConflict || strings.Contains(refused.Body.String(), "http:") || strings.Contains(refused.Body.String(), "synthetic-") {
					t.Fatalf("changed target approval: %d %s", refused.Code, refused.Body.String())
				}
				stillPending, err := env.store.GetPendingAction(env.ctx, id)
				if err != nil || stillPending.Status != models.PendingStatusPending {
					t.Fatalf("target mismatch changed authority: %+v %v", stillPending, err)
				}
				env.s.SetConnectorRegistry(original)
				if approved := approveTargetFixture(t, env, env.s, id, surface); approved.Code != http.StatusOK {
					t.Fatalf("approve original target: %d %s", approved.Code, approved.Body.String())
				}
				env.s.SetConnectorRegistry(changed)
				response := callTargetFixture(t, env, env.s)
				freshID := pendingTargetFixture(t, response)
				old, err := env.store.GetPendingAction(env.ctx, id)
				if err != nil || old.Status != models.PendingStatusApproved {
					t.Fatalf("changed target consumed original capability: %+v %v", old, err)
				}
				fresh, err := env.store.GetPendingAction(env.ctx, freshID)
				if err != nil || freshID == id || fresh.TargetRevision == old.TargetRevision || fresh.TargetRevision != currentFixtureTargetRevision(t, env.s, "fixture") {
					t.Fatalf("changed target did not create new review: %+v %v", fresh, err)
				}
				if env.effects.Load() != 0 || effects.Load() != 0 {
					t.Fatal("target change executed a protected tool")
				}
				if capture {
					action, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
					if err != nil || action.TargetRevision == nil || *action.TargetRevision != fresh.TargetRevision {
						t.Fatalf("action target does not match actual pinned target: %+v %v", action, err)
					}
				}
			})
		}
	}
}

func TestManagedTargetRevisionDiffersAcrossReplicas(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprint(capture), func(t *testing.T) {
			env := newActionAPIEnv(t, "json", "approved")
			if !capture {
				env.s.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: policy.NewStoreAuditor(env.store), MandatoryHITL: []policy.MandatoryHITLRule{{ID: "review", TenantID: "acme", Connector: "fixture", Tools: []string{"write"}}}}))
			}
			id := pendingTargetFixture(t, callTargetFixture(t, env, env.s))
			if response := approveTargetFixture(t, env, env.s, id, "admin"); response.Code != http.StatusOK {
				t.Fatalf("approval: %d %s", response.Code, response.Body.String())
			}
			registry, effects := changedTargetFixture(t, env.s.connectors)
			peer := NewServer(env.store, env.queue, inprocess.NewBroker(), inprocess.NewCancelBus())
			peer.SetConnectorRegistry(registry)
			peer.SetPolicyEngine(env.s.policy)
			ensureConnectorSessions(peer)
			fresh := pendingTargetFixture(t, callTargetFixture(t, env, peer))
			if fresh == id || effects.Load() != 0 || env.effects.Load() != 0 {
				t.Fatal("different replica reused reviewed target authority")
			}
			old, err := env.store.GetPendingAction(env.ctx, id)
			if err != nil || old.Status != models.PendingStatusApproved {
				t.Fatalf("peer consumed original target capability: %+v %v", old, err)
			}
		})
	}
}

func TestManagedLegacyEmptyTargetRequiresFreshApproval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-target.db")
	env := newActionAPIEnvAtPath(t, path, "json", "approved")
	id := pendingTargetFixture(t, callTargetFixture(t, env, env.s))
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Simulate a retained pre-revision row. Its old key did not include a
	// target revision and must not collide with a newly issued review key.
	if _, err := db.ExecContext(context.Background(), `UPDATE pending_actions SET target_revision='',pending_key=NULL WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	response := approveTargetFixture(t, env, env.s, id, "admin")
	if response.Code != http.StatusConflict {
		t.Fatalf("legacy target could be approved: %d %s", response.Code, response.Body.String())
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE pending_actions SET status='approved',pending_key=NULL WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	fresh := pendingTargetFixture(t, callTargetFixture(t, env, env.s))
	old, err := env.store.GetPendingAction(env.ctx, id)
	if err != nil || fresh == id || old.Status != models.PendingStatusApproved || env.effects.Load() != 0 {
		t.Fatalf("legacy target authority was reused: %+v %v", old, err)
	}
}

type targetChangeDuringDecisionAuditor struct {
	server *Server
	next   *connector.Registry
	store  policy.Auditor
}

func (a *targetChangeDuringDecisionAuditor) WritePolicyDecision(ctx context.Context, in policy.PolicyInput, dec policy.PolicyDecision) error {
	if err := a.store.WritePolicyDecision(ctx, in, dec); err != nil {
		return err
	}
	if in.Stage == policy.StageToolCall {
		a.server.SetConnectorRegistry(a.next)
	}
	return nil
}

func TestReviewApprovalRejectsTargetChangeDuringDecision(t *testing.T) {
	env := newReviewFixture(t)
	entry, err := env.server.connectors.Get("files")
	if err != nil {
		t.Fatal(err)
	}
	cfg := entry.Config
	mcp := *cfg.MCP
	mcp.Revision = "next-contract"
	cfg.MCP = &mcp
	next := connector.NewRegistry(map[string]connector.ConnectorConfig{"files": cfg})
	env.server.SetPolicyEngine(policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, Auditor: &targetChangeDuringDecisionAuditor{server: env.server, next: next, store: policy.NewStoreAuditor(env.store)}}))
	id := env.pending["a"][0]
	response := reviewRequest(env.handler, http.MethodPost, "/review-api/pending-actions/"+id+"/approve", "approve-a", "", nil, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("late target change minted approval: %d %s", response.Code, response.Body.String())
	}
	row, err := env.store.GetPendingAction(tenant.ScopedContext(t.Context(), "a"), id)
	if err != nil || row.Status != models.PendingStatusPending {
		t.Fatalf("late target change mutated authority: %+v %v", row, err)
	}
}

func TestManagedCallKeepsPinnedTargetThroughPolicyDelay(t *testing.T) {
	for _, capture := range []bool{false, true} {
		t.Run(fmt.Sprint(capture), func(t *testing.T) {
			env := newActionAPIEnv(t, "json", "approved")
			approvalID := env.approve(t)
			originalRevision := currentFixtureTargetRevision(t, env.s, "fixture")
			next, nextEffects := changedTargetFixture(t, env.s.connectors)
			env.s.SetPolicyEngine(policy.New(policy.Config{
				ForceEnable: true, DefaultEffect: policy.EffectAllow, AuditRequired: true, ActionEvidence: capture,
				Auditor:       &targetChangeDuringDecisionAuditor{server: env.s, next: next, store: policy.NewStoreAuditor(env.store)},
				MandatoryHITL: []policy.MandatoryHITLRule{{ID: "review", TenantID: "acme", Connector: "fixture", Tools: []string{"write"}}},
			}))
			response := callTargetFixture(t, env, env.s)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "synthetic-sensitive-result") || env.effects.Load() != 1 || nextEffects.Load() != 0 {
				t.Fatalf("bound target changed after authority evaluation: %d old=%d next=%d %s", response.Code, env.effects.Load(), nextEffects.Load(), response.Body.String())
			}
			approval, err := env.store.GetPendingAction(env.ctx, approvalID)
			if err != nil || approval.Status != models.PendingStatusConsumed || approval.TargetRevision != originalRevision {
				t.Fatalf("pinned target did not consume exactly its own authority: %+v %v", approval, err)
			}
			if capture {
				action, err := env.store.GetConnectorAction(env.ctx, response.Header().Get(headerConnectorActionID))
				if err != nil || action.TargetRevision == nil || *action.TargetRevision != originalRevision || action.State != models.ConnectorActionResponseObserved {
					t.Fatalf("recorded target differs from actual bound execution: %+v %v", action, err)
				}
			}
		})
	}
}
