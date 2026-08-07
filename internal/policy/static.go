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
	tenant := strings.TrimSpace(in.TenantID)
	agent := strings.TrimSpace(in.AgentID)
	connector := strings.TrimSpace(in.Connector)

	var matched *Grant
	for i := range s.grants {
		g := &s.grants[i]
		if strings.TrimSpace(g.TenantID) != tenant {
			continue
		}
		if strings.TrimSpace(g.AgentID) != agent {
			continue
		}
		if strings.TrimSpace(g.Connector) != connector {
			continue
		}
		matched = g
		break
	}
	if matched == nil {
		return PolicyDecision{
			Effect:     EffectDeny,
			Reason:     "no grant for tenant/agent/connector",
			ReasonCode: ReasonPolicyNoGrant,
		}
	}

	if in.Stage == StageToolCall {
		tool := strings.TrimSpace(in.Tool)
		if tool == "" || !toolAllowed(matched.Tools, tool) {
			return PolicyDecision{
				Effect:     EffectDeny,
				Reason:     "tool not allowed by grant",
				ReasonCode: ReasonPolicyToolDenied,
				RuleID:     matched.ID,
			}
		}
	}

	return PolicyDecision{
		Effect: EffectAllow,
		Reason: "matched grant",
		RuleID: matched.ID,
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
