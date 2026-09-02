package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/tenant"
)

func TestUsageEvents_WriteSumSummary(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := tenant.SystemContext(context.Background())
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	ev := &models.UsageEvent{
		ID: "u1", TS: ts, TenantID: "acme", RunID: "r1", AgentID: "echo",
		TokensIn: 100, TokensOut: 50, USDEstimate: 0.01, Source: models.UsageSourceTerminalOutput,
	}
	if err := s.WriteUsageEvent(ctx, ev); err != nil {
		t.Fatalf("WriteUsageEvent: %v", err)
	}
	// Upsert same run+source replaces totals.
	ev2 := &models.UsageEvent{
		ID: "u1b", TS: ts, TenantID: "acme", RunID: "r1", AgentID: "echo",
		TokensIn: 200, TokensOut: 80, USDEstimate: 0.02, Source: models.UsageSourceTerminalOutput,
	}
	if err := s.WriteUsageEvent(ctx, ev2); err != nil {
		t.Fatalf("WriteUsageEvent upsert: %v", err)
	}

	since := time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC)
	until := since.Add(24 * time.Hour)
	tin, tout, usd, err := s.SumUsage(ctx, "acme", "echo", since, until)
	if err != nil {
		t.Fatal(err)
	}
	if tin != 200 || tout != 80 || usd != 0.02 {
		t.Fatalf("SumUsage = %d/%d/%v, want 200/80/0.02", tin, tout, usd)
	}

	rows, err := s.SearchUsageSummary(ctx, &models.UsageSummaryRequest{TenantID: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Day != "2026-09-02" || rows[0].TokensIn != 200 || rows[0].RunCount != 1 {
		t.Fatalf("SearchUsageSummary = %#v", rows)
	}
}

func TestCountRunsSince(t *testing.T) {
	s, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := tenant.WithContext(context.Background(), "acme")
	if err := s.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.CreateThread(ctx, &models.Thread{ThreadID: "t1", Status: models.ThreadStatusIdle, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, &models.Run{
		RunID: "r-a", ThreadID: "t1", AgentID: "echo", Status: models.RunStatusPending,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountRunsSince(context.Background(), "acme", "echo", now.Add(-time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("CountRunsSince = %d err=%v", n, err)
	}
	n, err = s.CountRunsSince(context.Background(), "acme", "other", now.Add(-time.Minute))
	if err != nil || n != 0 {
		t.Fatalf("CountRunsSince other = %d err=%v", n, err)
	}
}
