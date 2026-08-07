package policy_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/getrunkite/runkite/internal/policy"
)

func TestStaticGrantAllowDeny(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID:        "g1",
			TenantID:  "acme",
			AgentID:   "sales",
			Connector: "salesforce",
			Tools:     &policy.ToolFilter{Allow: []string{"query"}, Deny: []string{"updateRecord"}},
		}},
	})
	if !eng.Enabled() {
		t.Fatal("expected engine enabled")
	}

	ctx := context.Background()
	allow := eng.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "salesforce", Tool: "query",
	})
	if allow.Effect != policy.EffectAllow {
		t.Fatalf("query: %+v", allow)
	}

	denyTool := eng.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "salesforce", Tool: "updateRecord",
	})
	if denyTool.Effect != policy.EffectDeny || denyTool.ReasonCode != policy.ReasonPolicyToolDenied {
		t.Fatalf("updateRecord: %+v", denyTool)
	}

	denyTenant := eng.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "other", AgentID: "sales",
		Connector: "salesforce",
	})
	if denyTenant.Effect != policy.EffectDeny || denyTenant.ReasonCode != policy.ReasonPolicyNoGrant {
		t.Fatalf("other tenant: %+v", denyTenant)
	}

	denyAgent := eng.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "acme", AgentID: "hr",
		Connector: "salesforce",
	})
	if denyAgent.Effect != policy.EffectDeny || denyAgent.ReasonCode != policy.ReasonPolicyNoGrant {
		t.Fatalf("other agent: %+v", denyAgent)
	}

	sess := eng.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "acme", AgentID: "sales",
		Connector: "salesforce",
	})
	if sess.Effect != policy.EffectAllow || sess.RuleID != "g1" {
		t.Fatalf("session: %+v", sess)
	}
}

func TestNilEngineWhenEmpty(t *testing.T) {
	if policy.New(policy.Config{}) != nil {
		t.Fatal("empty config should yield nil engine")
	}
}

func TestMissingBindingDenied(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{TenantID: "acme", AgentID: "sales", Connector: "sf"}},
	})
	dec := eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageConnectorSession, Connector: "sf",
	})
	if dec.Effect != policy.EffectDeny || dec.ReasonCode != policy.ReasonPolicyMissingBinding {
		t.Fatalf("%+v", dec)
	}
}

func TestConcurrentTenantsNoCrossTalk(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{
			{TenantID: "a", AgentID: "bot", Connector: "sf", Tools: &policy.ToolFilter{Allow: []string{"query"}}},
			{TenantID: "b", AgentID: "bot", Connector: "sf"}, // no tools filter → all tools allowed for b
		},
	})
	ctx := context.Background()
	errCh := make(chan error, 64)
	for i := 0; i < 32; i++ {
		go func() {
			da := eng.Decide(ctx, policy.PolicyInput{
				Stage: policy.StageToolCall, TenantID: "a", AgentID: "bot",
				Connector: "sf", Tool: "updateRecord",
			})
			if da.Effect != policy.EffectDeny {
				errCh <- fmt.Errorf("tenant a should deny updateRecord: %+v", da)
				return
			}
			db := eng.Decide(ctx, policy.PolicyInput{
				Stage: policy.StageToolCall, TenantID: "b", AgentID: "bot",
				Connector: "sf", Tool: "updateRecord",
			})
			if db.Effect != policy.EffectAllow {
				errCh <- fmt.Errorf("tenant b should allow updateRecord: %+v", db)
				return
			}
			errCh <- nil
		}()
	}
	for i := 0; i < 32; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}
