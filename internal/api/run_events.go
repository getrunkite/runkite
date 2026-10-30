package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/transport"
)

// replayRunEvents keeps a live client attached when a superseded terminal closes
// the broker subscription. Subscribe must happen before enqueue or replay. The
// raw broker remains available to completion recovery; filtering it there would
// recursively invoke CompleteRun.
func (s *Server) replayRunEvents(
	ctx context.Context,
	runID string,
	since *int64,
	eventCh <-chan *transport.RunEvent,
	emit func(*transport.RunEvent) error,
) error {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	generation := state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey)
	cursorGeneration := generation
	if _, ok := run.Metadata[state.AttemptGenerationKey]; !ok {
		cursorGeneration = -1
	}
	seen := make(map[int64]int64)
	controlEvents := make(map[string]bool)
	isPolicyEvent := func(event *transport.RunEvent) bool {
		return event.Method == "tool_auth" && strings.HasPrefix(event.EventID, runID+"_tool_auth_")
	}
	marked := func(event *transport.RunEvent) bool {
		if event.Seq > 0 && !isPolicyEvent(event) {
			seq, ok := seen[event.Generation]
			return ok && event.Seq <= seq
		}
		return event.EventID != "" && controlEvents[event.EventID]
	}
	mark := func(event *transport.RunEvent) {
		if event.Seq > 0 && !isPolicyEvent(event) {
			if event.Seq > seen[event.Generation] {
				seen[event.Generation] = event.Seq
			}
		} else if event.EventID != "" {
			controlEvents[event.EventID] = true
		}
	}
	refresh := func() error {
		latest, err := s.store.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		run = latest
		if current := state.MetadataGeneration(run.Metadata, state.AttemptGenerationKey); current > generation {
			generation = current
		}
		return nil
	}
	durableEnd := func() error {
		data, _ := json.Marshal(map[string]string{"status": string(run.Status)})
		return emit(&transport.RunEvent{
			Generation: state.MetadataGeneration(run.Metadata, state.CompletionGenerationKey),
			EventID:    runID + "_end", Method: "end", Data: data,
		})
	}
	// A failed completion must not consume its terminal cursor. Replaying it
	// retries the existing idempotent transaction and its finalization steps.
	consume := func(event *transport.RunEvent) (done bool, err error) {
		if event == nil || marked(event) {
			return false, nil
		}
		// Older control-plane policy notices did not stamp the envelope's
		// generation. Keep these nonterminal notices without admitting legacy
		// runner terminals or advancing a runner's independent sequence cursor.
		legacyPolicy := event.Generation == 0 && isPolicyEvent(event)
		if event.Generation < generation && !legacyPolicy {
			mark(event)
			return false, nil
		}
		if cursorGeneration < 0 {
			cursorGeneration = event.Generation
		}
		if since != nil && event.Generation == cursorGeneration && event.Seq <= *since && !event.IsTerminal() {
			mark(event)
			return false, nil
		}
		if event.IsTerminal() {
			terminal := models.RunStatusError
			var data struct {
				Status  models.RunStatus `json:"status"`
				Message string           `json:"message"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				mark(event)
				return false, nil
			}
			if event.Method == "end" {
				terminal = data.Status
			}
			if !state.TerminalRunStatus(terminal) {
				mark(event)
				return false, nil
			}
			accepted, err := s.CompleteRun(ctx, &state.RunCompletion{
				RunID: runID, Generation: event.Generation, Status: terminal, Error: data.Message,
			})
			if err != nil {
				return false, err
			}
			if !accepted {
				if err := refresh(); err != nil {
					return false, err
				}
				mark(event)
				return false, nil
			}
			if err := emit(event); err != nil {
				return true, err
			}
			return true, nil
		}
		if event.Generation > generation && !isPolicyEvent(event) {
			generation = event.Generation
		}
		if err := emit(event); err != nil {
			return true, err
		}
		mark(event)
		return false, nil
	}
	// Poll only after the push subscription closes or a completion needs retry.
	// A low-frequency state check also handles durable cancellation when a
	// compatible broker cannot signal Close to this subscriber.
	stateCheck := time.NewTicker(5 * time.Second)
	defer stateCheck.Stop()
	delay := 100 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		retry := false
		past, replayErr := s.broker.Replay(ctx, runID, 0)
		if replayErr != nil {
			// Completed records remain readable when retained events expire or
			// the broker is unavailable. Never substitute a stale raw terminal.
			if err := refresh(); err == nil && state.TerminalRunStatus(run.Status) {
				return durableEnd()
			}
			retry = true
		} else {
			for _, event := range past {
				done, err := consume(event)
				if done {
					return err
				}
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					slog.Warn("retrying streamed run completion", "run_id", runID, "error", err)
					retry = true
					break
				}
			}
		}
		if !retry {
			if err := refresh(); err != nil {
				retry = true
			} else if state.TerminalRunStatus(run.Status) {
				// Cancellation, timeout or expired replay may leave no matching
				// runner terminal. Finish with the persisted status, never a stale one.
				return durableEnd()
			}
		}
		if eventCh == nil || retry {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			if delay < time.Second {
				delay *= 2
				if delay > time.Second {
					delay = time.Second
				}
			}
			continue
		}
		for eventCh != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-stateCheck.C:
				if err := refresh(); err != nil || state.TerminalRunStatus(run.Status) {
					// Drain replay before considering a synthetic durable end.
					goto replay
				}
			case event, ok := <-eventCh:
				if !ok {
					eventCh = nil
					goto replay
				}
				done, err := consume(event)
				if done {
					return err
				}
				if err != nil {
					// The broker may already have closed on this terminal. Its
					// retained replay is the retry source; do not advance the cursor.
					if ctx.Err() != nil {
						return ctx.Err()
					}
					eventCh = nil
					goto replay
				}
			}
		}
	replay:
		delay = 100 * time.Millisecond
	}
}

func writeRunSSE(w interface{ Write([]byte) (int, error) }, event *transport.RunEvent) error {
	_, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.EventID, event.Method, event.Data)
	return err
}
