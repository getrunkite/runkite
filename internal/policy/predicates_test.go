package policy

import (
	"context"
	"testing"
	"time"
)

func TestValidatePredicate_RejectsBadOpAndAllow(t *testing.T) {
	_, err := ValidatePredicate(Predicate{
		ID: "p", TenantID: "acme", Connector: "bank", Path: "amount", Op: "regex", Effect: EffectPending,
	})
	if err == nil {
		t.Fatal("want error for unknown op")
	}
	_, err = ValidatePredicate(Predicate{
		ID: "p", TenantID: "acme", Connector: "bank", Path: "amount", Op: "gt", Effect: EffectAllow,
	})
	if err == nil {
		t.Fatal("predicates must not allow")
	}
}

func TestPredicate_AmountPendingVsAllow(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank"}},
		Predicates: []Predicate{{
			ID: "transfer-over-100", TenantID: "acme", AgentID: "payroll",
			Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(100),
			Effect: EffectPending, Reason: "approval required for transfers over 100",
			ReasonCode: "predicate_amount",
		}},
	})
	base := PolicyInput{
		Stage: StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
	}
	low := base
	low.Args = map[string]any{"amount": float64(50)}
	low.ArgsDigest = "low"
	if dec := eng.Decide(context.Background(), low); dec.Effect != EffectAllow {
		t.Fatalf("amount 50: %+v", dec)
	}
	high := base
	high.Args = map[string]any{"amount": float64(250)}
	high.ArgsDigest = "high"
	dec := eng.Decide(context.Background(), high)
	if dec.Effect != EffectPending || dec.ReasonCode != "predicate_amount" || dec.RuleID != "transfer-over-100" {
		t.Fatalf("amount 250: %+v", dec)
	}
}

func TestPredicate_GrantDenyWinsOverPending(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{
			ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank",
			Tools: &ToolFilter{Deny: []string{"transfer"}},
		}},
		Predicates: []Predicate{{
			ID: "p", TenantID: "acme", Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(1), Effect: EffectPending,
		}},
	})
	dec := eng.Decide(context.Background(), PolicyInput{
		Stage: StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
		Args: map[string]any{"amount": float64(250)},
	})
	if dec.Effect != EffectDeny || dec.ReasonCode != ReasonPolicyToolDenied {
		t.Fatalf("grant deny must win: %+v", dec)
	}
}

func TestPredicate_MissingPathDoesNotMatch(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank"}},
		Predicates: []Predicate{{
			ID: "p", TenantID: "acme", Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(100), Effect: EffectPending,
		}},
	})
	dec := eng.Decide(context.Background(), PolicyInput{
		Stage: StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
		Args: map[string]any{"currency": "USD"},
	})
	if dec.Effect != EffectAllow {
		t.Fatalf("missing path should not match: %+v", dec)
	}
}

func TestPredicate_StringNumberNoMatch(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank"}},
		Predicates: []Predicate{{
			ID: "p", TenantID: "acme", Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(100), Effect: EffectPending,
		}},
	})
	dec := eng.Decide(context.Background(), PolicyInput{
		Stage: StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
		Args: map[string]any{"amount": "250"},
	})
	if dec.Effect != EffectAllow {
		t.Fatalf("string amount must not match numeric gt: %+v", dec)
	}
}

func TestPredicate_DenyWinsOverEarlierPending(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank"}},
		Predicates: []Predicate{
			{
				ID: "pending", TenantID: "acme", Connector: "bank", Tool: "transfer",
				Path: "amount", Op: "gt", Value: float64(100), Effect: EffectPending,
			},
			{
				ID: "deny", TenantID: "acme", Connector: "bank", Tool: "transfer",
				Path: "amount", Op: "gt", Value: float64(1000), Effect: EffectDeny,
			},
		},
	})
	dec := eng.Decide(context.Background(), PolicyInput{
		Stage: StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
		Args: map[string]any{"amount": float64(1500)},
	})
	if dec.Effect != EffectDeny || dec.RuleID != "deny" {
		t.Fatalf("first matching deny should win: %+v", dec)
	}
}

func TestPredicate_OnlyEnablesEngine(t *testing.T) {
	eng := New(Config{
		Predicates: []Predicate{{
			ID: "p", TenantID: "acme", Connector: "bank",
			Path: "amount", Op: "gt", Value: float64(100), Effect: EffectPending,
		}},
	})
	if eng == nil {
		t.Fatal("non-empty predicates should enable the engine")
	}
}

