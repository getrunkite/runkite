package models

import "time"

// PolicyGrant is one durable connector grant row (Admin CRUD / Postgres).
// Deployment defaults still live in langgraph.json; DB rows overlay them.
type PolicyGrant struct {
	ID        string            `json:"id"`
	TenantID  string            `json:"tenant_id"`
	AgentID   string            `json:"agent_id"`
	Connector string            `json:"connector"`
	Tools     *PolicyToolFilter `json:"tools,omitempty"`
	CreatedAt time.Time         `json:"created_at,omitempty"`
	UpdatedAt time.Time         `json:"updated_at,omitempty"`
}

// PolicyToolFilter is allow/deny for tools within a grant.
type PolicyToolFilter struct {
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`
}

// PolicyGrantSearchRequest filters Admin grant list pages.
type PolicyGrantSearchRequest struct {
	TenantID  string `json:"tenant_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	Connector string `json:"connector,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	Cursor    string `json:"cursor,omitempty"` // keyset on (tenant_id, id)
}
