package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/finops"
	"github.com/getrunkite/runkite/internal/hooks"
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

// usageHoldStore is optional; SQL backends that migrated usage_holds implement it.
type usageHoldStore interface {
	UpsertUsageHold(ctx context.Context, h *models.UsageHold) error
	ReleaseUsageHold(ctx context.Context, runID string) error
	SumUsageHolds(ctx context.Context, tenantID, agentID string, since, until time.Time) (usd float64, tokens int64, count int64, err error)
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
		snap := finops.UsageSnapshot{TokensIn: tin, TokensOut: tout, USD: usd, Runs: runs}
		if hs, ok := s.store.(usageHoldStore); ok {
			holdUSD, holdTok, _, herr := hs.SumUsageHolds(ctx, tenantID, agent, since, until)
			if herr != nil {
				return finops.UsageSnapshot{}, herr
			}
			snap.USD += holdUSD
			snap.TokensIn += holdTok // reservation is undifferentiated tokens
			// Do not add hold count to Runs: CountRunsSince already includes
			// in-flight creates, so counting open holds again would ~2× the
			// run dimension whenever reservation is paired with max_runs_per_day.
		}
		return snap, nil
	}

	var soft *finops.BudgetVerdict
	var approach *finops.BudgetVerdict
	softPct := s.finops.SoftPct()
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
			s.emitBudgetAlert(ctx, tenantID, agentID, runID, policy.ReasonBudgetExceeded, &v, true)
			if s.finops.CancelInflightOnHardBreach() {
				drainAgent := ""
				if pair.scope == "agent" {
					drainAgent = agentID
				}
				// Async so admission latency stays low; drain reuses kill-switch cancel.
				go s.cancelInflightForBudget(context.WithoutCancel(ctx), tenantID, drainAgent, "admission")
			}
			return false, v.Reason
		}
		if v.Soft && soft == nil {
			cp := v
			soft = &cp
		}
		if soft == nil && approach == nil {
			if a := finops.ApproachCap(pair.cap, snap, pair.scope, softPct); a != nil {
				approach = a
			}
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
		s.emitBudgetAlert(ctx, tenantID, agentID, runID, policy.ReasonBudgetSoft, soft, false)
	} else if approach != nil {
		// Approach alerts fire on every create while spend sits in the
		// soft_pct..hard band; without dedupe that spams webhooks/Admin.
		// Once per tenant/agent/scope/kind/UTC-day in this process is enough
		// signal (soft/hard still emit every time). Multi-replica may still
		// duplicate — intentional best-effort, not a cluster lock.
		if s.shouldEmitApproachAlert(tenantID, agentID, approach.Scope, approach.CapKind, time.Now().UTC()) {
			s.writeSecurityAudit(ctx, &models.AuditEvent{
				TenantID:     tenantID,
				Action:       policy.StageRunCreate,
				ResourceType: "agent",
				ResourceID:   agentID,
				Decision:     policy.EffectAllow,
				ReasonCode:   policy.ReasonBudgetAlert,
				AgentID:      agentID,
				RunID:        runID,
				Attrs: map[string]interface{}{
					"reason":   approach.Reason,
					"scope":    approach.Scope,
					"kind":     approach.CapKind,
					"soft_pct": softPct,
				},
			})
			s.emitBudgetAlert(ctx, tenantID, agentID, runID, policy.ReasonBudgetAlert, approach, false)
		}
	}
	return true, ""
}

// emitBudgetAlert dispatches hooks.BudgetAlert and is nil-safe when no sinks.
func (s *Server) emitBudgetAlert(ctx context.Context, tenantID, agentID, runID, reasonCode string, v *finops.BudgetVerdict, hard bool) {
	if s == nil || v == nil || s.hooks == nil || !s.hooks.HasSinks() {
		return
	}
	severity := "soft"
	if hard {
		severity = "hard"
	} else if reasonCode == policy.ReasonBudgetAlert {
		severity = "approach"
	}
	s.hooks.Dispatch(hooks.Event{
		Type:      hooks.BudgetAlert,
		RunID:     runID,
		AgentID:   agentID,
		TenantID:  tenantID,
		Timestamp: time.Now().UTC(),
		Data: map[string]interface{}{
			"reason_code": reasonCode,
			"reason":      v.Reason,
			"scope":       v.Scope,
			"kind":        v.CapKind,
			"severity":    severity,
		},
	})
}

