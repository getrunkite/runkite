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

// Config is the runtime finops section (pricebook + budgets).
type Config struct {
	Pricebook Pricebook `json:"pricebook,omitempty"`
	Budgets   Budgets   `json:"budgets,omitempty"`
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
func UTCDayWindow(t time.Time) (since, until time.Time) {
	utc := t.UTC()
	since = time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	until = since.Add(24 * time.Hour)
	return since, until
}