func TestPredicate_PendingNotCached(t *testing.T) {
	eng := New(Config{
		CacheTTL: time.Hour,
		Grants:   []Grant{{ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank"}},
		Predicates: []Predicate{{
			ID: "p", TenantID: "acme", Connector: "bank", Tool: "transfer",
			Path: "amount", Op: "gt", Value: float64(100), Effect: EffectPending,
		}},
	})
	in := PolicyInput{
		Stage: StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
		Args: map[string]any{"amount": float64(250)}, ArgsDigest: "high",
	}
	for i := 0; i < 3; i++ {
		if dec := eng.Decide(context.Background(), in); dec.Effect != EffectPending {
			t.Fatalf("call %d: %+v", i, dec)
		}
	}
	if n := eng.CacheLen(); n != 0 {
		t.Fatalf("CacheLen=%d want 0 (pending must not be cached)", n)
	}
}

func TestPredicate_ContainsAndInAndExists(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{ID: "g", TenantID: "acme", AgentID: "ops", Connector: "mail"}},
		Predicates: []Predicate{
			{
				ID: "subj", TenantID: "acme", Connector: "mail", Tool: "send",
				Path: "subject", Op: "contains", Value: "wire", Effect: EffectDeny,
			},
			{
				ID: "dest", TenantID: "acme", Connector: "mail", Tool: "send",
				Path: "to", Op: "in", Value: []any{"evil@example.com"}, Effect: EffectDeny,
			},
			{
				ID: "cc", TenantID: "acme", Connector: "mail", Tool: "send",
				Path: "bcc", Op: "exists", Effect: EffectPending,
			},
		},
	})
	base := PolicyInput{Stage: StageToolCall, TenantID: "acme", AgentID: "ops", Connector: "mail", Tool: "send"}
	sub := base
	sub.Args = map[string]any{"subject": "please wire funds", "to": "ok@example.com"}
	if dec := eng.Decide(context.Background(), sub); dec.Effect != EffectDeny || dec.RuleID != "subj" {
		t.Fatalf("contains: %+v", dec)
	}
	dest := base
	dest.Args = map[string]any{"subject": "hi", "to": "evil@example.com"}
	if dec := eng.Decide(context.Background(), dest); dec.Effect != EffectDeny || dec.RuleID != "dest" {
		t.Fatalf("in: %+v", dec)
	}
	bcc := base
	bcc.Args = map[string]any{"subject": "hi", "to": "ok@example.com", "bcc": "hidden@example.com"}
	if dec := eng.Decide(context.Background(), bcc); dec.Effect != EffectPending || dec.RuleID != "cc" {
		t.Fatalf("exists: %+v", dec)
	}
}

func TestPredicate_NestedPathAndIndex(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank"}},
		Predicates: []Predicate{{
			ID: "p", TenantID: "acme", Connector: "bank", Tool: "transfer",
			Path: "items.0.amount", Op: "gte", Value: float64(100), Effect: EffectDeny,
		}},
	})
	dec := eng.Decide(context.Background(), PolicyInput{
		Stage: StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
		Args: map[string]any{"items": []any{map[string]any{"amount": float64(100)}}},
	})
	if dec.Effect != EffectDeny {
		t.Fatalf("nested index path: %+v", dec)
	}
}

func TestPredicate_SkipsSession(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank"}},
		Predicates: []Predicate{{
			ID: "p", TenantID: "acme", Connector: "bank",
			Path: "amount", Op: "exists", Effect: EffectDeny,
		}},
	})
	dec := eng.Decide(context.Background(), PolicyInput{
		Stage: StageConnectorSession, TenantID: "acme", AgentID: "payroll",
		Connector: "bank",
		Args:      map[string]any{"amount": float64(1)},
	})
	if dec.Effect != EffectAllow {
		t.Fatalf("session must ignore predicates: %+v", dec)
	}
}

func TestPredicate_InvalidSkipped(t *testing.T) {
	eng := New(Config{
		Grants: []Grant{{ID: "g", TenantID: "acme", AgentID: "payroll", Connector: "bank"}},
		Predicates: []Predicate{{
			ID: "bad", TenantID: "acme", Connector: "bank",
			Path: "amount", Op: "nope", Effect: EffectDeny,
		}},
	})
	if eng == nil {
		t.Fatal("engine should still build from grants")
	}
	dec := eng.Decide(context.Background(), PolicyInput{
		Stage: StageToolCall, TenantID: "acme", AgentID: "payroll",
		Connector: "bank", Tool: "transfer",
		Args: map[string]any{"amount": float64(999)},
	})
	if dec.Effect != EffectAllow {
		t.Fatalf("invalid predicate must not fail-closed: %+v", dec)
	}
}
