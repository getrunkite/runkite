package api

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
	"github.com/getrunkite/runkite/internal/tenant"
)

// tryBreakGlassBypass returns true when an active window covers this
// policy decision and a durable audit row was written. Fail-closed on
// lookup or audit errors (caller must run normal Decide). Never bypasses
// kill, authz, or admission_limits — only policy Decide.
func (s *Server) tryBreakGlassBypass(ctx context.Context, in policy.PolicyInput) bool {
	if s == nil || !s.policy.Enabled() {
		return false
	}
	store, ok := s.breakGlass()
	if !ok {
		return false
	}
	tenantID := in.TenantID
	if tenantID == "" {
		tenantID = tenant.FromContext(ctx)
	}
	win, err := store.FindActiveBreakGlass(ctx, tenantID, in.AgentID)
	if err != nil {
		slog.Warn("break-glass lookup failed", "error", err)
		return false
	}
	if win == nil {
		return false
	}
	aw, ok := s.auditWriter()
	if !ok {
		// SQL stores that have break-glass also have audit; if not, refuse
		// silent bypass.
		slog.Warn("break-glass active but audit store unavailable; refusing bypass",
			"window_id", win.ID)
		return false
	}
	actor := in.Principal
	if actor == "" {
		if ar := auth.FromContext(ctx); ar != nil {
			actor = ar.Identity
		}
	}
	resourceType := "connector"
	if in.Stage == policy.StageRunCreate {
		resourceType = "run"
	}
	suffix, err := randomHex(8)
	if err != nil {
		slog.Warn("break-glass audit id mint failed; refusing bypass",
			"window_id", win.ID, "error", err)
		return false
	}
	attrs := map[string]interface{}{
		"reason":     win.Reason,
		"window_id":  win.ID,
		"created_by": win.CreatedBy,
		"expires_at": win.ExpiresAt.UTC().Format(time.RFC3339),
	}
	// Observability only: note when this bypass also skipped a
	// mandatory_hitl gate (no behavior change — break-glass still wins).
	if in.Stage == policy.StageToolCall {
		if rule := s.policy.MatchMandatoryHITL(in); rule != nil {
			attrs["mandatory_hitl_bypassed"] = true
			if id := strings.TrimSpace(rule.ID); id != "" {
				attrs["mandatory_hitl_rule_id"] = id
			}
		}
	}
	ev := &models.AuditEvent{
		ID:           "bg-use-" + suffix,
		TS:           time.Now().UTC(),
		TenantID:     tenantID,
		Actor:        actor,
		Action:       in.Stage,
		ResourceType: resourceType,
		ResourceID:   in.Connector,
		Decision:     policy.EffectAllow,
		ReasonCode:   policy.ReasonBreakGlass,
		RuleID:       win.ID,
		RunID:        in.RunID,
		Generation:   in.Generation,
		AgentID:      in.AgentID,
		Connector:    in.Connector,
		Tool:         in.Tool,
		Attrs:        attrs,
	}
	if err := aw.WriteAuditEvent(ctx, ev); err != nil {
		slog.Warn("break-glass audit write failed; refusing bypass",
			"window_id", win.ID, "error", err)
		return false
	}
	return true
}
