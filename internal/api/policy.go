package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

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

// checkConnectorPolicy runs the policy engine when enabled. Returns
// (decision, true) when the caller should deny; (zero, false) to proceed.
func (s *Server) checkConnectorPolicy(ctx context.Context, stage, connectorName, tool string) (policy.PolicyDecision, bool) {
	if !s.policy.Enabled() {
		return policy.PolicyDecision{}, false
	}
	in := policyInputFromRequest(ctx, stage, connectorName, tool)
	// Pre-warm / createRun may not have AgentID on binding; callers that
	// know the agent should set it on context via withPolicyAgent.
	if v, ok := ctx.Value(policyAgentKey{}).(string); ok && in.AgentID == "" {
		in.AgentID = v
	}
	if v, ok := ctx.Value(policyRunKey{}).(string); ok && in.RunID == "" {
		in.RunID = v
	}
	dec := s.policy.Decide(ctx, in)
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

func mcpPolicyDenyData(dec policy.PolicyDecision) map[string]interface{} {
	data := map[string]interface{}{}
	if dec.ReasonCode != "" {
		data["reason_code"] = dec.ReasonCode
	}
	if dec.RuleID != "" {
		data["rule_id"] = dec.RuleID
	}
	return data
}

// emitToolAuthEvent publishes a best-effort RunEvent so Agent Protocol
// streams surface connector policy denials. Event IDs use a distinct
// namespace from runner `{run_id}_evt_{seq}` so they never collide; seq
// is max(replay)+1 (best-effort under concurrent runner publishes).
func (s *Server) emitToolAuthEvent(ctx context.Context, stage, connectorName, tool string, dec policy.PolicyDecision) {
	if s == nil || !s.policyRunEvents || s.broker == nil {
		return
	}
	in := policyInputFromRequest(ctx, stage, connectorName, tool)
	if in.RunID == "" {
		return
	}
	payload, err := json.Marshal(map[string]interface{}{
		"stage":       stage,
		"effect":      dec.Effect,
		"connector":   connectorName,
		"tool":        tool,
		"reason":      dec.Reason,
		"reason_code": dec.ReasonCode,
		"rule_id":     dec.RuleID,
		"generation":  in.Generation,
	})
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
		EventID:   in.RunID + "_tool_auth_" + idSuffix,
		Seq:       seq,
		Method:    "tool_auth",
		Namespace: []string{},
		Data:      payload,
		Ts:        time.Now().UnixMilli(),
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

// extractJSONRPCID pulls the JSON-RPC id from a raw body for deny responses.
func extractJSONRPCID(body []byte) json.RawMessage {
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &envelope)
	return envelope.ID
}

// extractToolsCallName returns params.name for a tools/call body, or "".
func extractToolsCallName(body []byte) (method, tool string) {
	var envelope struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return "", ""
	}
	if envelope.Method != "tools/call" {
		return envelope.Method, ""
	}
	var params struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(envelope.Params, &params)
	return envelope.Method, params.Name
}
