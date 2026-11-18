package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

func runFinalizationIdentityTests(t *testing.T, factory StoreFactory) {
	seed := func(t *testing.T) (state.Store, context.Context, *models.Run) {
		s := factory(t)
		ctx := tenant.ScopedContext(t.Context(), "finalization")
		now := time.Now().UTC()
		if err := s.CreateThread(ctx, &models.Thread{ThreadID: "thread", Status: models.ThreadStatusBusy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		run := &models.Run{RunID: "run", ThreadID: "thread", AgentID: "agent", Status: models.RunStatusPending, CreatedAt: now, UpdatedAt: now}
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		return s, ctx, run
	}
	t.Run("accounting_and_hold_bounds_remain_half_open", func(t *testing.T) {
		s, ctx, run := seed(t)
		events, ok := s.(state.RunUsageEventStore)
		if !ok {
			t.Skip("store has no SQL usage capability")
		}
		holds := s.(state.RunUsageHoldStore)
		boundary := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
		identity := state.RunIdentity(run)
		if err := events.WriteRunUsageEvent(ctx, identity, &models.UsageEvent{ID: "boundary", RunID: run.RunID, TenantID: "finalization", TokensIn: 5, TS: boundary}); err != nil {
			t.Fatal(err)
		}
		if err := holds.UpsertRunUsageHold(ctx, identity, &models.UsageHold{RunID: run.RunID, TenantID: "finalization", TokensHold: 7, CreatedAt: boundary}); err != nil {
			t.Fatal(err)
		}
		sum := s.(interface {
			SumUsage(context.Context, string, string, time.Time, time.Time) (int64, int64, float64, error)
		})
		held := s.(interface {
			SumUsageHolds(context.Context, string, string, time.Time, time.Time) (float64, int64, int64, error)
		})
		if tokens, _, _, err := sum.SumUsage(ctx, "finalization", "", boundary.Add(-time.Hour), boundary); err != nil || tokens != 0 {
			t.Fatal("exclusive usage bound counted following day", tokens, err)
		}
		if _, tokens, count, err := held.SumUsageHolds(ctx, "finalization", "", boundary.Add(-time.Hour), boundary); err != nil || tokens != 0 || count != 0 {
			t.Fatal("exclusive hold bound counted following day", tokens, count, err)
		}
	})
	t.Run("late_reservation_never_recreates_terminal_hold", func(t *testing.T) {
		s, ctx, run := seed(t)
		holds, ok := s.(state.RunUsageHoldStore)
		if !ok {
			t.Skip("store has no SQL hold capability")
		}
		identity := state.RunIdentity(run)
		hold := &models.UsageHold{RunID: run.RunID, TenantID: "finalization", AgentID: "agent", TokensHold: 3}
		if err := holds.UpsertRunUsageHold(ctx, identity, hold); err != nil {
			t.Fatal(err)
		}
		if accepted, err := s.BeginRunAttempt(ctx, run.RunID, identity, 1); err != nil || !accepted {
			t.Fatal(accepted, err)
		}
		if result, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: run.RunID, ExpectedIncarnationID: identity, Generation: 1, Status: models.RunStatusSuccess}); err != nil || !result.Applied {
			t.Fatal(result, err)
		}
		if err := holds.ReleaseRunUsageHold(ctx, run.RunID, identity); err != nil {
			t.Fatal(err)
		}
		if err := holds.UpsertRunUsageHold(ctx, identity, hold); err != nil {
			t.Fatal("late placement on terminal record should be harmless", err)
		}
		summary := s.(interface {
			SumUsageHolds(context.Context, string, string, time.Time, time.Time) (float64, int64, int64, error)
		})
		if _, tokens, count, err := summary.SumUsageHolds(ctx, "finalization", "agent", time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); err != nil || tokens != 0 || count != 0 {
			t.Fatal("late admission recreated a terminal hold", tokens, count, err)
		}
	})
	t.Run("independent_accounting_and_holds_for_reused_id", func(t *testing.T) {
		s, ctx, original := seed(t)
		events, ok := s.(state.RunUsageEventStore)
		if !ok {
			t.Skip("store has no SQL usage capability")
		}
		holds := s.(state.RunUsageHoldStore)
		sum := s.(interface {
			SumUsage(context.Context, string, string, time.Time, time.Time) (int64, int64, float64, error)
		})
		held := s.(interface {
			SumUsageHolds(context.Context, string, string, time.Time, time.Time) (float64, int64, int64, error)
		})
		before, after := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
		oldID := state.RunIdentity(original)
		usage := &models.UsageEvent{ID: "usage-original", RunID: "run", TenantID: "finalization", AgentID: "agent", TokensIn: 5, Source: models.UsageSourceTerminalOutput}
		for range 2 {
			if err := events.WriteRunUsageEvent(ctx, oldID, usage); err != nil {
				t.Fatal(err)
			}
		}
		if err := holds.UpsertRunUsageHold(ctx, oldID, &models.UsageHold{RunID: "run", TenantID: "finalization", AgentID: "agent", TokensHold: 3}); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteRun(ctx, "run"); err != nil {
			t.Fatal(err)
		}
		replacement := *original
		if err := s.CreateRun(ctx, &replacement); err != nil {
			t.Fatal(err)
		}
		newID := state.RunIdentity(&replacement)
		if newID == "" || newID == oldID {
			t.Fatal("replacement reused physical identity")
		}
		next := *usage
		next.ID = "usage-replacement"
		next.TokensIn = 7
		if err := events.WriteRunUsageEvent(ctx, newID, &next); err != nil {
			t.Fatal(err)
		}
		if err := holds.UpsertRunUsageHold(ctx, newID, &models.UsageHold{RunID: "run", TenantID: "finalization", AgentID: "agent", TokensHold: 17}); err != nil {
			t.Fatal(err)
		}
		if err := events.WriteRunUsageEvent(ctx, oldID, usage); err == nil {
			t.Fatal("old accounting rewrote replacement")
		}
		if err := holds.UpsertRunUsageHold(ctx, oldID, &models.UsageHold{RunID: "run", TokensHold: 99}); err == nil {
			t.Fatal("old hold upsert rewrote replacement")
		}
		if err := holds.ReleaseRunUsageHold(ctx, "run", oldID); err == nil {
			t.Fatal("old finalization released replacement")
		}
		if in, _, _, err := sum.SumUsage(ctx, "finalization", "agent", before, after); err != nil || in != 12 {
			t.Fatal("distinct accounting lost", in, err)
		}
		summary := s.(interface {
			SearchUsageSummary(context.Context, *models.UsageSummaryRequest) ([]models.UsageSummaryRow, error)
		})
		rows, err := summary.SearchUsageSummary(ctx, &models.UsageSummaryRequest{TenantID: "finalization", AgentID: "agent", From: &before, To: &after})
		if err != nil || len(rows) != 1 || rows[0].RunCount != 2 || rows[0].TokensIn != 12 {
			t.Fatal("summary merged distinct physical records", rows, err)
		}
		if _, tokens, count, err := held.SumUsageHolds(ctx, "finalization", "agent", before, after); err != nil || tokens != 17 || count != 1 {
			t.Fatal("replacement hold changed", tokens, count, err)
		}
		if err := holds.ReleaseRunUsageHold(ctx, "run", newID); err != nil {
			t.Fatal(err)
		}
		if _, tokens, count, err := held.SumUsageHolds(ctx, "finalization", "agent", before, after); err != nil || tokens != 0 || count != 0 {
			t.Fatal("own hold not released", tokens, count, err)
		}
		if err := events.WriteRunUsageEvent(tenant.ScopedContext(ctx, "foreign"), newID, &next); err == nil {
			t.Fatal("foreign tenant used original proof")
		}
	})
	t.Run("legacy_rows_never_borrow_current_identity", func(t *testing.T) {
		s, ctx, run := seed(t)
		events, ok := s.(state.RunUsageEventStore)
		if !ok {
			t.Skip("store has no SQL usage capability")
		}
		holds := s.(state.RunUsageHoldStore)
		raw := s.(interface {
			WriteUsageEvent(context.Context, *models.UsageEvent) error
			UpsertUsageHold(context.Context, *models.UsageHold) error
		})
		event := &models.UsageEvent{ID: "legacy", RunID: "run", TenantID: "finalization", Source: models.UsageSourceTerminalOutput, TokensIn: 5}
		if err := raw.WriteUsageEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
		modern := *event
		modern.ID = "modern"
		if err := events.WriteRunUsageEvent(ctx, state.RunIdentity(run), &modern); err == nil {
			t.Fatal("legacy accounting was rebound")
		}
		hold := &models.UsageHold{RunID: "run", TenantID: "finalization", TokensHold: 5}
		if err := raw.UpsertUsageHold(ctx, hold); err != nil {
			t.Fatal(err)
		}
		if err := holds.UpsertRunUsageHold(ctx, state.RunIdentity(run), hold); err == nil {
			t.Fatal("legacy hold was rebound")
		}
		if err := holds.ReleaseRunUsageHold(ctx, "run", state.RunIdentity(run)); err == nil {
			t.Fatal("legacy hold was silently released")
		}
		if err := events.WriteRunUsageEvent(ctx, "", &modern); err == nil {
			t.Fatal("missing original accounting proof accepted")
		}
		summary := s.(interface {
			SearchUsageSummary(context.Context, *models.UsageSummaryRequest) ([]models.UsageSummaryRow, error)
		})
		rows, err := summary.SearchUsageSummary(ctx, &models.UsageSummaryRequest{TenantID: "finalization"})
		if err != nil || len(rows) != 1 || rows[0].RunCount != 1 || rows[0].TokensIn != 5 {
			t.Fatal("legacy empty-identity bucket changed", rows, err)
		}
	})
}
