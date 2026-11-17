package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

// Frozen fingerprint from the pre-upgrade evaluator for the unchanged rows
// below. An evaluator semantics upgrade must invalidate this existing authority.
const priorIdentityEvaluatorRevision = "policy-v1-8b84adc8ed8083ab9989664af58b26770630b5eda31178971ad7386abc06b38f"

func TestPolicySemanticsUpgradeRequiresFreshManagedApproval(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Init(ctx); err != nil {
		t.Fatal(err)
	}
	server := NewServer(db, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	registry, target := fixtureTargetRegistry(t, "files")
	server.SetConnectorRegistry(registry)
	engine := policy.New(policy.Config{ForceEnable: true, DefaultEffect: policy.EffectAllow, MandatoryHITL: []policy.MandatoryHITLRule{{TenantID: "default", AgentID: "agent", Connector: "files", Tools: []string{"write"}}}})
	server.SetPolicyEngine(engine)
	if engine.Revision() == priorIdentityEvaluatorRevision {
		t.Fatal("evaluator semantics retained old policy authority")
	}
	binding := &auth.RunBinding{RunID: "semantics-run", ThreadID: "semantics-thread", Generation: 1, TenantID: "default", AgentID: "agent", RunIncarnationID: originalRunIdentity(t, db, "semantics-run")}
	seedManagedPendingFixture(t, db, binding)
	_, digest, args := policy.BindArgs(json.RawMessage(`{"amount":3}`))
	old := &models.PendingAction{ID: pendingActionIDPrefix + "old-semantics", RunID: binding.RunID, TenantID: binding.TenantID, AgentID: binding.AgentID, Generation: binding.Generation, Connector: "files", Tool: "write", ArgsDigest: digest, Args: args, TargetRevision: target, PolicyRevision: priorIdentityEvaluatorRevision}
	if err := db.CreatePendingAction(ctx, old); err != nil {
		t.Fatal(err)
	}
	handler := auth.Middleware(auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{"admin": {Name: "admin", Permissions: []string{"admin"}}}), nil, nil, server.Handler())
	response := reviewRequest(handler, http.MethodPost, "/admin-api/pending-actions/"+old.ID+"/approve", "admin", "", nil, "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "policy changed") {
		t.Fatalf("old evaluator approval was accepted: %d %s", response.Code, response.Body.String())
	}
	// Model an approval issued before deployment, without modifying its history.
	if err := db.SetPendingActionApproved(ctx, old.ID, "prior-reviewer"); err != nil {
		t.Fatal(err)
	}
	bound := auth.WithRunBinding(tenant.WithContext(ctx, binding.TenantID), binding)
	input := server.policyInput(bound, policy.StageToolCall, "files", "write")
	input.TargetRevision = target
	input.Args = args
	input.ArgsMeta = args
	input.ArgsDigest = digest
	decision := engine.Decide(bound, input)
	if decision.Effect != policy.EffectPending {
		t.Fatal("review requirement disappeared", decision)
	}
	if server.tryConsumePendingCapability(bound, "files", "write", digest, target, engine.Revision(), "") {
		t.Fatal("consumed an approval issued under old evaluator semantics")
	}
	fresh, err := server.persistPendingAction(bound, input, "files", "write", decision, nil)
	if err != nil || fresh == old.ID {
		t.Fatal("new semantics did not require a fresh review", fresh, err)
	}
	stored, err := db.GetPendingAction(ctx, old.ID)
	if err != nil || stored.Status != models.PendingStatusApproved || stored.PolicyRevision != priorIdentityEvaluatorRevision {
		t.Fatal("upgrade rewrote historical approval", stored, err)
	}
}
