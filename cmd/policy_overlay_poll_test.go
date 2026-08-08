package main

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
	"github.com/getrunkite/runkite/internal/transport/inprocess"
)

func TestPolicyGrantsFingerprint_EmptyAndOrderIndependent(t *testing.T) {
	if got := policyGrantsFingerprint(nil); got != "0" {
		t.Fatalf("nil: want 0, got %q", got)
	}
	a := &models.PolicyGrant{ID: "a", UpdatedAt: time.Unix(1, 0).UTC()}
	b := &models.PolicyGrant{ID: "b", UpdatedAt: time.Unix(2, 0).UTC()}
	if policyGrantsFingerprint([]*models.PolicyGrant{a, b}) != policyGrantsFingerprint([]*models.PolicyGrant{b, a}) {
		t.Fatal("fingerprint must be order-independent")
	}
	b2 := &models.PolicyGrant{ID: "b", UpdatedAt: time.Unix(3, 0).UTC()}
	if policyGrantsFingerprint([]*models.PolicyGrant{a, b}) == policyGrantsFingerprint([]*models.PolicyGrant{a, b2}) {
		t.Fatal("updated_at change must alter fingerprint")
	}
}

func TestSyncPolicyOverlaysIfChanged_SiblingSeesAdminWrite(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Simulate replica B: engine started with no overlays.
	eng := policy.New(policy.Config{ForceEnable: true, DefaultEffect: "deny"})
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)

	fp, err := syncPolicyOverlaysIfChanged(ctx, store, apiServer, "")
	if err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	in := policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "acme", AgentID: "sales", Connector: "sf",
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectDeny {
		t.Fatalf("precondition: want deny, got %q", dec.Effect)
	}

	// Simulate replica A writing a grant (no local Reload on B).
	if err := store.UpsertPolicyGrant(ctx, &models.PolicyGrant{
		ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "sf",
	}); err != nil {
		t.Fatalf("UpsertPolicyGrant: %v", err)
	}

	next, err := syncPolicyOverlaysIfChanged(ctx, store, apiServer, fp)
	if err != nil {
		t.Fatalf("sync after write: %v", err)
	}
	if next == fp {
		t.Fatal("expected fingerprint change after upsert")
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectAllow {
		t.Fatalf("after poll reload: want allow, got %q reason=%s", dec.Effect, dec.ReasonCode)
	}

	// No-op tick: fingerprint stable, decision unchanged.
	stable, err := syncPolicyOverlaysIfChanged(ctx, store, apiServer, next)
	if err != nil {
		t.Fatalf("stable sync: %v", err)
	}
	if stable != next {
		t.Fatalf("stable tick changed fingerprint: %q -> %q", next, stable)
	}

	if err := store.DeletePolicyGrant(ctx, "g1"); err != nil {
		t.Fatalf("DeletePolicyGrant: %v", err)
	}
	afterDel, err := syncPolicyOverlaysIfChanged(ctx, store, apiServer, stable)
	if err != nil {
		t.Fatalf("sync after delete: %v", err)
	}
	if afterDel == stable {
		t.Fatal("expected fingerprint change after delete")
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectDeny {
		t.Fatalf("after delete poll: want deny, got %q", dec.Effect)
	}
}

func TestSyncPolicyOverlaysIfChanged_MandatoryHITL(t *testing.T) {
	ctx := context.Background()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}

	eng := policy.New(policy.Config{
		ForceEnable:   true,
		DefaultEffect: "deny",
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
		}},
	})
	apiServer := api.NewServer(store, inprocess.NewQueue(), inprocess.NewBroker(), inprocess.NewCancelBus())
	apiServer.SetPolicyEngine(eng)

	fp, err := syncPolicyOverlaysIfChanged(ctx, store, apiServer, "")
	if err != nil {
		t.Fatal(err)
	}
	in := policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "gh", Tool: "delete_repo",
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectAllow {
		t.Fatalf("precondition allow: %#v", dec)
	}

	if err := store.UpsertMandatoryHITLRule(ctx, &models.MandatoryHITLRule{
		ID: "m1", TenantID: "acme", Connector: "gh", Tools: []string{"delete_repo"},
	}); err != nil {
		t.Fatal(err)
	}
	next, err := syncPolicyOverlaysIfChanged(ctx, store, apiServer, fp)
	if err != nil {
		t.Fatal(err)
	}
	if next == fp {
		t.Fatal("expected fingerprint change after mandatory_hitl upsert")
	}
	if dec := eng.Decide(ctx, in); dec.Effect != policy.EffectPending || dec.ReasonCode != policy.ReasonMandatoryHITL {
		t.Fatalf("after poll: want mandatory pending, got %#v", dec)
	}
}
