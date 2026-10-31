package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/ratelimit"
	"github.com/getrunkite/runkite/internal/tenant"
)

// governanceDecideMaxIDLen caps caller-supplied ids. Grants match on the
// strings as given, so this is a size limit, not a format check.
const governanceDecideMaxIDLen = 256

// governancePendingTTL is how long an embedded approval stays pending.
// Connector HITL rows leave expires_at empty and are not swept.
// ponytail: fixed 24h, not config. Upgrade path is a langgraph.json
// duration if operators need a different ceiling.
var governancePendingTTL = 24 * time.Hour

// governanceDecideRequest is POST /governance/decide. tenant_id is accepted
// only so a mismatched claim can be rejected. It never selects the tenant.
type governanceDecideRequest struct {
	TenantID  *string         `json:"tenant_id"`
	AgentID   string          `json:"agent_id"`
	Connector string          `json:"connector"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	RunID     string          `json:"run_id"`
}

// governanceDecideResponse is allow, deny, or pending. Pending includes
// action_id. The caller polls GET /governance/actions/{action_id}.
type governanceDecideResponse struct {
	Effect     string `json:"effect"`
	Reason     string `json:"reason,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
	ActionID   string `json:"action_id,omitempty"`
}

// governanceActionResponse is the poll body. status is pending, approved,
// rejected, or consumed. denied is reported as rejected.
type governanceActionResponse struct {
	ActionID   string `json:"action_id"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
}

// handleGovernanceDecide answers whether the caller's tenant may run one
// tool. It does not create a run, dispatch a job, check kill switches, or
// check budgets. The caller executes the tool after allow, or after a
// later poll says approved.
func (s *Server) handleGovernanceDecide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.policy.Enabled() {
		writeError(w, http.StatusConflict, "policy engine is not enabled")
		return
	}

	var req governanceDecideRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.Connector = strings.TrimSpace(req.Connector)
	req.Tool = strings.TrimSpace(req.Tool)
	req.RunID = strings.TrimSpace(req.RunID)
	if req.AgentID == "" || req.Connector == "" || req.Tool == "" {
		writeError(w, http.StatusBadRequest, "agent_id, connector, and tool are required")
		return
	}
	if tooLongGovernanceID(req.AgentID) || tooLongGovernanceID(req.Connector) ||
		tooLongGovernanceID(req.Tool) || tooLongGovernanceID(req.RunID) {
		writeError(w, http.StatusBadRequest, "agent_id, connector, tool, and run_id must be at most 256 UTF-8 bytes")
		return
	}

	tenantID := tenant.FromContext(r.Context())
	if req.TenantID != nil && strings.TrimSpace(*req.TenantID) != tenantID {
		writeError(w, http.StatusForbidden, "tenant_id does not match the authenticated tenant")
		return
	}

	if !s.rateLimit.AllowAgent(req.AgentID) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, (&ratelimit.ErrRateLimited{Scope: "agent"}).Error())
		return
	}

	principal := ""
	if ar := auth.FromContext(r.Context()); ar != nil {
		principal = ar.Identity
	}
	parsed, digest, meta, err := policy.BindArgsStrict(req.Args)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid or ambiguous tool arguments")
		return
	}
	in := policy.PolicyInput{
		Stage:      policy.StageToolCall,
		TenantID:   tenantID,
		Principal:  principal,
		AgentID:    req.AgentID,
		RunID:      req.RunID,
		Generation: 0,
		Connector:  req.Connector,
		Tool:       req.Tool,
		Args:       parsed,
		ArgsDigest: digest,
		ArgsMeta:   meta,
	}
	dec := s.policy.Decide(r.Context(), in)

	if dec.Effect == policy.EffectPending {
		s.writeGovernancePending(w, r, in, dec)
		return
	}
	effect := dec.Effect
	reason := dec.Reason
	if effect != policy.EffectAllow && effect != policy.EffectDeny {
		effect = policy.EffectDeny
		if reason == "" {
			reason = "denied by policy"
		}
	}
	writeJSON(w, http.StatusOK, governanceDecideResponse{
		Effect:     effect,
		Reason:     reason,
		ReasonCode: dec.ReasonCode,
	})
}

func (s *Server) writeGovernancePending(w http.ResponseWriter, r *http.Request, in policy.PolicyInput, dec policy.PolicyDecision) {
	if in.RunID == "" {
		writeError(w, http.StatusBadRequest, "run_id is required when the decision is pending")
		return
	}
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	if err := store.ExpireGovernancePending(tenant.SystemContext(r.Context()), time.Now().UTC()); err != nil {
		handleStoreError(w, err)
		return
	}
	expires := time.Now().UTC().Add(governancePendingTTL)
	id, err := s.persistPendingAction(r.Context(), in, in.Connector, in.Tool, dec, &expires)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, governanceDecideResponse{
		Effect:     policy.EffectPending,
		Reason:     dec.Reason,
		ReasonCode: dec.ReasonCode,
		ActionID:   id,
	})
}

// handleGovernanceGetAction reports the operator's decision for one
// embedded pending row. It does not consume the approval. The app
// executes the tool after status is approved.
func (s *Server) handleGovernanceGetAction(w http.ResponseWriter, r *http.Request) {
	store, ok := s.pendingActions()
	if !ok {
		writeError(w, http.StatusNotImplemented, "pending actions require a SQL state backend (Postgres, MySQL, or SQLite)")
		return
	}
	if err := store.ExpireGovernancePending(tenant.SystemContext(r.Context()), time.Now().UTC()); err != nil {
		handleStoreError(w, err)
		return
	}
	id := strings.TrimSpace(r.PathValue("actionID"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "action id is required")
		return
	}
	a, err := store.GetPendingAction(r.Context(), id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	if a.TenantID != tenant.FromContext(r.Context()) {
		writeError(w, http.StatusNotFound, "pending action not found")
		return
	}
	if !currentPendingBinding(a) && (a.Status == models.PendingStatusPending || a.Status == models.PendingStatusApproved) {
		writeJSON(w, http.StatusOK, governanceActionResponse{ActionID: a.ID, Status: "rejected", Reason: "resubmit the operation for fresh approval", ReasonCode: "approval_binding_legacy"})
		return
	}
	writeJSON(w, http.StatusOK, governanceActionResponse{
		ActionID:   a.ID,
		Status:     governanceActionStatus(a.Status),
		Reason:     a.Reason,
		ReasonCode: a.ReasonCode,
	})
}

func governanceActionStatus(status string) string {
	switch status {
	case models.PendingStatusPending, models.PendingStatusApproved, models.PendingStatusConsumed:
		return status
	default:
		return "rejected"
	}
}

func tooLongGovernanceID(s string) bool {
	return len(s) > governanceDecideMaxIDLen
}

// SetGovernancePendingTTLForTest sets the embedded approval ceiling and
// returns a restore func. Production code does not call this.
func SetGovernancePendingTTLForTest(d time.Duration) (restore func()) {
	prev := governancePendingTTL
	governancePendingTTL = d
	return func() { governancePendingTTL = prev }
}
