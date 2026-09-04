package finops

import (
	"fmt"
	"math"
	"strings"
)

// Sanity ceilings for Admin write path. Fat-fingered rates/budgets are
// the same failure class as silent metering bugs — reject before apply.
const (
	maxPer1kUSD     = 100.0       // $100 / 1k tokens is already absurd
	maxBudgetUSDDay = 1_000_000.0 // $1M/day
	maxBudgetTokens = int64(1_000_000_000)
	maxBudgetRuns   = int64(10_000_000)
	maxHoldUSD      = 10_000.0
	maxHoldTokens   = int64(10_000_000)
)

// ValidateOverlay checks an Admin-supplied FinOps patch. Returns a
// human-readable error suitable for HTTP 400.
func ValidateOverlay(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("overlay payload is required")
	}
	for model, p := range cfg.Pricebook {
		model = strings.TrimSpace(model)
		if model == "" {
			return fmt.Errorf("pricebook model id must be non-empty")
		}
		if math.IsNaN(p.InputPer1k) || math.IsInf(p.InputPer1k, 0) || p.InputPer1k < 0 {
			return fmt.Errorf("pricebook %q input_per_1k must be a finite number >= 0", model)
		}
		if math.IsNaN(p.OutputPer1k) || math.IsInf(p.OutputPer1k, 0) || p.OutputPer1k < 0 {
			return fmt.Errorf("pricebook %q output_per_1k must be a finite number >= 0", model)
		}
		if p.InputPer1k > maxPer1kUSD || p.OutputPer1k > maxPer1kUSD {
			return fmt.Errorf("pricebook %q rate exceeds %.0f USD per 1k tokens (likely typo)", model, maxPer1kUSD)
		}
		// Swapped rates are a common typo when output ≪ input in the book.
		if p.InputPer1k > 0 && p.OutputPer1k > 0 && p.InputPer1k > p.OutputPer1k*20 {
			return fmt.Errorf("pricebook %q input_per_1k (%.6f) is >20x output_per_1k (%.6f) — check swapped rates", model, p.InputPer1k, p.OutputPer1k)
		}
	}
	checkCap := func(scope, key string, c BudgetCap) error {
		if math.IsNaN(c.MaxUSDPerDay) || math.IsInf(c.MaxUSDPerDay, 0) || c.MaxUSDPerDay < 0 {
			return fmt.Errorf("%s %q max_usd_per_day must be a finite number >= 0", scope, key)
		}
		if c.MaxUSDPerDay > maxBudgetUSDDay {
			return fmt.Errorf("%s %q max_usd_per_day exceeds %.0f", scope, key, maxBudgetUSDDay)
		}
		if c.MaxTokensPerDay < 0 || c.MaxTokensPerDay > maxBudgetTokens {
			return fmt.Errorf("%s %q max_tokens_per_day out of range", scope, key)
		}
		if c.MaxRunsPerDay < 0 || c.MaxRunsPerDay > maxBudgetRuns {
			return fmt.Errorf("%s %q max_runs_per_day out of range", scope, key)
		}
		return nil
	}
	for k, c := range cfg.Budgets.Tenants {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("budgets.tenants key must be non-empty")
		}
		if err := checkCap("budgets.tenants", k, c); err != nil {
			return err
		}
	}
	for k, c := range cfg.Budgets.Agents {
		if strings.TrimSpace(k) == "" || !strings.Contains(k, "/") {
			return fmt.Errorf("budgets.agents key %q must be tenant_id/agent_id", k)
		}
		if err := checkCap("budgets.agents", k, c); err != nil {
			return err
		}
	}
	if cfg.Alerts.SoftPct < 0 || cfg.Alerts.SoftPct > 100 {
		return fmt.Errorf("alerts.soft_pct must be in [0,100]")
	}
	if cfg.Reservation.USDPerRun < 0 || cfg.Reservation.USDPerRun > maxHoldUSD {
		return fmt.Errorf("reservation.usd_per_run out of range")
	}
	if cfg.Reservation.TokensPerRun < 0 || cfg.Reservation.TokensPerRun > maxHoldTokens {
		return fmt.Errorf("reservation.tokens_per_run out of range")
	}
	if cfg.Reservation.HoldTTL < 0 {
		return fmt.Errorf("reservation.hold_ttl must be >= 0")
	}
	for k, a := range cfg.Reservation.Agents {
		if strings.TrimSpace(k) == "" || !strings.Contains(k, "/") {
			return fmt.Errorf("reservation.agents key %q must be tenant_id/agent_id", k)
		}
		if a.USDPerRun < 0 || a.USDPerRun > maxHoldUSD || a.TokensPerRun < 0 || a.TokensPerRun > maxHoldTokens {
			return fmt.Errorf("reservation.agents %q amounts out of range", k)
		}
	}
	if cfg.Routing.SoftPct < 0 || cfg.Routing.SoftPct > 100 {
		return fmt.Errorf("routing.soft_pct must be in [0,100]")
	}
	for alias, targets := range cfg.Routing.Aliases {
		if strings.TrimSpace(alias) == "" {
			return fmt.Errorf("routing.aliases key must be non-empty")
		}
		if len(targets) == 0 {
			return fmt.Errorf("routing.aliases %q needs at least one target agent_id", alias)
		}
		for _, t := range targets {
			if strings.TrimSpace(t) == "" {
				return fmt.Errorf("routing.aliases %q has an empty target", alias)
			}
		}
	}
	switch cfg.OnHardBreach {
	case "", OnHardBreachCancelInflight:
	default:
		return fmt.Errorf("on_hard_breach must be empty or %q", OnHardBreachCancelInflight)
	}
	return nil
}
