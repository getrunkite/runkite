package policy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/getrunkite/runkite/internal/policy"
)

func TestMandatoryHITL_ForcesAllowToPending(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
			Tools: &policy.ToolFilter{Allow: []string{"delete_repo", "list_repos"}},
		}},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			ID: "m1", TenantID: "acme", Connector: "gh", Tools: []string{"delete_repo"},
		}},
	})
	ctx := context.Background()

	del := eng.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "gh", Tool: "delete_repo",
	})
	if del.Effect != policy.EffectPending || del.ReasonCode != policy.ReasonMandatoryHITL {
		t.Fatalf("delete_repo: %#v", del)
	}
	if del.RuleID != "m1" {
		t.Fatalf("rule_id=%q", del.RuleID)
	}

	list := eng.Decide(ctx, policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "gh", Tool: "list_repos",
	})
	if list.Effect != policy.EffectAllow {
		t.Fatalf("list_repos should still allow: %#v", list)
	}
}

func TestMandatoryHITL_DoesNotElevateDeny(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
			Tools: &policy.ToolFilter{Allow: []string{"list_repos"}},
		}},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			TenantID: "acme", Connector: "gh", Tools: []string{"delete_repo"},
		}},
	})
	dec := eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "gh", Tool: "delete_repo",
	})
	if dec.Effect != policy.EffectDeny || dec.ReasonCode != policy.ReasonPolicyToolDenied {
		t.Fatalf("want tool deny, got %#v", dec)
	}
}

func TestMandatoryHITL_OverridesWebhookAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"effect":"allow","reason":"pdp ok"}`))
	}))
	t.Cleanup(srv.Close)

	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
		}},
		Webhook: &policy.WebhookConfig{URL: srv.URL},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			TenantID: "acme", AgentID: "sales", Connector: "gh", Tools: []string{"transfer_funds"},
		}},
	})
	dec := eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
		Connector: "gh", Tool: "transfer_funds",
	})
	if dec.Effect != policy.EffectPending || dec.ReasonCode != policy.ReasonMandatoryHITL {
		t.Fatalf("want mandatory pending over webhook allow, got %#v", dec)
	}
}

func TestMandatoryHITL_TenantWideEmptyAgent(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{
			{ID: "g1", TenantID: "acme", AgentID: "a", Connector: "gh"},
			{ID: "g2", TenantID: "acme", AgentID: "b", Connector: "gh"},
		},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			TenantID: "acme", AgentID: "", Connector: "gh", Tools: []string{"delete_repo"},
		}},
	})
	for _, agent := range []string{"a", "b"} {
		dec := eng.Decide(context.Background(), policy.PolicyInput{
			Stage: policy.StageToolCall, TenantID: "acme", AgentID: agent,
			Connector: "gh", Tool: "delete_repo",
		})
		if dec.Effect != policy.EffectPending {
			t.Fatalf("agent %s: %#v", agent, dec)
		}
	}
}

// Agent-scoped and tenant-wide rules can coexist for the same tenant+connector
// once Admin CRUD is in play. Matching must prefer the agent-scoped rule
// regardless of slice order (not "first match in iteration order").
func TestMandatoryHITL_AgentScopedWinsOverTenantWide(t *testing.T) {
	tenantWide := policy.MandatoryHITLRule{
		ID: "tenant-wide", TenantID: "acme", AgentID: "", Connector: "gh",
		Tools: []string{"delete_repo"},
	}
	agentScoped := policy.MandatoryHITLRule{
		ID: "agent-sales", TenantID: "acme", AgentID: "sales", Connector: "gh",
		Tools: []string{"delete_repo"},
	}
	grants := []policy.Grant{
		{ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh"},
		{ID: "g2", TenantID: "acme", AgentID: "other", Connector: "gh"},
	}

	for _, name := range []string{"tenant-first", "agent-first"} {
		var rules []policy.MandatoryHITLRule
		if name == "tenant-first" {
			rules = []policy.MandatoryHITLRule{tenantWide, agentScoped}
		} else {
			rules = []policy.MandatoryHITLRule{agentScoped, tenantWide}
		}
		t.Run(name, func(t *testing.T) {
			eng := policy.New(policy.Config{Grants: grants, MandatoryHITL: rules})
			sales := eng.Decide(context.Background(), policy.PolicyInput{
				Stage: policy.StageToolCall, TenantID: "acme", AgentID: "sales",
				Connector: "gh", Tool: "delete_repo",
			})
			if sales.Effect != policy.EffectPending || sales.RuleID != "agent-sales" {
				t.Fatalf("sales: want agent-scoped rule, got %#v", sales)
			}
			other := eng.Decide(context.Background(), policy.PolicyInput{
				Stage: policy.StageToolCall, TenantID: "acme", AgentID: "other",
				Connector: "gh", Tool: "delete_repo",
			})
			if other.Effect != policy.EffectPending || other.RuleID != "tenant-wide" {
				t.Fatalf("other: want tenant-wide fallback, got %#v", other)
			}
		})
	}
}

func TestMandatoryHITL_SkipsSessionAndRunCreate(t *testing.T) {
	eng := policy.New(policy.Config{
		Grants: []policy.Grant{{
			ID: "g1", TenantID: "acme", AgentID: "sales", Connector: "gh",
		}},
		MandatoryHITL: []policy.MandatoryHITLRule{{
			TenantID: "acme", Connector: "gh", Tools: []string{"delete_repo"},
		}},
	})
	sess := eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageConnectorSession, TenantID: "acme", AgentID: "sales",
		Connector: "gh",
	})
	if sess.Effect != policy.EffectAllow {
		t.Fatalf("session: %#v", sess)
	}
	create := eng.Decide(context.Background(), policy.PolicyInput{
		Stage: policy.StageRunCreate, TenantID: "acme", AgentID: "sales",
	})
	if create.Effect != policy.EffectAllow {
		t.Fatalf("run.create: %#v", create)
	}
}
