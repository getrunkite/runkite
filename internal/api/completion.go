package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"time"

	"github.com/getrunkite/runkite/internal/auth"
	"github.com/getrunkite/runkite/internal/jsonvalue"
	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
	"github.com/getrunkite/runkite/internal/transport"
)

// BeginRunAttempt is called before a dequeued assignment is delivered. The
// durable fence makes a completion from a superseded dispatch harmless even
// when reclaim races a SQL write or a runner reconnects to another replica.
func (s *Server) BeginRunAttempt(ctx context.Context, id, incarnation string, generation int64) (bool, error) {
	// Recover a completed event stream before delivering a reclaimed job.
	// This resends no model/tool work merely because ReportStatus was interrupted.
	run, err := s.store.GetRun(tenant.SystemContext(ctx), id)
	if err != nil {
		return false, err
	}
	if !auth.ValidRunIncarnation(incarnation) || state.RunIdentity(run) != incarnation {
		return false, nil
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
	return s.store.BeginRunAttempt(tenant.SystemContext(ctx), id, incarnation, generation)
}

// recoverDeliveredCompletion preserves a delivered agent outcome when the
// final stream acknowledgement or ReportStatus is lost. Reclaim also uses it
// at the retry ceiling, where there will be no next GetJob to recover it.
func (s *Server) recoverDeliveredCompletion(ctx context.Context, run *models.Run) (bool, error) {
	generation := state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey)
	events, err := s.broker.Replay(ctx, run.RunID, state.RunIdentity(run), 0)
	if err != nil {
		return false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if !event.IsTerminal() || event.Generation != generation || event.RunIncarnationID != state.RunIdentity(run) {
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
		return s.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, RunID: run.RunID, ExpectedIncarnationID: state.RunIdentity(run), Generation: generation, Status: status, Error: data.Message})
	}
	return false, nil
}

// CompleteReclaimedRun finalizes an exhausted assignment before the reaper
// removes its retained lease. Backend and finalization failures are retried by
// the next reclaim tick, including after a control-plane process restarts.
// A true result also permits disposal of a definitively retired physical row.
// That path performs no completion or finalization on a replacement record.
func (s *Server) CompleteReclaimedRun(ctx context.Context, pill transport.PoisonPill, errorMsg string) (bool, error) {
	if pill.Generation <= 0 {
		return false, fmt.Errorf("invalid poison-pill generation")
	}
	generation := pill.Generation - 1
	assignment, err := s.queue.LookupInflight(ctx, pill.RunID)
	if err != nil {
		return false, err
	}
	if assignment == nil || !auth.ValidRunIncarnation(pill.RunIncarnationID) || assignment.RunIncarnationID != pill.RunIncarnationID || assignment.Generation != generation {
		return false, nil
	}
	run, err := s.store.GetRun(tenant.SystemContext(ctx), pill.RunID)
	if err != nil {
		var missing *state.ErrNotFound
		if errors.As(err, &missing) {
			return true, nil
		}
		return false, err
	}
	if state.RunIdentity(run) != pill.RunIncarnationID {
		// The queue's subsequent Ack still compares the captured old record and
		// generation atomically. Never finalize or bill the new physical row.
		return auth.ValidRunIncarnation(state.RunIdentity(run)), nil
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
		recovery := state.WithTrustedAttemptRecovery(transport.WithWorkloadIdentity(tenant.SystemContext(ctx), assignment.WorkloadOwner))
		accepted, err := s.store.BeginRunAttempt(recovery, pill.RunID, pill.RunIncarnationID, generation)
		if err != nil || !accepted {
			return false, err
		}
	}
	return s.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, RunID: pill.RunID, ExpectedIncarnationID: pill.RunIncarnationID, Generation: generation, Status: models.RunStatusError, Error: errorMsg})
}

// CompleteRun is retryable after either an RPC failure or a lost response. The
// state store commits the result before the bridge removes its queue lease.
func (s *Server) CompleteRun(ctx context.Context, c *state.RunCompletion) (bool, error) {
	result, err := s.completeRunResult(ctx, c)
	return result.Accepted, err
}

