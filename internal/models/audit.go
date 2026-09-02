package models

import "time"

// AuditEvent is one durable policy (or security) decision row.
// Phase 1 writes policy decisions; Phase 2 adds Admin search (UI later).
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

// AuditSearchRequest filters Admin audit list pages
// (GET /admin-api/audit-events). SQL backends only (not Mongo).
type AuditSearchRequest struct {
	TenantID   string     `json:"tenant_id,omitempty"`
	Decision   string     `json:"decision,omitempty"` // allow | deny | pending
	Action     string     `json:"action,omitempty"`   // e.g. tool.call, connector.session
	ReasonCode string     `json:"reason_code,omitempty"`
	RunID      string     `json:"run_id,omitempty"`
	AgentID    string     `json:"agent_id,omitempty"`
	Connector  string     `json:"connector,omitempty"`
	Tool       string     `json:"tool,omitempty"`
	Since      *time.Time `json:"since,omitempty"` // inclusive lower bound on ts
	Until      *time.Time `json:"until,omitempty"` // exclusive upper bound on ts
	Limit      int        `json:"limit,omitempty"`
	Offset     int        `json:"offset,omitempty"`
	Cursor     string     `json:"cursor,omitempty"` // keyset on (ts, id); ignores Offset
}
