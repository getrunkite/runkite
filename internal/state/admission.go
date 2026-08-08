package state

import (
	"fmt"
	"time"
)

// RunAdmissionCaps are occupancy/quota ceilings enforced atomically with
// CreateRunAdmitted (lock + COUNT + INSERT on one connection/tx). Values
// <= 0 mean unlimited for that dimension.
type RunAdmissionCaps struct {
	TenantConcurrent int
	TenantDaily      int
	AgentConcurrent  int
	AgentDaily       int
	// Now is used for the UTC day boundary of max_daily. Zero means time.Now.
	Now time.Time
}

// Enabled reports whether any ceiling is set.
func (c *RunAdmissionCaps) Enabled() bool {
	return c != nil && (c.TenantConcurrent > 0 || c.TenantDaily > 0 || c.AgentConcurrent > 0 || c.AgentDaily > 0)
}

// DayStart returns the UTC midnight for Now (or time.Now if zero).
func (c *RunAdmissionCaps) DayStart() time.Time {
	now := c.Now
	if now.IsZero() {
		now = time.Now()
	}
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

// ErrAdmissionLimitExceeded is returned by CreateRunAdmitted when a
// concurrent or daily ceiling would be exceeded. Mapped to HTTP 429.
type ErrAdmissionLimitExceeded struct {
	Scope   string // "tenant" | "agent"
	Kind    string // "concurrent" | "daily"
	Limit   int
	Current int
}

func (e *ErrAdmissionLimitExceeded) Error() string {
	return fmt.Sprintf("admission_limits: %s %s at %d (limit %d)", e.Scope, e.Kind, e.Current, e.Limit)
}

// EvaluateRunAdmission runs COUNT checks against already-locked scope.
// countActive/countSince must observe the same transactional snapshot
// as the subsequent INSERT (same connection/tx).
func EvaluateRunAdmission(
	caps *RunAdmissionCaps,
	agentID string,
	countActive func(agentID string) (int, error),
	countSince func(since time.Time, agentID string) (int, error),
) error {
	if !caps.Enabled() {
		return nil
	}
	if caps.TenantConcurrent > 0 {
		n, err := countActive("")
		if err != nil {
			return fmt.Errorf("admission_limits: count tenant concurrent: %w", err)
		}
		if n >= caps.TenantConcurrent {
			return &ErrAdmissionLimitExceeded{Scope: "tenant", Kind: "concurrent", Limit: caps.TenantConcurrent, Current: n}
		}
	}
	if caps.AgentConcurrent > 0 {
		n, err := countActive(agentID)
		if err != nil {
			return fmt.Errorf("admission_limits: count agent concurrent: %w", err)
		}
		if n >= caps.AgentConcurrent {
			return &ErrAdmissionLimitExceeded{Scope: "agent", Kind: "concurrent", Limit: caps.AgentConcurrent, Current: n}
		}
	}
	dayStart := caps.DayStart()
	if caps.TenantDaily > 0 {
		n, err := countSince(dayStart, "")
		if err != nil {
			return fmt.Errorf("admission_limits: count tenant daily: %w", err)
		}
		if n >= caps.TenantDaily {
			return &ErrAdmissionLimitExceeded{Scope: "tenant", Kind: "daily", Limit: caps.TenantDaily, Current: n}
		}
	}
	if caps.AgentDaily > 0 {
		n, err := countSince(dayStart, agentID)
		if err != nil {
			return fmt.Errorf("admission_limits: count agent daily: %w", err)
		}
		if n >= caps.AgentDaily {
			return &ErrAdmissionLimitExceeded{Scope: "agent", Kind: "daily", Limit: caps.AgentDaily, Current: n}
		}
	}
	return nil
}
