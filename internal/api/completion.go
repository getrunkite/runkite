package api

import (
	"context"
	"encoding/json"
	"fmt"

	"time"

	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// BeginRunAttempt is called before a dequeued assignment is delivered. The
// durable fence makes a completion from a superseded dispatch harmless even
// when reclaim races a SQL write or a runner reconnects to another replica.
func (s *Server) BeginRunAttempt(ctx context.Context, id string, generation int64) (bool, error) {
	// Recover a completed event stream before delivering a reclaimed job.
	// This resends no model/tool work merely because ReportStatus was interrupted.
	run, err := s.store.GetRun(tenant.SystemContext(ctx), id)
	if err != nil {
		return false, err
	}
	if state.TerminalRunStatus(run.Status) {
		if _, done := run.Metadata[state.CompletionGenerationKey]; done {
			if err := s.finalizeCommittedRun(tenant.WithContext(ctx, run.TenantID), run, false); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	previous := state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey)
	_, attempted := run.Metadata[state.AttemptGenerationKey]
	if attempted && generation > previous {
		accepted, err := s.recoverDeliveredCompletion(ctx, run)
		if err != nil {
			return false, err
		}
		if accepted {
			return false, nil
		}
	}
	return s.store.BeginRunAttempt(tenant.SystemContext(ctx), id, generation)
}

// recoverDeliveredCompletion preserves a delivered agent outcome when the
// final stream acknowledgement or ReportStatus is lost. Reclaim also uses it
// at the retry ceiling, where there will be no next GetJob to recover it.
func (s *Server) recoverDeliveredCompletion(ctx context.Context, run *models.Run) (bool, error) {
	generation := state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey)
	events, err := s.broker.Replay(ctx, run.RunID, 0)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if !event.IsTerminal() || event.Generation != generation {
			continue
		}
		status := models.RunStatusError
		var data struct {
			Status  models.RunStatus `json:"status"`
			Message string           `json:"message"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return false, err
		}
		if event.Method == "end" {
			status = data.Status
		}
		return s.CompleteRun(ctx, &state.RunCompletion{RunID: run.RunID, Generation: generation, Status: status, Error: data.Message})
	}
	return false, nil
}

// CompleteReclaimedRun finalizes an exhausted assignment before the reaper
// removes its retained lease. Backend and finalization failures are retried by
// the next reclaim tick, including after a control-plane process restarts.
func (s *Server) CompleteReclaimedRun(ctx context.Context, pill transport.PoisonPill, errorMsg string) (bool, error) {
	if pill.Generation <= 0 {
		return false, fmt.Errorf("invalid poison-pill generation")
	}
	generation := pill.Generation - 1
	assignment, err := s.queue.LookupInflight(ctx, pill.RunID)
	if err != nil {
		return false, err
	}
	if assignment == nil || assignment.Generation != generation {
		return false, nil
	}
	run, err := s.store.GetRun(tenant.SystemContext(ctx), pill.RunID)
	if err != nil {
		return false, err
	}
	if state.TerminalRunStatus(run.Status) {
		if _, done := run.Metadata[state.CompletionGenerationKey]; done {
			return true, s.finalizeCommittedRun(tenant.WithContext(ctx, run.TenantID), run, false)
		}
		return true, nil
	}
	current := state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey)
	if current > generation {
		return false, nil
	}
	_, attempted := run.Metadata[state.AttemptGenerationKey]
	if attempted {
		if accepted, err := s.recoverDeliveredCompletion(ctx, run); accepted || err != nil {
			return accepted, err
		}
	}
	// A queue claim may have outlived its GetJob process before dispatch was
	// recorded. Only the trusted reaper can bind that retained lease here.
	if current != generation || !attempted {
		accepted, err := s.store.BeginRunAttempt(tenant.SystemContext(ctx), pill.RunID, generation)
		if err != nil || !accepted {
			return false, err
		}
	}
	return s.CompleteRun(ctx, &state.RunCompletion{RunID: pill.RunID, Generation: generation, Status: models.RunStatusError, Error: errorMsg})
}

// CompleteRun is retryable after either an RPC failure or a lost response. The
// state store commits the result before the bridge removes its queue lease.
func (s *Server) CompleteRun(ctx context.Context, c *state.RunCompletion) (bool, error) {
	run, err := s.store.GetRun(tenant.SystemContext(ctx), c.RunID)
	if err != nil {
		return false, err
	}
	if c.RunnerKind != "" {
		var kind string
		if manifest, ok := run.Metadata["run_manifest"].(map[string]interface{}); ok {
			kind, _ = manifest["runner_kind"].(string)
		}
		if kind == "" {
			agent, lookupErr := s.store.GetAgent(tenant.WithContext(ctx, run.TenantID), run.AgentID)
			if lookupErr != nil {
				return false, lookupErr
			}
			kind, _ = agent.Metadata["runner_kind"].(string)
			if kind == "" {
				kind = "python-langgraph"
			}
		}
		if kind != c.RunnerKind {
			return false, fmt.Errorf("run belongs to another runner kind")
		}
	}
	ctx = tenant.WithContext(ctx, run.TenantID)
	// An absent queue lease permits only replay of an already committed result.
	// It must never let an unaffiliated worker finish a pending run by guessing id.
	if c.ReplayOnly {
		_, done := run.Metadata[state.CompletionGenerationKey]
		if !done || state.MetadataGeneration(run.Metadata, state.CompletionGenerationKey) != c.Generation || run.Status != c.Status {
			return false, nil
		}
		return true, s.finalizeCommittedRun(ctx, run, false)
	}
	if !state.TerminalRunStatus(c.Status) {
		return false, fmt.Errorf("invalid terminal run status %q", c.Status)
	}
	completion := *c
	if !completion.CancelCurrentAttempt {
		if checked := state.CheckCompletion(run.Metadata, run.Status, &completion); !checked.Applied {
			if !checked.Accepted {
				return false, nil
			}
			return true, s.finalizeCommittedRun(ctx, run, false)
		}
		events, err := s.broker.Replay(ctx, c.RunID, 0)
		if err != nil {
			return false, fmt.Errorf("replay final output: %w", err)
		}
		for i := len(events) - 1; i >= 0; i-- {
			event := events[i]
			if event.Method != "values" || event.Generation != completion.Generation {
				continue
			}
			if err := json.Unmarshal(event.Data, &completion.Values); err != nil {
				return false, fmt.Errorf("decode final output: %w", err)
			}
			break
		}
	}
	result, err := s.store.CompleteRun(ctx, &completion)
	if err != nil {
		return false, err
	}
	if !result.Accepted {
		return false, nil
	}
	committed, err := s.store.GetRun(ctx, c.RunID)
	if err != nil {
		return false, err
	}
	return true, s.finalizeCommittedRun(ctx, committed, result.Applied)
}

// Finalization operations have durable idempotency keys. They run again after
// a lost response or a transient backend failure; only the first transaction
// winner increments process metrics and emits usage-shape alerts.
func (s *Server) finalizeCommittedRun(ctx context.Context, run *models.Run, applied bool) error {
	if applied {
		metrics.ActiveRuns.Dec()
		metrics.RunsTotal.WithLabelValues(run.AgentID, string(run.Status)).Inc()
		metrics.RunDuration.WithLabelValues(run.AgentID).Observe(time.Since(run.CreatedAt).Seconds())
	}
	if err := s.persistTerminalUsage(ctx, run, applied); err != nil {
		return err
	}
	if err := s.releaseUsageHold(ctx, run.RunID); err != nil {
		return err
	}
	s.maybeCancelInflightAfterTerminal(ctx, run)
	if run.Status == models.RunStatusSuccess && len(run.Output) > 0 {
		var values map[string]interface{}
		if err := json.Unmarshal(run.Output, &values); err != nil {
			return err
		}
		if err := s.maybeCacheRunResult(ctx, run, values); err != nil {
			return err
		}
	}
	if err := s.broker.Close(run.RunID); err != nil {
		return err
	}
	return s.finishRun(run.RunID, run.ThreadID, run.AgentID, run.TenantID, run.Status, run.Error)
}

// completeObservedRun shares the durable completion contract with wait callers
// that observe the terminal event before the runner's ReportStatus RPC.
func (s *Server) completeObservedRun(ctx context.Context, run *models.Run, terminal string, generation int64) (*models.Run, error) {
	latest, err := s.store.GetRun(ctx, run.RunID)
	if err != nil {
		return nil, err
	}
	if terminal != "" && !state.TerminalRunStatus(latest.Status) {
		if _, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: run.RunID, Generation: generation, Status: models.RunStatus(terminal)}); err != nil {
			return nil, err
		}
		return s.store.GetRun(ctx, run.RunID)
	}
	return latest, nil
}

// A stale terminal may close the event subscription while a newer attempt is
// active. Reconcile its durable result instead of returning the stale values or
// treating a closed subscription as successful completion.
func (s *Server) awaitDurableCompletion(ctx context.Context, run *models.Run) (*models.Run, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		latest, err := s.store.GetRun(ctx, run.RunID)
		if err != nil {
			return nil, err
		}
		if state.TerminalRunStatus(latest.Status) {
			return latest, nil
		}
		generation := state.MetadataGeneration(latest.Metadata, state.AttemptGenerationKey)
		events, err := s.broker.Replay(ctx, run.RunID, 0)
		if err != nil {
			return nil, err
		}
		for i := len(events) - 1; i >= 0; i-- {
			event := events[i]
			if !event.IsTerminal() || event.Generation != generation {
				continue
			}
			terminal := models.RunStatusError
			var data struct {
				Status  models.RunStatus `json:"status"`
				Message string           `json:"message"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return nil, err
			}
			if event.Method == "end" {
				terminal = data.Status
			}
			accepted, err := s.CompleteRun(ctx, &state.RunCompletion{RunID: run.RunID, Generation: generation, Status: terminal, Error: data.Message})
			if err != nil {
				return nil, err
			}
			if accepted {
				return s.store.GetRun(ctx, run.RunID)
			}
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
