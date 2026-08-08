package policy_test

import (
	"context"
	"testing"

	"github.com/getrunkite/runkite/internal/policy"
)

func TestReplaceOverlays_DBWinsWithoutRestart(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "cfg", TenantID: "acme", AgentID: "sales", Connector: "sf",
			Tools: &policy.ToolFilter{Allow: []string{"query"}},
		}},
	})
	in := policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "sf", Tool: "updateRecord",
	}
	if dec := eng.Decide(context.Background(), in); dec.Effect != policy.EffectDeny {
		t.Fatalf("baseline deny updateRecord: got %q", dec.Effect)
	}

	eng.ReplaceOverlays([]policy.Grant{{
		ID: "db", TenantID: "acme", AgentID: "sales", Connector: "sf",
		Tools: &policy.ToolFilter{Allow: []string{"query", "updateRecord"}},
	}})
	dec := eng.Decide(context.Background(), in)
	if dec.Effect != policy.EffectAllow || dec.RuleID != "db" {
		t.Fatalf("overlay should allow: effect=%q rule=%q", dec.Effect, dec.RuleID)
	}

	eng.ReplaceOverlays(nil)
	if dec := eng.Decide(context.Background(), in); dec.Effect != policy.EffectDeny {
		t.Fatalf("after overlay delete, want deny again, got %q", dec.Effect)
	}
}

func TestReplaceOverlays_AddsGrantNotInConfig(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "cfg", TenantID: "acme", AgentID: "sales", Connector: "sf",
		}},
	})
	in := policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "beta", AgentID: "ops", Connector: "gh",
	}
	if dec := eng.Decide(context.Background(), in); dec.Effect != policy.EffectDeny {
		t.Fatalf("want deny for unknown grant, got %q", dec.Effect)
	}
	eng.ReplaceOverlays([]policy.Grant{{
		ID: "db-gh", TenantID: "beta", AgentID: "ops", Connector: "gh",
	}})
	if dec := eng.Decide(context.Background(), in); dec.Effect != policy.EffectAllow {
		t.Fatalf("want allow after overlay add, got %q", dec.Effect)
	}
}
