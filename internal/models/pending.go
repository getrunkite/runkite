package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/jsonvalue"
)

// Pending action statuses for connector HITL.
const (
	PendingStatusPending  = "pending"
	PendingStatusApproved = "approved"
	PendingStatusDenied   = "denied"
	PendingStatusConsumed = "consumed"

	// PendingReasonExpired is set when an embedded-governance row passes
	// its expires_at while still pending. Connector HITL rows leave
	// expires_at empty and are not swept.
	PendingReasonExpired = "pending_expired"
)

// PendingAction is one connector tool call waiting on operator approval.
type PendingAction struct {
	ID    string `json:"id"`
	RunID string `json:"run_id"`
	// RunIncarnationID pins managed approval authority to the stored run row.
	// Embedded decisions leave it empty because they do not create a run.
	RunIncarnationID string `json:"run_incarnation_id,omitempty"`
	// TargetRevision pins managed authority to a nonsecret connector target descriptor.
	TargetRevision string         `json:"target_revision,omitempty"`
	Generation     int64          `json:"generation"`
	TenantID       string         `json:"tenant_id"`
	AgentID        string         `json:"agent_id"`
	Connector      string         `json:"connector"`
	Tool           string         `json:"tool"`
	RuleID         string         `json:"rule_id,omitempty"`
	Reason         string         `json:"reason,omitempty"`
	ReasonCode     string         `json:"reason_code,omitempty"`
	Status         string         `json:"status"`
	ArgsDigest     string         `json:"args_digest,omitempty"`
	Args           map[string]any `json:"args,omitempty"`
	DecidedBy      string         `json:"decided_by,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	// ExpiresAt is set only for embedded-governance rows. Nil means the
	// row waits until an operator decides, which is connector HITL.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
	if err := jsonvalue.Unmarshal(b, &m); err != nil || len(m) == 0 {
		return nil
	}
	return m
}

// PendingActionSearchRequest filters Admin pending-action lists.
type PendingActionSearchRequest struct {
	TenantID       string `json:"tenant_id,omitempty"`
	TargetRevision string `json:"target_revision,omitempty"`
	Status         string `json:"status,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	Connector      string `json:"connector,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	Offset         int    `json:"offset,omitempty"`
	Cursor         string `json:"cursor,omitempty"`
}

// PendingActionKey is an unambiguous, fixed-size key for an open approval.
// Embedded decisions and connector capabilities occupy different namespaces.
func PendingActionKey(a *PendingAction) string {
	parts := []any{a.TenantID, a.AgentID, a.ExpiresAt != nil,
		a.RunID, a.Generation, a.Connector, a.Tool, a.ArgsDigest}
	// Preserve historical standalone/unbound keys. A structured new suffix
	// cannot collide with an old incarnation string, even for store-only fixtures.
	if a.ExpiresAt == nil {
		if a.TargetRevision != "" {
			parts = append(parts, []string{"target-bound-v1", a.RunIncarnationID, a.TargetRevision})
		} else if a.RunIncarnationID != "" {
			parts = append(parts, a.RunIncarnationID)
		}
	}
	value, _ := json.Marshal(parts)
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

// ValidConnectorTargetRevision recognizes the complete versioned target digest.
// It never accepts an absent or malformed managed approval binding.
func ValidConnectorTargetRevision(value string) bool {
	const prefix = "target-v1-"
	if len(value) != len(prefix)+64 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for _, c := range value[len(prefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
