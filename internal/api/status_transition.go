package api

import (
	"log/slog"

	"github.com/getrunkite/runkite/internal/metrics"
)

// tryStatusTransition runs a thread/run status side effect once, retries
// once on error, then logs ERROR and increments
// runkite_status_transition_errors_total. Returns true if the write
// eventually succeeded. Call sites that previously discarded these
// errors left threads stuck busy (permanent 409s) or runs stuck
// non-terminal with no signal in logs or metrics.
func tryStatusTransition(op, threadID, runID string, fn func() error) bool {
	err := fn()
	if err != nil {
		err = fn()
	}
	if err == nil {
		return true
	}
	slog.Error("status transition failed after retry",
		"op", op, "thread_id", threadID, "run_id", runID, "error", err)
	metrics.StatusTransitionErrorsTotal.WithLabelValues(op).Inc()
	return false
}
