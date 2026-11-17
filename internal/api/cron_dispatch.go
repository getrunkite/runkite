package api

import (
	"errors"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
)

// ScheduledRunDispatchError carries the internal scheduler's retry disposition.
// Its original cause remains available through errors.Is and errors.As. It is
// not an HTTP response or proof of an exactly-once external effect.
type ScheduledRunDispatchError struct {
	cause     error
	retrySafe bool
}

func (e *ScheduledRunDispatchError) Error() string {
	if e.cause == nil {
		return "scheduled run dispatch failed"
	}
	return e.cause.Error()
}

func (e *ScheduledRunDispatchError) Unwrap() error { return e.cause }

// RetrySafe is true only after this call atomically failed its original run
// before any worker attempt began, with successful completion/finalization.
func (e *ScheduledRunDispatchError) RetrySafe() bool { return e != nil && e.retrySafe }

// runCreationProgress is private to scheduled creation. An attempted write can
// commit before returning an error, so absence of a returned run is not proof
// that retry is safe. Other run callers retain their existing contract.
type runCreationProgress struct{ attempted *models.Run }

func (p *runCreationProgress) beforeStore(run *models.Run) {
	if p != nil {
		p.attempted = run
	}
}

// These explicit atomic-store rejections prove that no run was inserted.
// Generic store errors, including ambiguous commit errors, provide no proof.
func scheduledCreationRejected(err error) bool {
	var admission *state.ErrAdmissionLimitExceeded
	if errors.As(err, &admission) {
		return true
	}
	var conflict *state.ErrConflict
	return errors.As(err, &conflict) && conflict.Resource == "thread" && conflict.Reason == "is busy"
}
