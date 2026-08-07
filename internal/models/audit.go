package models

import "time"

// AuditEvent is one durable policy (or security) decision row.
// Phase 1 writes policy decisions; Phase 2 adds search/Admin UI.
type AuditEvent struct {
	ID           string                 `json:"id"`
	TS           time.Time              `json:"ts"`
	TenantID     string                 `json:"tenant_id"`
	Actor        string                 `json:"actor,omitempty"`
	Action       string                 `json:"action"`
	ResourceType string                 `json:"resource_type,omitempty"`
	ResourceID   string                 `json:"resource_id,omitempty"`
	Decision     string                 `json:"decision"`
	ReasonCode   string                 `json:"reason_code,omitempty"`
	RuleID       string                 `json:"rule_id,omitempty"`
	LatencyMs    int                    `json:"latency_ms,omitempty"`
	RunID        string                 `json:"run_id,omitempty"`
	Generation   int64                  `json:"generation,omitempty"`
	AgentID      string                 `json:"agent_id,omitempty"`
	Connector    string                 `json:"connector,omitempty"`
	Tool         string                 `json:"tool,omitempty"`
	Attrs        map[string]interface{} `json:"attrs,omitempty"`
	TraceID      string                 `json:"trace_id,omitempty"`
}