// completeRunResult retains the transaction's Applied flag so trusted sweeps
// count and report only their own transition while still retrying finalization.
func (s *Server) completeRunResult(ctx context.Context, c *state.RunCompletion) (state.CompletionResult, error) {
	if !auth.ValidRunIncarnation(c.ExpectedIncarnationID) {
		return state.CompletionResult{}, &state.ErrConflict{Resource: "run", ID: c.RunID, Reason: "original run record identity required"}
	}
	run, err := s.store.GetRun(tenant.SystemContext(ctx), c.RunID)
	if err != nil {
		return state.CompletionResult{}, err
	}
	if c.ExpectedIncarnationID != "" && c.ExpectedIncarnationID != state.RunIdentity(run) {
		return state.CompletionResult{}, &state.ErrConflict{Resource: "run", ID: c.RunID, Reason: "retained run incarnation changed"}
	}
	if err := state.CheckRunCompletionOwner(ctx, run.Metadata, c); err != nil {
		return state.CompletionResult{}, err
	}
	if c.RunnerKind != "" {
		var kind string
		if manifest, ok := run.Metadata["run_manifest"].(map[string]interface{}); ok {
			kind, _ = manifest["runner_kind"].(string)
		}
		if kind == "" {
			agent, lookupErr := s.store.GetAgent(tenant.WithContext(ctx, run.TenantID), run.AgentID)
			if lookupErr != nil {
				return state.CompletionResult{}, lookupErr
			}
			kind, _ = agent.Metadata["runner_kind"].(string)
			if kind == "" {
				kind = "python-langgraph"
			}
		}
		if kind != c.RunnerKind {
			return state.CompletionResult{}, fmt.Errorf("run belongs to another runner kind")
		}
	}
	ctx = tenant.WithContext(ctx, run.TenantID)
	// An absent queue lease permits only replay of an already committed result.
	// It must never let an unaffiliated worker finish a pending run by guessing id.
	if c.ReplayOnly {
		_, done := run.Metadata[state.CompletionGenerationKey]
		if !done || state.MetadataGeneration(run.Metadata, state.CompletionGenerationKey) != c.Generation || run.Status != c.Status {
			return state.CompletionResult{}, nil
		}
		// A lost acknowledgement can be retried after the queue lease is gone.
		// The captured read is insufficient when deletion and recreation race it;
		// check the echoed original identity under the same lock as completion.
		result, err := s.store.CompleteRun(ctx, c)
		if err != nil || !result.Accepted {
			return result, err
		}
		if result.Applied {
			return result, fmt.Errorf("completed-result replay unexpectedly applied a transition")
		}
		committed, err := s.getObservedRun(ctx, run)
		if err != nil {
			return result, err
		}
		return result, s.finalizeCommittedRun(ctx, committed, false)
	}
	if !state.TerminalRunStatus(c.Status) {
		return state.CompletionResult{}, fmt.Errorf("invalid terminal run status %q", c.Status)
	}
	completion := *c
	if completion.CancelCurrentAttempt {
		completion.Generation = state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey)
		completion.CancelUsage = nil
		// Cancellation must remain available when the broker is unavailable.
		// Capture only usage already delivered by this attempt. The store drops
		// it if reclaim advances the attempt before the cancellation commits.
		replayTimeout := 100 * time.Millisecond
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline)/4 < replayTimeout {
			replayTimeout = time.Until(deadline) / 4
		}
		replayCtx, cancelReplay := context.WithTimeout(ctx, replayTimeout)
		type replayResult struct {
			events []*transport.RunEvent
			err    error
		}
		replayed := make(chan replayResult, 1)
		go func() {
			events, err := s.broker.Replay(replayCtx, c.RunID, state.RunIdentity(run), 0)
			replayed <- replayResult{events: events, err: err}
		}()
		var result replayResult
		select {
		case result = <-replayed:
		case <-replayCtx.Done():
			// Some broker clients use their own socket timeout. Optional
			// accounting must not hold the durable cancellation behind it.
			result.err = replayCtx.Err()
		}
		cancelReplay()
		if result.err != nil {
			slog.Warn("could not capture delivered usage before cancellation", "run_id", c.RunID, "error", result.err)
		} else {
			for i := len(result.events) - 1; i >= 0; i-- {
				event := result.events[i]
				if event.Method != "values" || event.Generation != completion.Generation || event.RunIncarnationID != state.RunIdentity(run) {
					continue
				}
				var values struct {
					Usage map[string]interface{} `json:"usage"`
				}
				if err := jsonvalue.Unmarshal(event.Data, &values); err == nil && values.Usage != nil {
					completion.CancelUsage = values.Usage
					break
				}
			}
		}
	} else {
		if checked := state.CheckCompletion(run.Metadata, run.Status, &completion); !checked.Applied {
			if !checked.Accepted {
				return checked, nil
			}
			return checked, s.finalizeCommittedRun(ctx, run, false)
		}
		// A pending-only enqueue failure has no admitted worker output. Do not
		// make definite rejection recovery depend on a healthy event broker.
		if !completion.FailUnstarted {
			events, err := s.broker.Replay(ctx, c.RunID, state.RunIdentity(run), 0)
			if err != nil {
				return state.CompletionResult{}, fmt.Errorf("replay final output: %w", err)
			}
			for i := len(events) - 1; i >= 0; i-- {
				event := events[i]
				if event.Method != "values" || event.Generation != completion.Generation || event.RunIncarnationID != state.RunIdentity(run) {
					continue
				}
				if err := jsonvalue.Unmarshal(event.Data, &completion.Values); err != nil {
					return state.CompletionResult{}, fmt.Errorf("decode final output: %w", err)
				}
				break
			}
		}
	}
	result, err := s.store.CompleteRun(ctx, &completion)
	if err != nil {
		return state.CompletionResult{}, err
	}
	if !result.Accepted {
		return result, nil
	}
	committed, err := s.getObservedRun(ctx, run)
	if err != nil {
		return result, err
	}
	return result, s.finalizeCommittedRun(ctx, committed, result.Applied)
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
	if err := s.releaseUsageHold(ctx, run.RunID, state.RunIdentity(run)); err != nil {
		return err
	}
	s.maybeCancelInflightAfterTerminal(ctx, run)
	if run.Status == models.RunStatusSuccess && len(run.Output) > 0 {
		var values map[string]interface{}
		if err := jsonvalue.Unmarshal(run.Output, &values); err != nil {
			return err
		}
		if err := s.maybeCacheRunResult(ctx, run, values); err != nil {
			return err
		}
	}
	if err := s.broker.Close(run.RunID, state.RunIdentity(run)); err != nil {
		return err
	}
	return s.finishRun(run)
}

