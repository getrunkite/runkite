package policy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPolicyRevisionTracksAuthorityAndExcludesSigningSecret(t *testing.T) {
	cfg := Config{ForceEnable: true, DefaultEffect: EffectAllow, Webhook: &WebhookConfig{URL: "https://policy.example.test/decide", Revision: "rules-1", Secret: "synthetic-old"}}
	first := New(cfg)
	revision := first.Revision()
	cfg.Webhook.Secret = "synthetic-new"
	if New(cfg).Revision() != revision {
		t.Fatal("signing rotation changed policy identity")
	}
	cfg.Webhook.Revision = "rules-2"
	if New(cfg).Revision() == revision {
		t.Fatal("remote contract change did not invalidate policy")
	}
	cfg.Webhook.Revision = "rules-1"
	cfg.ApprovalRequirements = ApprovalRequirements{RequiredApprovals: 2, ExcludeRequester: true}
	if New(cfg).Revision() == revision {
		t.Fatal("review authority change did not invalidate policy")
	}
	first.ReplaceOverlays([]Grant{{TenantID: "a", AgentID: "agent", Connector: "files"}})
	if first.Revision() == revision {
		t.Fatal("live overlay change retained old authority")
	}
}

func TestPolicyChangeDuringDecisionCannotAuthorize(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"effect":"allow"}`))
	}))
	defer server.Close()
	engine := New(Config{ForceEnable: true, DefaultEffect: EffectAllow, Webhook: &WebhookConfig{URL: server.URL}})
	result := make(chan PolicyDecision, 1)
	go func() {
		result <- engine.Decide(context.Background(), PolicyInput{Stage: StageToolCall, TenantID: "a", AgentID: "agent", Connector: "files", Tool: "write"})
	}()
	<-entered
	engine.ReplaceMandatoryHITL([]MandatoryHITLRule{{TenantID: "a", Connector: "files"}})
	close(release)
	decision := <-result
	if decision.Effect != EffectDeny || decision.ReasonCode != "policy_revision_changed" {
		t.Fatalf("changed policy granted authority: %+v", decision)
	}
}

func TestInvalidApprovalPlanFailsClosed(t *testing.T) {
	engine := New(Config{ForceEnable: true, DefaultEffect: EffectAllow, ApprovalRequirements: ApprovalRequirements{RequiredApprovals: 6}})
	if got := engine.Decide(context.Background(), PolicyInput{TenantID: "a", AgentID: "agent", Stage: StageToolCall}); got.Effect != EffectDeny {
		t.Fatal("invalid review policy allowed access")
	}
}
