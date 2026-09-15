package models

import (
	"encoding/json"
	"time"
)

// Pending action statuses for connector HITL.
const (
	PendingStatusPending  = "pending"
	PendingStatusApproved = "approved"
	PendingStatusDenied   = "denied"
	PendingStatusConsumed = "consumed"
)

// PendingAction is one connector tool call waiting on operator approval.
type PendingAction struct {
	ID         string         `json:"id"`
	RunID      string         `json:"run_id"`
	Generation int64          `json:"generation"`
	TenantID   string         `json:"tenant_id"`
	AgentID    string         `json:"agent_id"`
	Connector  string         `json:"connector"`
	Tool       string         `json:"tool"`
	RuleID     string         `json:"rule_id,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	ReasonCode string         `json:"reason_code,omitempty"`
	Status     string         `json:"status"`
	ArgsDigest string         `json:"args_digest,omitempty"`
	Args       map[string]any `json:"args,omitempty"`
	DecidedBy  string         `json:"decided_by,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// MarshalPendingArgs is the JSON stored on pending_actions.args (display map).
func MarshalPendingArgs(m map[string]any) []byte {
	if len(m) == 0 {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// UnmarshalPendingArgs reads pending_actions.args.
func UnmarshalPendingArgs(b []byte) map[string]any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

// PendingActionSearchRequest filters Admin pending-action lists.
type PendingActionSearchRequest struct {
	TenantID  string `json:"tenant_id,omitempty"`
	Status    string `json:"status,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Connector string `json:"connector,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	Cursor    string `json:"cursor,omitempty"`
}
