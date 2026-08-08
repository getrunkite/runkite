package policy

import "strings"

// ReasonMandatoryHITL is returned when a config mandatory_hitl rule
// forces tool.call to pending despite an underlying allow.
const ReasonMandatoryHITL = "mandatory_hitl"

// MandatoryHITLRule forces connector tool.call decisions to pending when
// they would otherwise allow. Empty AgentID matches every agent in the
// tenant; empty Tools matches every tool on the connector. Hard deny from
// Decide is never elevated to pending.
type MandatoryHITLRule struct {
	ID        string   `json:"id,omitempty"`
	TenantID  string   `json:"tenant_id"`
	AgentID   string   `json:"agent_id,omitempty"` // empty = whole tenant
	Connector string   `json:"connector"`
	Tools     []string `json:"tools,omitempty"` // empty = all tools
}

// applyMandatoryHITL overrides EffectAllow on tool.call when a rule
// matches. Deny and existing pending are left unchanged. Break-glass
// bypasses Decide entirely, so it also skips this override.
func (e *Engine) applyMandatoryHITL(in PolicyInput, dec PolicyDecision) PolicyDecision {
	if e == nil || in.Stage != StageToolCall || dec.Effect != EffectAllow {
		return dec
	}
	rule := e.matchMandatoryHITL(in)
	if rule == nil {
		return dec
	}
	id := strings.TrimSpace(rule.ID)
	if id == "" {
		id = "mandatory_hitl"
	}
	return PolicyDecision{
		Effect:     EffectPending,
		Reason:     "mandatory HITL for tool",
		ReasonCode: ReasonMandatoryHITL,
		RuleID:     id,
		LatencyMs:  dec.LatencyMs,
	}
}

// MatchMandatoryHITL reports whether a rule would force pending for this
// input (observability / break-glass audit). Nil when none match.
func (e *Engine) MatchMandatoryHITL(in PolicyInput) *MandatoryHITLRule {
	return e.matchMandatoryHITL(in)
}

func (e *Engine) matchMandatoryHITL(in PolicyInput) *MandatoryHITLRule {
	e.mu.RLock()
	rules := e.mandatoryHITL
	e.mu.RUnlock()
	if len(rules) == 0 {
		return nil
	}
	tenant := strings.TrimSpace(in.TenantID)
	agent := strings.TrimSpace(in.AgentID)
	connector := strings.TrimSpace(in.Connector)
	tool := strings.TrimSpace(in.Tool)
	var tenantWide *MandatoryHITLRule
	for i := range rules {
		r := &rules[i]
		if strings.TrimSpace(r.TenantID) != tenant {
			continue
		}
		if strings.TrimSpace(r.Connector) != connector {
			continue
		}
		if !mandatoryToolMatch(r.Tools, tool) {
			continue
		}
		aid := strings.TrimSpace(r.AgentID)
		if aid == "" {
			if tenantWide == nil {
				tenantWide = r
			}
			continue
		}
		if aid == agent {
			return r // agent-scoped wins over tenant-wide
		}
	}
	return tenantWide
}

func mandatoryToolMatch(tools []string, tool string) bool {
	if len(tools) == 0 {
		return tool != ""
	}
	for _, t := range tools {
		if strings.TrimSpace(t) == tool {
			return true
		}
	}
	return false
}
