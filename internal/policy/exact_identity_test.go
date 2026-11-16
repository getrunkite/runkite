package policy

import (
	"context"
	"testing"
	"time"
)

func TestPositiveGrantScopesAreExact(t *testing.T) {
	base := PolicyInput{Stage: StageConnectorSession, TenantID: "tenant", AgentID: "agent", Connector: "bank"}
	for _, axis := range []string{"tenant", "agent", "connector"} {
		t.Run(axis, func(t *testing.T) {
			changed := base
			switch axis {
			case "tenant":
				changed.TenantID = " tenant "
			case "agent":
				changed.AgentID = " agent "
			case "connector":
				changed.Connector = " bank "
			}
			for _, inverse := range []bool{false, true} {
				g, in := base, changed
				if inverse {
					g, in = changed, base
				}
				engine := New(Config{Grants: []Grant{{ID: "exact", TenantID: g.TenantID, AgentID: g.AgentID, Connector: g.Connector}}})
				if got := engine.Decide(context.Background(), in); got.Effect != EffectDeny || got.ReasonCode != ReasonPolicyNoGrant {
					t.Fatalf("distinct identity borrowed grant: %+v", got)
				}
				if got := engine.Decide(context.Background(), g); got.Effect != EffectAllow {
					t.Fatalf("exact identity lost its own grant: %+v", got)
				}
			}
		})
	}
}

func TestGrantOverlayKeysPreserveExactScopesAndSeparators(t *testing.T) {
	for _, pair := range [][2]Grant{
		{{ID: "base", TenantID: "tenant", AgentID: "agent", Connector: "bank"}, {ID: "overlay", TenantID: "tenant", AgentID: " agent ", Connector: "bank"}},
		{{ID: "base", TenantID: "tenant", AgentID: "agent", Connector: "bank"}, {ID: "overlay", TenantID: " tenant ", AgentID: "agent", Connector: "bank"}},
		{{ID: "base", TenantID: "tenant", AgentID: "agent", Connector: "bank"}, {ID: "overlay", TenantID: "tenant", AgentID: "agent", Connector: " bank "}},
		{{ID: "base", TenantID: "a\x00b", AgentID: "c", Connector: "bank"}, {ID: "overlay", TenantID: "a", AgentID: "b\x00c", Connector: "bank"}},
	} {
		engine := New(Config{Grants: []Grant{pair[0]}, Overlays: []Grant{pair[1]}})
		for _, g := range pair {
			if got := engine.Decide(context.Background(), PolicyInput{Stage: StageConnectorSession, TenantID: g.TenantID, AgentID: g.AgentID, Connector: g.Connector}); got.Effect != EffectAllow || got.RuleID != g.ID {
				t.Fatalf("overlay aliased exact scope: %+v", got)
			}
		}
	}
}

func TestPositiveToolNamesAreExactAndLegacyDenialsRemain(t *testing.T) {
	for _, filter := range []*ToolFilter{{Allow: []string{"write"}}, {Deny: []string{"write"}}} {
		engine := New(Config{DefaultEffect: EffectAllow, Grants: []Grant{{ID: "restriction", TenantID: " tenant ", AgentID: " agent ", Connector: " bank ", Tools: filter}}})
		in := PolicyInput{Stage: StageToolCall, TenantID: "tenant", AgentID: "agent", Connector: "bank", Tool: " write "}
		if len(filter.Allow) > 0 {
			in.Tool = " delete "
		}
		if got := engine.Decide(context.Background(), in); got.Effect != EffectDeny || got.ReasonCode != ReasonPolicyToolDenied {
			t.Fatalf("default allow removed legacy restrictive filter: %+v", got)
		}
	}
	engine := New(Config{Grants: []Grant{{TenantID: "tenant", AgentID: "agent", Connector: "bank", Tools: &ToolFilter{Allow: []string{"write"}}}}})
	for _, tool := range []string{" write", "write ", " write "} {
		if got := engine.Decide(context.Background(), PolicyInput{Stage: StageToolCall, TenantID: "tenant", AgentID: "agent", Connector: "bank", Tool: tool}); got.Effect != EffectDeny {
			t.Fatalf("wire tool %q borrowed allow entry: %+v", tool, got)
		}
	}
}

func TestRestrictivePaddedPredicateAndReviewCoverageRemains(t *testing.T) {
	for _, effect := range []string{EffectDeny, EffectPending} {
		engine := New(Config{DefaultEffect: EffectAllow, Predicates: []Predicate{{ID: "legacy", TenantID: " tenant ", AgentID: " agent ", Connector: " bank ", Tool: " write ", Path: "amount", Op: "eq", Value: 1, Effect: effect}}})
		if got := engine.Decide(context.Background(), PolicyInput{Stage: StageToolCall, TenantID: "tenant", AgentID: "agent", Connector: "bank", Tool: "write", Args: map[string]any{"amount": 1}}); got.Effect != effect {
			t.Fatalf("restriction narrowed: %+v", got)
		}
	}
	engine := New(Config{DefaultEffect: EffectAllow, MandatoryHITL: []MandatoryHITLRule{{ID: "legacy-review", TenantID: " tenant ", AgentID: " agent ", Connector: " bank ", Tools: []string{" write "}}}})
	if got := engine.Decide(context.Background(), PolicyInput{Stage: StageToolCall, TenantID: "tenant", AgentID: "agent", Connector: "bank", Tool: "write"}); got.Effect != EffectPending {
		t.Fatalf("review narrowed: %+v", got)
	}
}

func TestDecisionCacheExactTupleDoesNotAliasSeparators(t *testing.T) {
	engine := New(Config{CacheTTL: time.Hour, Grants: []Grant{{TenantID: "tenant", AgentID: "agent", Connector: "bank", Tools: &ToolFilter{Allow: []string{"send\x00other"}}}}})
	first := PolicyInput{Stage: StageToolCall, TenantID: "tenant", AgentID: "agent", Connector: "bank", Tool: "send\x00other"}
	second := first
	second.Connector = "bank\x00send"
	second.Tool = "other"
	if got := engine.Decide(context.Background(), first); got.Effect != EffectAllow {
		t.Fatal(got)
	}
	if got := engine.Decide(context.Background(), second); got.Effect != EffectDeny {
		t.Fatalf("separator-containing tuple borrowed cached authority: %+v", got)
	}
	if engine.CacheLen() != 2 {
		t.Fatal("distinct identities did not occupy distinct cache entries")
	}
}
