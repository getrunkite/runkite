package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// TimeoutOverdueRuns finds pending/running runs whose created_at is older
// than maxDuration and forces each to status "timeout". Multi-instance
// safe: the shared completion transaction selects one transition winner and
// atomically releases the thread. Queue removal follows successful finalization.
// The count includes newly committed timeouts, even if their finalization fails;
// an error reports incomplete bookkeeping that ordinary reclaim can retry.
//
// Distinct from crash reclaim (heartbeat lease): reclaim covers a dead
// runner; this covers a live one that never finishes (stuck tool loop,
// etc.). Opt-in via langgraph.json's "run_timeout" section -- see
// cmd/run_timeout.go.
func (s *Server) TimeoutOverdueRuns(ctx context.Context, maxDuration time.Duration, limit int) (int, error) {
	if maxDuration <= 0 {
		return 0, nil
	}
	sysCtx := tenant.SystemContext(ctx)
	before := time.Now().UTC().Add(-maxDuration)
	runs, err := s.store.ListActiveRunsCreatedBefore(sysCtx, before, limit)
	if err != nil {
		return 0, err
	}

	const errMsg = "run exceeded max_duration"
	n := 0
	var failures []error
	for _, run := range runs {
		result, completionErr := s.completeRunResult(ctx, &state.RunCompletion{
			RunID: run.RunID, Status: models.RunStatusTimeout, Error: errMsg, CancelCurrentAttempt: true,
		})
		if result.Applied {
			n++
		}
		runCtx := tenant.WithContext(ctx, run.TenantID)
		if completionErr != nil {
			if result.Accepted {
				// Stop the live runner while retaining its recoverable lease. A
				// reclaimed GetJob finalizes the committed timeout before Ack.
				tryStatusTransition("publish_cancel", run.ThreadID, run.RunID, func() error {
					return s.cancel.PublishCancel(runCtx, run.RunID)
				})
			}
			slog.Error("run timeout: durable completion failed", "run_id", run.RunID, "error", completionErr)
			failures = append(failures, fmt.Errorf("timeout run %s: %w", run.RunID, completionErr))
			continue
		}
		if !result.Accepted {
			continue
		}
		if !tryStatusTransition("queue_cancel", run.ThreadID, run.RunID, func() error {
			return s.queue.Cancel(runCtx, run.RunID)
		}) {
			failures = append(failures, fmt.Errorf("timeout run %s: failed to cancel run queue lease", run.RunID))
		}
		if !tryStatusTransition("publish_cancel", run.ThreadID, run.RunID, func() error {
			return s.cancel.PublishCancel(runCtx, run.RunID)
		}) {
			failures = append(failures, fmt.Errorf("timeout run %s: failed to publish runner cancellation", run.RunID))
		}

		// Stop anything this run delegated to -- same cascade as an
		// explicit client cancel, so a timed-out parent doesn't leave
		// orphaned child runs executing with no caller left to stop them.
		s.cascadeCancelDescendants(runCtx, run)

		slog.Info("run timed out", "run_id", run.RunID, "thread_id", run.ThreadID, "agent_id", run.AgentID, "max_duration", maxDuration)
	}
	return n, errors.Join(failures...)
}