// shouldEmitApproachAlert is true the first time this process sees an
// approach key for the UTC day. Soft/hard callers must not use this.
func (s *Server) shouldEmitApproachAlert(tenantID, agentID, scope, kind string, now time.Time) bool {
	if s == nil {
		return true
	}
	day := now.UTC().Format("2006-01-02")
	key := tenantID + "|" + agentID + "|" + scope + "|" + kind + "|" + day
	_, loaded := s.approachAlertSeen.LoadOrStore(key, struct{}{})
	return !loaded
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
	if usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.CostUSD == 0 && !usage.Unmetered {
		return
	}
	var pb finops.Pricebook
	if s.finops != nil {
		pb = s.finops.Pricebook
	}
	usd := pb.EstimateUSD(model, usage.PromptTokens, usage.CompletionTokens, usage.CostUSD)
	if usage.CostUSD == 0 && (usage.PromptTokens > 0 || usage.CompletionTokens > 0) &&
		len(pb) > 0 && !pb.HasModel(model) {
		// Pricebook is configured but this model id is missing — the
		// "admin swapped models, forgot to update the pricebook" case.
		// Empty pricebook stays quiet (tokens-only metering). A present
		// row with $0 rates is intentional free tier, not unpriced.
		s.writeSecurityAudit(ctx, &models.AuditEvent{
			TenantID:     run.TenantID,
			Action:       policy.StageRunCreate,
			ResourceType: "agent",
			ResourceID:   run.AgentID,
			Decision:     policy.EffectAllow,
			ReasonCode:   policy.ReasonUsageUnpriced,
			AgentID:      run.AgentID,
			RunID:        run.RunID,
			Attrs: map[string]interface{}{
				"model":             model,
				"prompt_tokens":     usage.PromptTokens,
				"completion_tokens": usage.CompletionTokens,
			},
		})
	}
	if usage.Unmetered {
		// The runner found an AI-shaped reply (a real model turn ran) but
		// could not extract any token/cost data from it in any recognized
		// shape -- most likely a provider/framework integration this
		// codebase has never seen (a brand-new model, a community
		// integration that has not adopted LangChain's standardized
		// usage_metadata fields, or a custom adapter that has not wired
		// FinOps up at all). Silence here would look identical to "this
		// agent made no LLM call and owes nothing", which is the one
		// failure mode worse than an under-priced run: an entirely
		// invisible one.
		s.writeSecurityAudit(ctx, &models.AuditEvent{
			TenantID:     run.TenantID,
			Action:       policy.StageRunCreate,
			ResourceType: "agent",
			ResourceID:   run.AgentID,
			Decision:     policy.EffectAllow,
			ReasonCode:   policy.ReasonUsageUnmetered,
			AgentID:      run.AgentID,
			RunID:        run.RunID,
			Attrs: map[string]interface{}{
				"model": model,
			},
		})
	}
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

func (s *Server) usageHolds() (usageHoldStore, bool) {
	hs, ok := s.store.(usageHoldStore)
	return hs, ok
}

func (s *Server) placeUsageHold(ctx context.Context, run *models.Run) {
	if s == nil || run == nil || s.finops == nil || !s.finops.ReservationEnabled() {
		return
	}
	hs, ok := s.usageHolds()
	if !ok {
		return
	}
	usd, tokens := s.finops.ReservationFor(run.TenantID, run.AgentID)
	if usd <= 0 && tokens <= 0 {
		return
	}
	h := &models.UsageHold{
		RunID:      run.RunID,
		TenantID:   run.TenantID,
		AgentID:    run.AgentID,
		USDHold:    usd,
		TokensHold: tokens,
		CreatedAt:  time.Now().UTC(),
	}
	if err := hs.UpsertUsageHold(ctx, h); err != nil {
		slog.Warn("usage hold upsert failed", "run_id", run.RunID, "error", err)
	}
}

func (s *Server) releaseUsageHold(ctx context.Context, runID string) {
	hs, ok := s.usageHolds()
	if !ok {
		return
	}
	if err := hs.ReleaseUsageHold(ctx, runID); err != nil {
		slog.Warn("usage hold release failed", "run_id", runID, "error", err)
	}
}

// applyFinOpsRouting rewrites agentID to a cheaper configured target when
// soft_pct of a hard day cap is exceeded. Returns (agentID, routedFrom).
// aliasKey is the pre-resolve client agent_id (may equal agentID). Map
// lookup tries aliasKey first so operators can key prefer_cheaper maps
// by the public alias name.
func (s *Server) applyFinOpsRouting(ctx context.Context, tenantID, agentID, aliasKey string) (string, string) {
	if s == nil || s.finops == nil || !s.finops.Routing.Enabled || len(s.finops.Routing.Aliases) == 0 {
		return agentID, ""
	}
	var targets []string
	for _, key := range []string{aliasKey, agentID} {
		if key == "" {
			continue
		}
		if t := s.finops.Routing.Aliases[key]; len(t) > 0 {
			targets = t
			break
		}
	}
	if len(targets) == 0 {
		return agentID, ""
	}
	store, ok := s.usageEvents()
	if !ok {
		return agentID, ""
	}
	since, until := finops.UTCDayWindow(time.Now().UTC())
	tenantCap, agentCap := s.finops.LookupCaps(tenantID, agentID)
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
				// Runs already counted via CountRunsSince; see admission loadSnap.
			}
		}
		return snap, nil
	}
	near := false
	pct := s.finops.RoutingSoftPct()
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
		snap, err := load(pair.agent)
		if err != nil {
			continue
		}
		if finops.ApproachCap(pair.cap, snap, pair.scope, pct) != nil {
			near = true
			break
		}
		// also treat soft-tripped as near
		v := finops.EvaluateCap(pair.cap, snap, pair.scope)
		if v.Soft || v.Hard {
			near = true
			break
		}
	}
	if !near {
		return agentID, ""
	}
	from := agentID
	to := targets[0]
	s.writeSecurityAudit(ctx, &models.AuditEvent{
		TenantID:     tenantID,
		Action:       policy.StageRunCreate,
		ResourceType: "agent",
		ResourceID:   to,
		Decision:     policy.EffectAllow,
		ReasonCode:   "budget_route",
		AgentID:      to,
		Attrs: map[string]interface{}{
			"from_agent_id": from,
			"to_agent_id":   to,
		},
	})
	return to, from
}
