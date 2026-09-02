package models

import "time"

// UsageEvent is one durable token/cost observation for a run (SQL backends).
// Terminal ingest writes source=terminal_output from run Output.usage.
type UsageEvent struct {
	ID          string    `json:"id"`
	TS          time.Time `json:"ts"`
	TenantID    string    `json:"tenant_id"`
	RunID       string    `json:"run_id"`
	AgentID     string    `json:"agent_id,omitempty"`
	Principal   string    `json:"principal,omitempty"`
	Model       string    `json:"model,omitempty"`
	TokensIn    int64     `json:"tokens_in"`
	TokensOut   int64     `json:"tokens_out"`
	USDEstimate float64   `json:"usd_estimate"`
	Source      string    `json:"source"`
}

// UsageSourceTerminalOutput is written when a run reaches a terminal
// status with Output containing a conventional usage object.
const UsageSourceTerminalOutput = "terminal_output"

// UsageSummaryRequest filters Admin usage rollups
// (GET /admin-api/usage/summary). SQL backends only (not Mongo).
type UsageSummaryRequest struct {
	TenantID string     `json:"tenant_id,omitempty"`
	AgentID  string     `json:"agent_id,omitempty"`
	From     *time.Time `json:"from,omitempty"` // inclusive lower bound on ts
	To       *time.Time `json:"to,omitempty"`   // exclusive upper bound on ts
}

// UsageSummaryRow is one (tenant, agent, UTC day) rollup of usage_events.
type UsageSummaryRow struct {
	Day         string  `json:"day"` // YYYY-MM-DD (UTC)
	TenantID    string  `json:"tenant_id"`
	AgentID     string  `json:"agent_id"`
	TokensIn    int64   `json:"tokens_in"`
	TokensOut   int64   `json:"tokens_out"`
	USDEstimate float64 `json:"usd_estimate"`
	RunCount    int64   `json:"run_count"`
}
