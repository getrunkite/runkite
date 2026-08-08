package api

import (
	"context"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

// AdmissionLimits is the runtime occupancy/quota config (values <= 0 =
// unlimited). Distinct from rate_limit token buckets.
type AdmissionLimits struct {
	TenantConcurrent int
	TenantDaily      int
	AgentConcurrent  int
	AgentDaily       int
}

// Enabled reports whether any ceiling is configured.
func (l *AdmissionLimits) Enabled() bool {
	return l != nil && (l.TenantConcurrent > 0 || l.TenantDaily > 0 || l.AgentConcurrent > 0 || l.AgentDaily > 0)
}

func (l *AdmissionLimits) toCaps(now time.Time) *state.RunAdmissionCaps {
	if !l.Enabled() {
		return nil
	}
	return &state.RunAdmissionCaps{
		TenantConcurrent: l.TenantConcurrent,
		TenantDaily:      l.TenantDaily,
		AgentConcurrent:  l.AgentConcurrent,
		AgentDaily:       l.AgentDaily,
		Now:              now,
	}
}

func retryAfterAdmission(err *state.ErrAdmissionLimitExceeded, now time.Time) int {
	if err != nil && err.Kind == "daily" {
		return secondsUntilUTCMidnight(now)
	}
	return 1
}

func secondsUntilUTCMidnight(now time.Time) int {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	sec := int(next.Sub(utc).Seconds())
	if sec < 1 {
		return 1
	}
	return sec
}

// createRunRespectingLimits inserts via CreateRunAdmitted when caps are
// configured so COUNT+INSERT share one locked connection/transaction.
func (s *Server) createRunRespectingLimits(ctx context.Context, run *models.Run, now time.Time) error {
	if caps := s.admissionLimits.toCaps(now); caps != nil {
		return s.store.CreateRunAdmitted(ctx, run, caps)
	}
	return s.store.CreateRun(ctx, run)
}
