package models

import "time"

// KillSwitch refuses new run creates for a tenant (agent_id empty) or a
// single agent. When PauseOnly is false, activating also cancels
// non-terminal runs in scope (blast-radius drain).
type KillSwitch struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	AgentID   string    `json:"agent_id,omitempty"` // empty = whole tenant
	PauseOnly bool      `json:"pause_only"`
	Reason    string    `json:"reason,omitempty"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// KillSwitchSearchRequest filters Admin kill-switch list pages.
type KillSwitchSearchRequest struct {
	TenantID string `json:"tenant_id,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
}
