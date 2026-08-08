package policy_test

import (
	"context"
	"testing"

	"github.com/getrunkite/runkite/internal/policy"
)

func TestDecide_RunCreateSkipsConnectorGrants(t *testing.T) {
	eng := policy.New(policy.Config{
		DefaultEffect: "deny",
		Grants: []policy.Grant{{
			ID: "sf", TenantID: "acme", AgentID: "sales", Connector: "sf",
		}},
	})
	// Connector stage still requires a matching grant.
	if dec := eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "acme", AgentID: "sales", Connector: "sf",
	}); dec.Effect != policy.EffectAllow {
		t.Fatalf("connector: want allow, got %q", dec.Effect)
	}
	// run.create must not deny just because there is no connector on the input.
	dec := eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageRunCreate, TenantID: "acme", AgentID: "sales",
	})
	if dec.Effect != policy.EffectAllow {
		t.Fatalf("run.create: want allow (no webhook), got %q reason=%s", dec.Effect, dec.ReasonCode)
	}
}
