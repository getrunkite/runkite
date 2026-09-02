package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/policy"
)

// usageStore is the SQL surface for usage_events + budget Counters.
type usageStore interface {
	WriteUsageEvent(ctx context.Context, ev *models.UsageEvent) error
	SumUsage(ctx context.Context, tenantID, agentID string, since, until time.Time) (tokensIn, tokensOut int64, usd float64, err error)
	CountRunsSince(ctx context.Context, tenantID, agentID string, since time.Time) (int64, error)
	SearchUsageSummary(ctx context.Context, req *models.UsageSummaryRequest) ([]models.UsageSummaryRow, error)
}

func (s *Server) usageEvents() (usageStore, bool) {
	us, ok := s.store.(usageStore)
	return us, ok
}

// checkBudgetAdmission evaluates tenant + agent UTC-day caps. Hard deny
// wins over soft. Break-glass must not bypass this (caller invokes after
// policy / break-glass). Nil finops or empty budgets → allow.
func (s *Server) checkBudgetAdmission(ctx context.Context, tenantID, agentID, runID string) (allow bool, reason string) {
	if s == nil || s.finops == nil || !s.finops.Enabled() {
		return true, ""
	}
	store, ok := s.usageEvents()
	if !ok {
		// No durable usage store (Mongo): fail open on budgets so
		// admission still works; Admin usage routes return 501.
		return true, ""
	}
	tenantCap, agentCap := s.finops.LookupCaps(tenantID, agentID)
	if tenantCap == nil && agentCap == nil {
		return true, ""
	}

	now := time.Now().UTC()
	since, until := finops.UTCDayWindow(now)

	loadSnap := func(agent string) (finops.UsageSnapshot, error) {
		tin, tout, usd, err := store.SumUsage(ctx, tenantID, agent, since, until)
		if err != nil {
			return finops.UsageSnapshot{}, err
		}
		runs, err := store.CountRunsSince(ctx, tenantID, agent, since)
		if err != nil {
			return finops.UsageSnapshot{}, err
		}
		return finops.UsageSnapshot{TokensIn: tin, TokensOut: tout, USD: usd, Runs: runs}, nil
	}

	var soft *finops.BudgetVerdict
	for _, pair := range []struct {
		cap   *finops.BudgetCap
		agent string
		scope string
	}{
		{tenantCap, "", "tenant"},
		{agentCap, agentID, "agent"},
	} {
		if pair.cap == nil {
			continue
		}
		snap, err := loadSnap(pair.agent)
		if err != nil {
			slog.Warn("budget admission lookup failed", "tenant_id", tenantID, "agent_id", agentID, "error", err)
			return false, "budget lookup failed: " + err.Error()
		}
		v := finops.EvaluateCap(pair.cap, snap, pair.scope)
		if v.Hard {
			s.writeAdmissionDenyAudit(ctx, tenantID, agentID, runID, policy.ReasonBudgetExceeded, v.Reason)
			return false, v.Reason
		}
		if v.Soft && soft == nil {
			cp := v
			soft = &cp
		}
	}
	if soft != nil {
		s.writeSecurityAudit(ctx, &models.AuditEvent{
			TenantID:     tenantID,
			Action:       policy.StageRunCreate,
			ResourceType: "agent",
			ResourceID:   agentID,
			Decision:     policy.EffectAllow,
			ReasonCode:   policy.ReasonBudgetSoft,
			AgentID:      agentID,
			RunID:        runID,
			Attrs: map[string]interface{}{
				"reason": soft.Reason,
				"scope":  soft.Scope,
				"kind":   soft.CapKind,
			},
		})
	}
	return true, ""
}

// ingestTerminalUsage writes a usage_events row from run Output when
// present. Best-effort; no-ops on Mongo or empty/zero usage.
func (s *Server) ingestTerminalUsage(ctx context.Context, run *models.Run) {
	if s == nil || run == nil || len(run.Output) == 0 {
		return
	}
	if !isTerminalStatus(run.Status) {
		return
	}
	store, ok := s.usageEvents()
	if !ok {
		return
	}
	usage, model := extractRunUsageWithModel(run.Output)
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.CostUSD == 0 {
		return
	}
	var pb finops.Pricebook
	if s.finops != nil {
		pb = s.finops.Pricebook
	}
	usd := pb.EstimateUSD(model, usage.PromptTokens, usage.CompletionTokens, usage.CostUSD)
	id := "usage-" + run.RunID
	ev := &models.UsageEvent{
		ID:          id,
		TS:          time.Now().UTC(),
		TenantID:    run.TenantID,
		RunID:       run.RunID,
		AgentID:     run.AgentID,
		Model:       model,
		TokensIn:    usage.PromptTokens,
		TokensOut:   usage.CompletionTokens,
		USDEstimate: usd,
		Source:      models.UsageSourceTerminalOutput,
	}
	if err := store.WriteUsageEvent(ctx, ev); err != nil {
		slog.Warn("usage event write failed", "run_id", run.RunID, "error", err)
	}
}

// extractRunUsageWithModel is extractRunUsage plus optional usage.model.
func extractRunUsageWithModel(output json.RawMessage) (RunUsage, string) {
	if len(output) == 0 {
		return RunUsage{}, ""
	}
	var parsed struct {
		Usage struct {
			RunUsage
			Model string `json:"model"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(output, &parsed); err != nil {
		return RunUsage{}, ""
	}
	u := parsed.Usage.RunUsage
	if u.TotalTokens == 0 && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u, parsed.Usage.Model
}
