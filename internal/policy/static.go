package policy

import (
	"context"
	"strings"
)

// Grant is one connector grant from langgraph.json policy.grants.
type Grant struct {
	ID        string      `json:"id,omitempty"`
	TenantID  string      `json:"tenant_id"`
	AgentID   string      `json:"agent_id"`
	Connector string      `json:"connector"` // connector name
	Tools     *ToolFilter `json:"tools,omitempty"`
}

// ToolFilter mirrors connector.ToolFilter semantics for per-grant tools.
type ToolFilter struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// Static evaluates in-process grants. Unmatched → Deny (caller applies
// default_effect only when no static layer exists).
type Static struct {
	grants []Grant
}

// NewStatic copies grants for evaluation.
func NewStatic(grants []Grant) *Static {
	out := make([]Grant, len(grants))
	copy(out, grants)
	return &Static{grants: out}
}

// Decide implements Provider for static grants.
func (s *Static) Decide(_ context.Context, in PolicyInput) (PolicyDecision, error) {
	return s.decide(in), nil
}

func (s *Static) decide(in PolicyInput) PolicyDecision {
	// Preserve legacy restrictive coverage, including under default allow.
	// Only the exact lookup below can grant authority. A trimmed-scope
	// filter can still deny, so previously denied padded configurations do
	// not become allowed merely because positive matching is now exact.
	if in.Stage == StageToolCall {
		for i := range s.grants {
			g := &s.grants[i]
			if legacyRestrictiveGrantScope(g, in) &&
				(!toolAllowed(g.Tools, strings.TrimSpace(in.Tool)) || !toolAllowed(g.Tools, in.Tool) || strings.TrimSpace(in.Tool) == "") {
				return grantToolDenied(g)
			}
		}
	}

	var matched *Grant
	for i := range s.grants {
		g := &s.grants[i]
		if g.TenantID == in.TenantID && g.AgentID == in.AgentID && g.Connector == in.Connector {
			matched = g
			break
		}
	}
	if matched == nil {
		return PolicyDecision{
			Effect:     EffectDeny,
			Reason:     "no grant for tenant/agent/connector",
			ReasonCode: ReasonPolicyNoGrant,
		}
	}

	return PolicyDecision{
		Effect: EffectAllow,
		Reason: "matched grant",
		RuleID: matched.ID,
	}
}

// Legacy matching is restrictive only. Never use it to select an allow grant.
func legacyRestrictiveGrantScope(g *Grant, in PolicyInput) bool {
	return strings.TrimSpace(g.TenantID) == strings.TrimSpace(in.TenantID) &&
		strings.TrimSpace(g.AgentID) == strings.TrimSpace(in.AgentID) &&
		strings.TrimSpace(g.Connector) == strings.TrimSpace(in.Connector)
}

func grantToolDenied(g *Grant) PolicyDecision {
	return PolicyDecision{
		Effect:     EffectDeny,
		Reason:     "tool not allowed by grant",
		ReasonCode: ReasonPolicyToolDenied,
		RuleID:     g.ID,
	}
}

// toolAllowed: deny wins; non-empty allow must contain tool; nil filter = all.
func toolAllowed(filter *ToolFilter, tool string) bool {
	if filter == nil {
		return true
	}
	for _, d := range filter.Deny {
		if d == tool {
			return false
		}
	}
	if len(filter.Allow) == 0 {
		return true
	}
	for _, a := range filter.Allow {
		if a == tool {
			return true
		}
	}
	return false
}
