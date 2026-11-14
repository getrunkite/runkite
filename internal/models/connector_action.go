package models

import "time"

const (
	ConnectorActionReceived         = "received"
	ConnectorActionBlocked          = "blocked"
	ConnectorActionApprovalRequired = "approval_required"
	ConnectorActionReady            = "ready"
	ConnectorActionNotDispatched    = "not_dispatched"
	ConnectorActionDispatchIntent   = "dispatch_intent"
	ConnectorActionResponseObserved = "response_observed"
	ConnectorActionOutcomeUnknown   = "outcome_unknown"
)

// ConnectorAction is metadata about one managed MCP tools/call request. A
// response is evidence of that response, not proof of a business effect.
type ConnectorAction struct {
	// ExportSinkID pins the operator configured delivery destination identity.
	ExportSinkID        string     `json:"export_sink_id,omitempty"`
	ID                  string     `json:"action_id"`
	TenantID            string     `json:"tenant_id"`
	RunID               string     `json:"run_id"`
	RunCreatedAt        time.Time  `json:"run_created_at"`
	RunIncarnationID    string     `json:"run_incarnation_id"`
	ThreadID            string     `json:"thread_id"`
	Generation          int64      `json:"generation"`
	AgentID             string     `json:"agent_id"`
	AgentVersion        int        `json:"agent_version"`
	Principal           string     `json:"principal,omitempty"`
	Connector           string     `json:"connector"`
	Tool                string     `json:"tool"`
	Transport           string     `json:"transport"`
	ArgsDigest          string     `json:"args_digest"`
	State               string     `json:"state"`
	Version             int64      `json:"version"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	Decision            string     `json:"decision,omitempty"`
	ReasonCode          string     `json:"reason_code,omitempty"`
	RuleID              string     `json:"rule_id,omitempty"`
	AuthorizationSource string     `json:"authorization_source,omitempty"`
	BreakGlassID        string     `json:"break_glass_id,omitempty"`
	PendingActionID     string     `json:"pending_action_id,omitempty"`
	PolicyRevision      *string    `json:"policy_revision,omitempty"`
	TargetRevision      *string    `json:"target_revision,omitempty"`
	ToolRevision        *string    `json:"tool_revision,omitempty"`
	CredentialRef       *string    `json:"credential_ref,omitempty"`
	TraceID             string     `json:"trace_id,omitempty"`
	DispatchID          string     `json:"dispatch_id,omitempty"`
	DispatchDeadlineAt  *time.Time `json:"dispatch_deadline_at,omitempty"`
	ResponseKind        string     `json:"response_kind,omitempty"`
	HTTPStatus          *int       `json:"http_status,omitempty"`
	RPCErrorCode        *int64     `json:"rpc_error_code,omitempty"`
	ResponseDigest      string     `json:"response_digest,omitempty"`
	ResponseBytes       *int64     `json:"response_bytes,omitempty"`
}

// ConnectorActionDecision records policy and the effective authority separately.
// PendingActionID links existing approvals; it does not consume their authority.
type ConnectorActionDecision struct {
	Effect              string  `json:"effect"`
	ReasonCode          string  `json:"reason_code,omitempty"`
	RuleID              string  `json:"rule_id,omitempty"`
	AuthorizationSource string  `json:"authorization_source,omitempty"`
	BreakGlassID        string  `json:"break_glass_id,omitempty"`
	PendingActionID     string  `json:"pending_action_id,omitempty"`
	PolicyRevision      *string `json:"policy_revision,omitempty"`
}

// ConnectorActionObservation excludes arguments, payloads and provider text.
type ConnectorActionObservation struct {
	State          string `json:"state"`
	ReasonCode     string `json:"reason_code,omitempty"`
	ResponseKind   string `json:"response_kind,omitempty"`
	HTTPStatus     *int   `json:"http_status,omitempty"`
	RPCErrorCode   *int64 `json:"rpc_error_code,omitempty"`
	ResponseDigest string `json:"response_digest,omitempty"`
	ResponseBytes  *int64 `json:"response_bytes,omitempty"`
}

// ConnectorActionEvent is an append-only versioned metadata snapshot. Its
// mutation identity makes SQL persistence retryable without replaying a tool.
type ConnectorActionEvent struct {
	ActionID            string    `json:"action_id"`
	TenantID            string    `json:"tenant_id"`
	Sequence            int64     `json:"sequence"`
	MutationID          string    `json:"mutation_id"`
	Kind                string    `json:"kind"`
	ObservedAt          time.Time `json:"observed_at"`
	Observer            string    `json:"observer"`
	State               string    `json:"state"`
	Decision            string    `json:"decision,omitempty"`
	ReasonCode          string    `json:"reason_code,omitempty"`
	RuleID              string    `json:"rule_id,omitempty"`
	AuthorizationSource string    `json:"authorization_source,omitempty"`
	BreakGlassID        string    `json:"break_glass_id,omitempty"`
	PendingActionID     string    `json:"pending_action_id,omitempty"`
	DispatchID          string    `json:"dispatch_id,omitempty"`
	ResponseKind        string    `json:"response_kind,omitempty"`
	HTTPStatus          *int      `json:"http_status,omitempty"`
	RPCErrorCode        *int64    `json:"rpc_error_code,omitempty"`
	ResponseDigest      string    `json:"response_digest,omitempty"`
	ResponseBytes       *int64    `json:"response_bytes,omitempty"`
}

type ConnectorActionSearchRequest struct {
	TenantID  string
	RunID     string
	AgentID   string
	Connector string
	State     string
	Limit     int
	Offset    int
	Cursor    string
}
