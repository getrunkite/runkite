package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/sharanharsoor/runkite/internal/metrics"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/tenant"
)

// TimeoutOverdueRuns finds pending/running runs whose created_at is older
// than maxDuration and forces each to status "timeout". Multi-instance
// safe: TryMarkRunTimeout ensures exactly one replica wins per run_id,
// and only that winner cancels the queue lease, signals the runner,
// releases the thread, closes the broker, and finishes bookkeeping.
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
	for _, run := range runs {
		ok, markErr := s.store.TryMarkRunTimeout(sysCtx, run.RunID, errMsg)
		if markErr != nil {
			slog.Error("run timeout: TryMarkRunTimeout failed", "run_id", run.RunID, "error", markErr)
			continue
		}
		if !ok {
			continue
		}
		n++

		// Subsequent ops are scoped to the run's own tenant (same pattern
		// as StatusCallback): system context won the status race across
		// tenants; writes below must not silently land in "default".
		runCtx := tenant.WithContext(ctx, run.TenantID)
		_ = s.queue.Cancel(runCtx, run.RunID)
		_ = s.cancel.PublishCancel(runCtx, run.RunID)

		// Metrics normally land in StatusCallback, but a hung/pending run
		// may never ReportStatus -- record the terminal outcome here so
		// ActiveRuns doesn't leak for the cases this sweep exists to catch.
		metrics.ActiveRuns.Dec()
		metrics.RunsTotal.WithLabelValues(run.AgentID, string(models.RunStatusTimeout)).Inc()
		metrics.RunDuration.WithLabelValues(run.AgentID).Observe(time.Since(run.CreatedAt).Seconds())

		if _, err := s.store.ReleaseThreadIfNoOtherActive(runCtx, run.ThreadID, run.RunID, models.ThreadStatusIdle); err != nil {
			slog.Error("run timeout: failed to reset thread status", "thread_id", run.ThreadID, "error", err)
		}

		_ = s.broker.Close(run.RunID)
		s.finishRun(run.RunID, run.ThreadID, run.AgentID, models.RunStatusTimeout, errMsg)

		// Stop anything this run delegated to -- same cascade as an
		// explicit client cancel, so a timed-out parent doesn't leave
		// orphaned child runs executing with no caller left to stop them.
		s.cascadeCancelDescendants(runCtx, run)

		slog.Info("run timed out", "run_id", run.RunID, "thread_id", run.ThreadID, "agent_id", run.AgentID, "max_duration", maxDuration)
	}
	return n, nil
}