// completeObservedRun shares the durable completion contract with wait callers
// that observe the terminal event before the runner's ReportStatus RPC.
func (s *Server) completeObservedRun(ctx context.Context, run *models.Run, terminal string, generation int64) (*models.Run, error) {
	latest, err := s.getObservedRun(ctx, run)
	if err != nil {
		return nil, err
	}
	if terminal != "" && !state.TerminalRunStatus(latest.Status) {
		if _, err := s.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: state.RunIdentity(run), RunID: run.RunID, Generation: generation, Status: models.RunStatus(terminal)}); err != nil {
			return nil, err
		}
		return s.getObservedRun(ctx, run)
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
		latest, err := s.getObservedRun(ctx, run)
		if err != nil {
			return nil, err
		}
		if state.TerminalRunStatus(latest.Status) {
			return latest, nil
		}
		generation := state.MetadataGeneration(latest.Metadata, state.AttemptGenerationKey)
		events, err := s.broker.Replay(ctx, run.RunID, state.RunIdentity(run), 0)
		if err != nil {
			return nil, err
		}
		for i := len(events) - 1; i >= 0; i-- {
			event := events[i]
			if !event.IsTerminal() || event.Generation != generation || event.RunIncarnationID != state.RunIdentity(run) {
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
			accepted, err := s.CompleteRun(ctx, &state.RunCompletion{TrustedSystem: true, ExpectedIncarnationID: state.RunIdentity(run), RunID: run.RunID, Generation: generation, Status: terminal, Error: data.Message})
			if err != nil {
				return nil, err
			}
			if accepted {
				return s.getObservedRun(ctx, run)
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
