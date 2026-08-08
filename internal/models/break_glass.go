package models

import "time"

// BreakGlassWindow is a time-bounded policy bypass for a tenant (or
// tenant+agent). While active it short-circuits policy Decide for
// run.create / connector.session / tool.call — it does not override kill,
// agent authz, or admission_limits.
type BreakGlassWindow struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	AgentID   string    `json:"agent_id,omitempty"` // empty = whole tenant
	Reason    string    `json:"reason"`
	CreatedBy string    `json:"created_by,omitempty"`
	StartsAt  time.Time `json:"starts_at"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// BreakGlassSearchRequest filters Admin break-glass list pages.
type BreakGlassSearchRequest struct {
	TenantID string `json:"tenant_id,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}
