package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/ratelimit"
	"github.com/getrunkite/runkite/internal/tenant"
)

// governanceDecideMaxIDLen caps caller-supplied ids. Grants match on the
// strings as given, so this is a size limit, not a format check.
const governanceDecideMaxIDLen = 256

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

// governanceDecideResponse is allow or deny. Pending is not returned:
// this slice cannot wait for an approver, so a pending engine result is
// reported as deny and keeps the engine's reason_code. The audit row
// still records the engine effect (pending), which Decide writes itself.
type governanceDecideResponse struct {
	Effect     string `json:"effect"`
	Reason     string `json:"reason,omitempty"`
	ReasonCode string `json:"reason_code,omitempty"`
}

// handleGovernanceDecide answers whether the caller's tenant may run one
// tool. It does not create a run, dispatch a job, check kill switches, or
// check budgets. The caller executes the tool.
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
		writeError(w, http.StatusBadRequest, "agent_id, connector, tool, and run_id must be at most 256 characters")
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
	parsed, digest, meta := policy.BindArgs(req.Args)
	dec := s.policy.Decide(r.Context(), policy.PolicyInput{
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
	})

	effect := dec.Effect
	reason := dec.Reason
	if effect == policy.EffectPending {
		// No wait handle in this slice. Fail closed. The audit row already
		// says pending, which is the engine's real answer.
		effect = policy.EffectDeny
		if reason == "" {
			reason = "approval is not available on this endpoint"
		}
	}
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

func tooLongGovernanceID(s string) bool {
	return len(s) > governanceDecideMaxIDLen
}
