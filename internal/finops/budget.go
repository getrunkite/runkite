package finops

import "time"

// BudgetCap is one tenant- or agent-scoped daily ceiling. Zero / unset
// fields are unlimited. Soft=true means admission still allows but
// emits a budget_soft audit; Soft=false (default) hard-denies.
type BudgetCap struct {
	MaxUSDPerDay    float64 `json:"max_usd_per_day,omitempty"`
	MaxTokensPerDay int64   `json:"max_tokens_per_day,omitempty"`
	MaxRunsPerDay   int64   `json:"max_runs_per_day,omitempty"`
	Soft            bool    `json:"soft,omitempty"`
}

// Budgets holds optional tenant and agent caps from langgraph.json
// finops.budgets. Agent keys are "tenant_id/agent_id".
type Budgets struct {
	Tenants map[string]BudgetCap `json:"tenants,omitempty"`
	Agents  map[string]BudgetCap `json:"agents,omitempty"`
}

// AlertsConfig controls soft_pct approach alerts. Soft/hard budget
// trips always emit budget_alert hooks; SoftPct adds approaches.
type AlertsConfig struct {
	SoftPct float64 // 0 → default 80
}

// ReservationConfig is optimistic per-create hold amounts.
type ReservationConfig struct {
	USDPerRun    float64
	TokensPerRun int64
}

// RoutingConfig rewrites aliases to cheaper agents near soft_pct.
type RoutingConfig struct {
	Enabled bool
	SoftPct float64 // 0 → alerts SoftPct / 80
	Aliases map[string][]string
}

// Config is the runtime finops section (pricebook + budgets + F3+).
type Config struct {
	Pricebook   Pricebook          `json:"pricebook,omitempty"`
	Budgets     Budgets            `json:"budgets,omitempty"`
	Alerts      AlertsConfig       `json:"alerts,omitempty"`
	Reservation ReservationConfig  `json:"reservation,omitempty"`
	Routing     RoutingConfig      `json:"routing,omitempty"`
}

// SoftPct returns the configured approach threshold (default 80).
func (c *Config) SoftPct() float64 {
	if c != nil && c.Alerts.SoftPct > 0 {
		return c.Alerts.SoftPct
	}
	return 80
}

// RoutingSoftPct returns routing threshold (routing > alerts > 80).
func (c *Config) RoutingSoftPct() float64 {
	if c != nil && c.Routing.SoftPct > 0 {
		return c.Routing.SoftPct
	}
	return c.SoftPct()
}

// ReservationEnabled is true when any hold amount is configured.
func (c *Config) ReservationEnabled() bool {
	return c != nil && (c.Reservation.USDPerRun > 0 || c.Reservation.TokensPerRun > 0)
}

// Enabled reports whether any budget cap is configured.
func (c *Config) Enabled() bool {
	if c == nil {
		return false
	}
	return len(c.Budgets.Tenants) > 0 || len(c.Budgets.Agents) > 0
}

// HasPricebook reports whether any model prices are configured.
func (c *Config) HasPricebook() bool {
	return c != nil && len(c.Pricebook) > 0
}

// LookupCaps returns tenant and agent caps for a create (agent may be nil).
func (c *Config) LookupCaps(tenantID, agentID string) (tenantCap, agentCap *BudgetCap) {
	if c == nil {
		return nil, nil
	}
	if tenantID != "" {
		if cap, ok := c.Budgets.Tenants[tenantID]; ok {
			c2 := cap
			tenantCap = &c2
		}
	}
	if tenantID != "" && agentID != "" {
		key := tenantID + "/" + agentID
		if cap, ok := c.Budgets.Agents[key]; ok {
			c2 := cap
			agentCap = &c2
		}
	}
	return tenantCap, agentCap
}

// UsageSnapshot is current-day spend for budget evaluation.
type UsageSnapshot struct {
	TokensIn  int64
	TokensOut int64
	USD       float64
	Runs      int64
}

// Tokens returns tokens_in + tokens_out.
func (u UsageSnapshot) Tokens() int64 {
	return u.TokensIn + u.TokensOut
}

// BudgetVerdict is the result of checking one or more caps.
type BudgetVerdict struct {
	Allow      bool
	Soft       bool   // true when Allow and at least one soft cap tripped
	Hard       bool   // true when !Allow due to a hard cap
	Reason     string
	Scope      string // "tenant" or "agent"
	CapKind    string // "usd" | "tokens" | "runs"
}

// EvaluateCap compares a snapshot against one cap. Empty cap → allow.
// Hard deny wins over soft allow when multiple dimensions trip.
func EvaluateCap(cap *BudgetCap, snap UsageSnapshot, scope string) BudgetVerdict {
	if cap == nil {
		return BudgetVerdict{Allow: true}
	}
	var soft *BudgetVerdict
	check := func(kind string, over bool) *BudgetVerdict {
		if !over {
			return nil
		}
		reason := scope + " " + kind + " budget exceeded"
		if !cap.Soft {
			return &BudgetVerdict{Allow: false, Hard: true, Reason: reason, Scope: scope, CapKind: kind}
		}
		return &BudgetVerdict{Allow: true, Soft: true, Reason: reason, Scope: scope, CapKind: kind}
	}
	if v := check("usd", cap.MaxUSDPerDay > 0 && snap.USD >= cap.MaxUSDPerDay); v != nil {
		if v.Hard {
			return *v
		}
		soft = v
	}
	if v := check("tokens", cap.MaxTokensPerDay > 0 && snap.Tokens() >= cap.MaxTokensPerDay); v != nil {
		if v.Hard {
			return *v
		}
		if soft == nil {
			soft = v
		}
	}
	if v := check("runs", cap.MaxRunsPerDay > 0 && snap.Runs >= cap.MaxRunsPerDay); v != nil {
		if v.Hard {
			return *v
		}
		if soft == nil {
			soft = v
		}
	}
	if soft != nil {
		return *soft
	}
	return BudgetVerdict{Allow: true}
}

// UTCDayWindow returns [start, end) for the UTC calendar day containing t.

// ApproachCap reports a soft-style verdict when snap is at/above softPct
// of a hard (non-Soft) cap but not yet over the cap. softPct in (0,100];
// 0 means 80. Soft caps are ignored here (they already warn at 100%).
func ApproachCap(cap *BudgetCap, snap UsageSnapshot, scope string, softPct float64) *BudgetVerdict {
	if cap == nil || cap.Soft {
		return nil
	}
	if softPct <= 0 {
		softPct = 80
	}
	if softPct > 100 {
		softPct = 100
	}
	frac := softPct / 100
	check := func(kind string, used, max float64) *BudgetVerdict {
		if max <= 0 || used < max*frac || used >= max {
			return nil
		}
		return &BudgetVerdict{
			Allow: true, Soft: true,
			Reason: scope + " " + kind + " budget approaching",
			Scope: scope, CapKind: kind,
		}
	}
	if v := check("usd", snap.USD, cap.MaxUSDPerDay); v != nil {
		return v
	}
	if v := check("tokens", float64(snap.Tokens()), float64(cap.MaxTokensPerDay)); v != nil {
		return v
	}
	if v := check("runs", float64(snap.Runs), float64(cap.MaxRunsPerDay)); v != nil {
		return v
	}
	return nil
}

func UTCDayWindow(t time.Time) (since, until time.Time) {
	utc := t.UTC()
	since = time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	until = since.Add(24 * time.Hour)
	return since, until
}
