package finops_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/getrunkite/runkite/internal/finops"
)

func TestMerge_OverlayWinsOnMaps(t *testing.T) {
	base := &finops.Config{
		Pricebook: finops.Pricebook{"a": {InputPer1k: 1, OutputPer1k: 2}},
		Budgets: finops.Budgets{
			Tenants: map[string]finops.BudgetCap{"t1": {MaxUSDPerDay: 10}},
		},
	}
	overlay := &finops.Config{
		Pricebook: finops.Pricebook{"a": {InputPer1k: 3, OutputPer1k: 4}, "b": {InputPer1k: 0.1, OutputPer1k: 0.2}},
		Budgets: finops.Budgets{
			Tenants: map[string]finops.BudgetCap{"t1": {MaxUSDPerDay: 50}},
		},
		Alerts:       finops.AlertsConfig{SoftPct: 90},
		OnHardBreach: finops.OnHardBreachCancelInflight,
	}
	got := finops.Merge(base, overlay)
	if got.Pricebook["a"].InputPer1k != 3 || got.Pricebook["b"].OutputPer1k != 0.2 {
		t.Fatalf("pricebook merge = %#v", got.Pricebook)
	}
	if got.Budgets.Tenants["t1"].MaxUSDPerDay != 50 {
		t.Fatalf("budget merge = %#v", got.Budgets.Tenants)
	}
	if got.Alerts.SoftPct != 90 || got.OnHardBreach != finops.OnHardBreachCancelInflight {
		t.Fatalf("scalars = %#v %#v", got.Alerts, got.OnHardBreach)
	}
	if base.Pricebook["a"].InputPer1k != 1 {
		t.Fatal("Merge mutated base")
	}
}

func TestValidateOverlay_RejectsAbsurdRatesAndSwapped(t *testing.T) {
	err := finops.ValidateOverlay(&finops.Config{
		Pricebook: finops.Pricebook{"m": {InputPer1k: -1, OutputPer1k: 1}},
	})
	if err == nil {
		t.Fatal("expected negative rate reject")
	}
	err = finops.ValidateOverlay(&finops.Config{
		Pricebook: finops.Pricebook{"m": {InputPer1k: 50, OutputPer1k: 1}},
	})
	if err == nil {
		t.Fatal("expected swapped-rate reject")
	}
	err = finops.ValidateOverlay(&finops.Config{
		Budgets: finops.Budgets{Agents: map[string]finops.BudgetCap{"no-slash": {MaxUSDPerDay: 1}}},
	})
	if err == nil {
		t.Fatal("expected agent key format reject")
	}
	err = finops.ValidateOverlay(&finops.Config{
		Pricebook: finops.Pricebook{"gemini-2.5-flash": {InputPer1k: 0.00015, OutputPer1k: 0.0006}},
		Budgets: finops.Budgets{
			Agents: map[string]finops.BudgetCap{"default/agent": {MaxUSDPerDay: 25, Soft: true}},
		},
	})
	if err != nil {
		t.Fatalf("valid overlay rejected: %v", err)
	}
}

func TestEncodeDecodeOverlay_HoldTTLString(t *testing.T) {
	cfg := &finops.Config{
		Reservation: finops.ReservationConfig{USDPerRun: 0.5, HoldTTL: 24 * time.Hour},
	}
	raw, err := finops.EncodeOverlayPayload(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	res := probe["reservation"].(map[string]any)
	if res["hold_ttl"] != "24h0m0s" && res["hold_ttl"] != "24h" {
		// Duration.String() is "24h0m0s"
		if _, ok := res["hold_ttl"].(string); !ok {
			t.Fatalf("hold_ttl not string: %#v", res["hold_ttl"])
		}
	}
	back, err := finops.DecodeOverlayPayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Reservation.HoldTTL != 24*time.Hour {
		t.Fatalf("hold ttl = %v", back.Reservation.HoldTTL)
	}
}
