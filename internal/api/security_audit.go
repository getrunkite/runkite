package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
)

// writeSecurityAudit best-effort persists a non-policy security decision
// (admission authz/kill deny, kill activate/clear, budget soft/hard).
// SQL stores only; missing auditor is a no-op (Mongo).
func (s *Server) writeSecurityAudit(ctx context.Context, ev *models.AuditEvent) {
	if s == nil || ev == nil {
		return
	}
	aw, ok := s.auditWriter()
	if !ok {
		return
	}
	if ev.ID == "" {
		suffix, err := randomHex(8)
		if err != nil {
			slog.Warn("security audit id mint failed", "action", ev.Action, "error", err)
			return
		}
		ev.ID = "sec-" + suffix
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	if err := aw.WriteAuditEvent(ctx, ev); err != nil {
		slog.Warn("security audit write failed",
			"action", ev.Action, "reason_code", ev.ReasonCode, "error", err)
	}
}

func (s *Server) writeAdmissionDenyAudit(ctx context.Context, tenantID, agentID, runID, reasonCode, reason string) {
	actor := ""
	if ar := auth.FromContext(ctx); ar != nil {
		actor = ar.Identity
	}
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     tenantID,
		Actor:        actor,
		Action:       policy.StageRunCreate,
		ResourceType: "agent",
		ResourceID:   agentID,
		Decision:     policy.EffectDeny,
		ReasonCode:   reasonCode,
		AgentID:      agentID,
		RunID:        runID,
		Attrs: map[string]interface{}{
			"reason": reason,
		},
	})
}
