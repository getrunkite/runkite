package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// Action identifiers are opaque to clients. The prefix records which argument
// binding issued the row without rewriting historical approval audit records.
const pendingActionIDPrefix = "pending-v2-"

func currentPendingBinding(a *models.PendingAction) bool {
	return a != nil && a.ArgsDigest != "" && strings.HasPrefix(a.ID, pendingActionIDPrefix) &&
		(a.ExpiresAt != nil || (a.RunIncarnationID != "" && models.ValidConnectorTargetRevision(a.TargetRevision)))
}

// policyInputFromRequest builds a PolicyInput from run-binding (preferred)
// or falls back to client-auth context (pre-warm at createRun).
func policyInputFromRequest(ctx context.Context, stage, connectorName, tool string) policy.PolicyInput {
	in := policy.PolicyInput{
		Stage:     stage,
		Connector: connectorName,
		Tool:      tool,
		TenantID:  tenant.FromContext(ctx),
	}
	if b := auth.RunBindingFromContext(ctx); b != nil {
		in.TenantID = b.TenantID
		in.AgentID = b.AgentID
		in.RunID = b.RunID
		in.Generation = b.Generation
		if b.User != nil {
			in.Principal = b.User.Identity
		}
		return in
	}
	if ar := auth.FromContext(ctx); ar != nil {
		in.Principal = ar.Identity
		if ar.TenantID != "" {
			in.TenantID = ar.TenantID
		}
	}
	return in
}

func (s *Server) policyInput(ctx context.Context, stage, connectorName, tool string) policy.PolicyInput {
	in := policyInputFromRequest(ctx, stage, connectorName, tool)
	// Pre-warm / createRun may not have AgentID on binding; callers that
	// know the agent should set it on context via withPolicyAgent.
	if v, ok := ctx.Value(policyAgentKey{}).(string); ok && in.AgentID == "" {
		in.AgentID = v
	}
	if v, ok := ctx.Value(policyRunKey{}).(string); ok && in.RunID == "" {
		in.RunID = v
	}
	return in
}

// checkConnectorPolicy runs the policy engine when enabled. Returns
// (decision, true) when the caller should deny; (zero, false) to proceed.
func (s *Server) checkConnectorPolicy(ctx context.Context, in policy.PolicyInput) (policy.PolicyDecision, bool) {
	return s.checkConnectorPolicyEvaluation(ctx, in, nil)
}

type connectorPolicyEvaluation struct {
	Original     policy.PolicyDecision
	BreakGlassID string
}

func (s *Server) checkConnectorPolicyEvaluation(ctx context.Context, in policy.PolicyInput, evaluation *connectorPolicyEvaluation) (policy.PolicyDecision, bool) {
	if !s.policy.Enabled() {
		return policy.PolicyDecision{}, false
	}
	// In required-audit mode, establish a durable decision before any
	// exceptional authorization. An audit outage cannot become a bypass.
	if !s.policy.AuditRequired() && s.tryBreakGlassBypass(ctx, in) {
		return policy.PolicyDecision{
			Effect:     policy.EffectAllow,
			Reason:     "break-glass window active",
			ReasonCode: policy.ReasonBreakGlass,
		}, false
	}
	dec := s.policy.Decide(ctx, in)
	if evaluation != nil {
		evaluation.Original = dec
	}
	if !dec.AuditFailed && s.policy.AuditRequired() {
		if windowID := s.tryBreakGlassBypassID(ctx, in); windowID != "" {
			if evaluation != nil {
				evaluation.BreakGlassID = windowID
			}
			return policy.PolicyDecision{
				Effect:     policy.EffectAllow,
				Reason:     "break-glass window active",
				ReasonCode: policy.ReasonBreakGlass,
			}, false
		}
	}
	if dec.Effect != policy.EffectAllow {
		return dec, true
	}
	return dec, false
}

type policyAgentKey struct{}
type policyRunKey struct{}

// withPolicyAgent attaches agent_id for policy checks outside run-binding
// (createRun connector pre-warm).
func withPolicyAgent(ctx context.Context, agentID, runID string) context.Context {
	ctx = context.WithValue(ctx, policyAgentKey{}, agentID)
	if runID != "" {
		ctx = context.WithValue(ctx, policyRunKey{}, runID)
	}
	return ctx
}

func policyDenyJSON(dec policy.PolicyDecision) map[string]string {
	msg := dec.Reason
	if msg == "" {
		msg = "denied by policy"
	}
	out := map[string]string{"message": msg}
	if dec.ReasonCode != "" {
		out["reason_code"] = dec.ReasonCode
	}
	return out
}

func mcpPolicyDenyData(dec policy.PolicyDecision, actionID string) map[string]interface{} {
	data := map[string]interface{}{}
	if dec.ReasonCode != "" {
		data["reason_code"] = dec.ReasonCode
	}
	if dec.RuleID != "" {
		data["rule_id"] = dec.RuleID
	}
	if actionID != "" {
		data["action_id"] = actionID
	}
	if dec.Effect != "" {
		data["effect"] = dec.Effect
	}
	return data
}

