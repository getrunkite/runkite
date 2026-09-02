package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLangGraphJSON_FinOps(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"finops": {
			"pricebook": {
				"gpt-4o-mini": {"input_per_1k": 0.00015, "output_per_1k": 0.0006}
			},
			"budgets": {
				"tenants": {
					"acme": {"max_usd_per_day": 10, "max_tokens_per_day": 1000000, "max_runs_per_day": 100, "soft": false}
				},
				"agents": {
					"acme/echo": {"max_usd_per_day": 1, "soft": true}
				}
			}
		}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatalf("LoadLangGraphJSON: %v", err)
	}
	if cfg.FinOps == nil {
		t.Fatal("expected FinOps")
	}
	p := cfg.FinOps.Pricebook["gpt-4o-mini"]
	if p.InputPer1k != 0.00015 || p.OutputPer1k != 0.0006 {
		t.Fatalf("pricebook = %+v", p)
	}
	tc := cfg.FinOps.Budgets.Tenants["acme"]
	if tc.MaxUSDPerDay != 10 || tc.MaxRunsPerDay != 100 || tc.Soft {
		t.Fatalf("tenant cap = %+v", tc)
	}
	ac := cfg.FinOps.Budgets.Agents["acme/echo"]
	if ac.MaxUSDPerDay != 1 || !ac.Soft {
		t.Fatalf("agent cap = %+v", ac)
	}
}

func TestLoadLangGraphJSON_NoFinOpsIsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(`{"graphs": {"echo": "graph.py:graph"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FinOps != nil {
		t.Errorf("expected nil FinOps, got %+v", cfg.FinOps)
	}
}

func TestLoadLangGraphJSON_FinOpsContinuum(t *testing.T) {
	dir := t.TempDir()
	content := `{
		"graphs": {"echo": "graph.py:graph"},
		"finops": {
			"alerts": {"soft_pct": 75},
			"reservation": {"usd_per_run": 0.05, "tokens_per_run": 1000, "hold_ttl": "24h", "agents": {"acme/premium": {"usd_per_run": 0.5}}},
			"routing": {
				"enabled": true,
				"soft_pct": 70,
				"aliases": {"premium": ["economy"]}
			},
			"on_hard_breach": "cancel_inflight"
		}
	}`
	path := filepath.Join(dir, "langgraph.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLangGraphJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FinOps.Alerts == nil || cfg.FinOps.Alerts.SoftPct != 75 {
		t.Fatalf("alerts = %+v", cfg.FinOps.Alerts)
	}
	if cfg.FinOps.Reservation == nil || cfg.FinOps.Reservation.USDPerRun != 0.05 || cfg.FinOps.Reservation.HoldTTL != "24h" {
		t.Fatalf("reservation = %+v", cfg.FinOps.Reservation)
	}
	if cfg.FinOps.Reservation.Agents["acme/premium"].USDPerRun != 0.5 {
		t.Fatalf("reservation.agents = %+v", cfg.FinOps.Reservation.Agents)
	}
	if cfg.FinOps.Routing == nil || !cfg.FinOps.Routing.Enabled || cfg.FinOps.Routing.Aliases["premium"][0] != "economy" {
		t.Fatalf("routing = %+v", cfg.FinOps.Routing)
	}
	if cfg.FinOps.OnHardBreach != "cancel_inflight" {
		t.Fatalf("on_hard_breach = %q", cfg.FinOps.OnHardBreach)
	}
}
