package finops_test

import (
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/finops"
)

func TestPricebook_EstimateUSD(t *testing.T) {
	pb := finops.Pricebook{
		"gpt-4o-mini": {InputPer1k: 0.00015, OutputPer1k: 0.0006},
	}
	if got := pb.EstimateUSD("gpt-4o-mini", 0, 0, 1.5); got != 1.5 {
		t.Fatalf("cost_usd wins: got %v", got)
	}
	got := pb.EstimateUSD("gpt-4o-mini", 1000, 1000, 0)
	want := 0.00015 + 0.0006
	if abs(got-want) > 1e-12 {
		t.Fatalf("priced tokens: got %v want %v", got, want)
	}
	if got := (finops.Pricebook{}).EstimateUSD("gpt-4o-mini", 1000, 1000, 0); got != 0 {
		t.Fatalf("empty book without cost_usd → 0, got %v", got)
	}
}

func TestEvaluateCap_HardAndSoft(t *testing.T) {
	hard := &finops.BudgetCap{MaxRunsPerDay: 2}
	v := finops.EvaluateCap(hard, finops.UsageSnapshot{Runs: 2}, "tenant")
	if v.Allow || !v.Hard || v.CapKind != "runs" {
		t.Fatalf("hard deny: %#v", v)
	}
	soft := &finops.BudgetCap{MaxUSDPerDay: 1, Soft: true}
	v = finops.EvaluateCap(soft, finops.UsageSnapshot{USD: 1.5}, "agent")
	if !v.Allow || !v.Soft || v.CapKind != "usd" {
		t.Fatalf("soft allow: %#v", v)
	}
}

func TestUTCDayWindow(t *testing.T) {
	ts := time.Date(2026, 9, 2, 15, 30, 0, 0, time.UTC)
	since, until := finops.UTCDayWindow(ts)
	if since.Hour() != 0 || until.Sub(since) != 24*time.Hour {
		t.Fatalf("window = %v %v", since, until)
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
