package models

import "time"

// MandatoryHITLRule forces human approval for matching connector/tool calls.
// Empty AgentID means the whole tenant; empty Tools means all tools on the connector.
type MandatoryHITLRule struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	AgentID   string    `json:"agent_id,omitempty"` // empty = whole tenant
	Connector string    `json:"connector"`
	Tools     []string  `json:"tools,omitempty"` // empty = all tools
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// MandatoryHITLSearchRequest filters Admin mandatory-HITL list pages.
type MandatoryHITLSearchRequest struct {
	TenantID  string `json:"tenant_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Connector string `json:"connector,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	Cursor    string `json:"cursor,omitempty"` // keyset on (tenant_id, id)
}