// emitToolAuthEvent publishes a best-effort RunEvent so Agent Protocol
// streams surface connector policy denials / pending HITL. Event IDs use
// a distinct namespace from runner `{run_id}_evt_{seq}` so they never
// collide; seq is max(replay)+1 (best-effort under concurrent runner publishes).
func (s *Server) emitToolAuthEvent(ctx context.Context, in policy.PolicyInput, dec policy.PolicyDecision, actionID string) {
	if s == nil || !s.policyRunEvents || s.broker == nil {
		return
	}
	if in.RunID == "" {
		return
	}
	payloadMap := map[string]interface{}{
		"stage":       in.Stage,
		"effect":      dec.Effect,
		"connector":   in.Connector,
		"tool":        in.Tool,
		"reason":      dec.Reason,
		"reason_code": dec.ReasonCode,
		"rule_id":     dec.RuleID,
		"generation":  in.Generation,
	}
	if actionID != "" {
		payloadMap["action_id"] = actionID
	}
	if in.ConnectorActionID != "" {
		payloadMap["connector_action_id"] = in.ConnectorActionID
	}
	if in.ArgsDigest != "" {
		payloadMap["args_digest"] = in.ArgsDigest
	}
	payload, err := json.Marshal(payloadMap)
	if err != nil {
		slog.Warn("policy: tool_auth marshal failed", "error", err, "run_id", in.RunID)
		return
	}
	seq := int64(1)
	if prev, replayErr := s.broker.Replay(ctx, in.RunID, 0); replayErr == nil {
		for _, ev := range prev {
			if ev != nil && ev.Seq >= seq {
				seq = ev.Seq + 1
			}
		}
	}
	idSuffix, err := randomHex(8)
	if err != nil {
		slog.Warn("policy: tool_auth id failed", "error", err, "run_id", in.RunID)
		return
	}
	ev := &transport.RunEvent{
		Generation: in.Generation,
		EventID:    in.RunID + "_tool_auth_" + idSuffix,
		Seq:        seq,
		Method:     "tool_auth",
		Namespace:  []string{},
		Data:       payload,
		Ts:         time.Now().UnixMilli(),
	}
	if err := s.broker.Publish(ctx, in.RunID, ev); err != nil {
		slog.Warn("policy: tool_auth publish failed", "error", err, "run_id", in.RunID)
	}
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tryConsumePendingCapability burns one approved pending_action for this
// tools/call when the argument digest matches. Returns true when the call
// should proceed without Decide.
func (s *Server) tryConsumePendingCapability(ctx context.Context, connectorName, tool, argsDigest, targetRevision string) bool {
	if !models.ValidConnectorTargetRevision(targetRevision) {
		return false
	}
	store, ok := s.pendingActions()
	if !ok {
		return false
	}
	in := policyInputFromRequest(ctx, policy.StageToolCall, connectorName, tool)
	if in.RunID == "" {
		return false
	}
	id, err := store.ConsumeApprovedAction(ctx, in.TenantID, in.AgentID, in.RunID, in.Generation, connectorName, tool, argsDigest, targetRevision)
	if err != nil {
		slog.Warn("policy: consume approved action failed", "error", err, "run_id", in.RunID)
		return false
	}
	return id != ""
}

// persistPendingAction stores (or reuses) a pending HITL row for this
// exact argument digest. Returns action id.
func (s *Server) persistPendingAction(ctx context.Context, in policy.PolicyInput, connectorName, tool string, dec policy.PolicyDecision, expiresAt *time.Time) (string, error) {
	store, ok := s.pendingActions()
	if !ok {
		return "", errString("pending actions require a SQL state backend (Postgres, MySQL, or SQLite)")
	}
	if in.RunID == "" {
		return "", errString("pending actions require run binding")
	}
	digest := in.ArgsDigest
	if digest == "" {
		_, digest, _ = policy.BindArgs(nil)
	}
	suffix, err := randomHex(8)
	if err != nil {
		return "", err
	}
	a := &models.PendingAction{
		ID:         pendingActionIDPrefix + suffix,
		RunID:      in.RunID,
		Generation: in.Generation,
		TenantID:   in.TenantID,
		AgentID:    in.AgentID,
		Connector:  connectorName,
		Tool:       tool,
		RuleID:     dec.RuleID,
		Reason:     dec.Reason,
		ReasonCode: dec.ReasonCode,
		Status:     models.PendingStatusPending,
		ArgsDigest: digest,
		Args:       in.ArgsMeta,
		ExpiresAt:  expiresAt,
	}
	if a.ReasonCode == "" {
		a.ReasonCode = policy.ReasonPolicyPending
	}
	if expiresAt == nil {
		if !models.ValidConnectorTargetRevision(in.TargetRevision) {
			return "", errString("managed pending action requires a verified connector target")
		}
		a.TargetRevision = in.TargetRevision
		if binding := auth.RunBindingFromContext(ctx); binding != nil {
			run, err := s.store.GetRun(ctx, binding.RunID)
			if err != nil {
				return "", err
			}
			if run.TenantID != binding.TenantID || run.AgentID != binding.AgentID ||
				run.ThreadID != binding.ThreadID || in.RunID != binding.RunID ||
				in.TenantID != binding.TenantID || in.AgentID != binding.AgentID ||
				in.Generation != binding.Generation || run.IncarnationID == "" {
				return "", errString("pending action run identity does not match its binding")
			}
			if err := state.CheckRunCheckpointBinding(run, state.RunCheckpointBinding{
				RunID: binding.RunID, Generation: binding.Generation, ThreadID: binding.ThreadID,
			}); err != nil {
				return "", err
			}
			a.RunIncarnationID = run.IncarnationID
		}
	}
	if err := store.CreatePendingAction(ctx, a); err != nil {
		return "", err
	}
	return a.ID, nil
}
