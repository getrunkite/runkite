package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
)

// cancelInflightForBudget drains pending/running runs in scope when a
// hard day cap is already breached and finops.on_hard_breach is
// cancel_inflight. Reuses drainKillScope (same cancel path as the
// kill-switch drain). agentID empty = whole tenant; otherwise that agent.
// Safe to call asynchronously; cancelRunSingle is idempotent on terminal.
func (s *Server) cancelInflightForBudget(ctx context.Context, tenantID, agentID, trigger string) {
	if s == nil || s.finops == nil || !s.finops.CancelInflightOnHardBreach() {
		return
	}
	if tenantID == "" {
		return
	}
	n := s.drainKillScope(ctx, tenantID, agentID)
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     tenantID,
		Action:       policy.StageRunCreate,
		ResourceType: "agent",
		ResourceID:   agentID,
		Decision:     policy.EffectDeny,
		ReasonCode:   policy.ReasonBudgetKill,
		AgentID:      agentID,
		Attrs: map[string]interface{}{
			"trigger":   trigger,
			"cancelled": n,
			"scope":     budgetKillScope(agentID),
		},
	})
	slog.Info("finops cancelled inflight on hard budget breach",
		"tenant_id", tenantID, "agent_id", agentID, "trigger", trigger, "cancelled", n)
}

func budgetKillScope(agentID string) string {
	if agentID == "" {
		return "tenant"
	}
	return "agent"
}

// maybeCancelInflightAfterTerminal re-evaluates day caps after a run's
// usage is ingested and its hold released. If a hard cap is breached,
// drains remaining inflight in that scope. No-op unless cancel_inflight.
func (s *Server) maybeCancelInflightAfterTerminal(ctx context.Context, run *models.Run) {
	if s == nil || run == nil || s.finops == nil || !s.finops.CancelInflightOnHardBreach() {
		return
	}
	if !s.finops.Enabled() {
		return
	}
	store, ok := s.usageEvents()
	if !ok {
		return
	}
	tenantID := run.TenantID
	agentID := run.AgentID
	tenantCap, agentCap := s.finops.LookupCaps(tenantID, agentID)
	if tenantCap == nil && agentCap == nil {
		return
	}
	since, until := finops.UTCDayWindow(time.Now().UTC())
	load := func(agent string) (finops.UsageSnapshot, error) {
		tin, tout, usd, err := store.SumUsage(ctx, tenantID, agent, since, until)
		if err != nil {
			return finops.UsageSnapshot{}, err
		}
		runs, err := store.CountRunsSince(ctx, tenantID, agent, since)
		if err != nil {
			return finops.UsageSnapshot{}, err
		}
		snap := finops.UsageSnapshot{TokensIn: tin, TokensOut: tout, USD: usd, Runs: runs}
		if hs, ok := s.usageHolds(); ok {
			hu, ht, _, herr := hs.SumUsageHolds(ctx, tenantID, agent, since, until)
			if herr == nil {
				snap.USD += hu
				snap.TokensIn += ht
			}
		}
		return snap, nil
	}
	// Prefer agent-scoped drain when the agent cap is the one that tipped;
	// otherwise drain the whole tenant.
	drainAgent := ""
	hard := false
	for _, pair := range []struct {
		cap   *finops.BudgetCap
		agent string
		scope string
	}{
		{agentCap, agentID, "agent"},
		{tenantCap, "", "tenant"},
	} {
		if pair.cap == nil {
			continue
		}
		snap, err := load(pair.agent)
		if err != nil {
			continue
		}
		v := finops.EvaluateCap(pair.cap, snap, pair.scope)
		if v.Hard {
			hard = true
			if pair.scope == "agent" {
				drainAgent = agentID
			}
			break
		}
	}
	if !hard {
		return
	}
	// Detach from request lifetime — cancel is best-effort cleanup.
	go s.cancelInflightForBudget(context.WithoutCancel(ctx), tenantID, drainAgent, "terminal_ingest")
}
