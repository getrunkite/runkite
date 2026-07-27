package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/runkite/runkite/internal/metrics"
	"github.com/runkite/runkite/internal/models"
	"github.com/runkite/runkite/internal/transport"
)

// POST /threads/{threadID}/stream -- Open SSE event stream
func (s *Server) handleOpenThreadStream(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	var req models.EventStreamRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Verify thread exists
	if _, err := s.store.GetThread(r.Context(), threadID); err != nil {
		handleStoreError(w, err)
		return
	}

	// Find active runs on this thread
	runs, err := s.store.SearchRuns(r.Context(), &models.RunSearchRequest{
		ThreadID: threadID,
		Limit:    10,
	})
	if err != nil {
		handleStoreError(w, err)
		return
	}

	// Find the most recent non-terminal run
	var activeRunID string
	for _, run := range runs {
		if !isTerminalStatus(run.Status) {
			activeRunID = run.RunID
			break
		}
	}

	if activeRunID == "" {
		// No active run -- return empty stream that waits for one
		// For now, return an error (full implementation would use ThreadEventSession)
		writeError(w, http.StatusNotFound, "no active run on this thread")
		return
	}

	// Subscribe FIRST so live events aren't missed, then replay history
	// (same pattern as streamExistingRun / waitExistingRun).
	eventCh, err := s.broker.Subscribe(r.Context(), activeRunID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to subscribe")
		return
	}

	metrics.ActiveSSEConnections.Inc()
	defer metrics.ActiveSSEConnections.Dec()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	channelSet := make(map[string]bool)
	for _, ch := range req.Channels {
		channelSet[ch] = true
	}

	sseEmit := func(event *transport.RunEvent, streamEvent *models.StreamingEvent) {
		eventJSON, _ := json.Marshal(streamEvent)
		fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", event.EventID, event.Method, string(eventJSON))
		flusher.Flush()
	}

	replayAndTail(r.Context(), s.broker, activeRunID, req.Since, channelSet, eventCh, sseEmit)
}

// replayAndTail is the transport-agnostic core of "replay history, then tail
// live events, converting each to a StreamingEvent, until a terminal event."
// Shared by SSE (handleOpenThreadStream) and WebSocket (handleThreadWebSocket)
// so the two transports can never drift in replay/dedup/channel-filter
// semantics -- only how a converted event gets put on the wire differs.
func replayAndTail(
	ctx context.Context,
	broker transport.EventBroker,
	runID string,
	since *int64,
	channelSet map[string]bool,
	eventCh <-chan *transport.RunEvent,
	emit func(event *transport.RunEvent, streamEvent *models.StreamingEvent),
) {
	seq := int64(0)
	toStreamEvent := func(event *transport.RunEvent) (*models.StreamingEvent, bool) {
		method := event.Method
		if method != "lifecycle" && method != "end" && method != "error" {
			if len(channelSet) > 0 && !channelSet[method] && !channelSet["*"] {
				return nil, false
			}
		}
		seq++
		return &models.StreamingEvent{
			Type:    "event",
			EventID: event.EventID,
			Seq:     seq,
			Method:  method,
			Params: map[string]interface{}{
				"namespace": event.Namespace,
				"timestamp": event.Ts,
				"data":      json.RawMessage(event.Data),
			},
		}, true
	}

	var maxReplayedSeq int64
	if past, replayErr := broker.Replay(ctx, runID, 0); replayErr == nil {
		for _, event := range past {
			if since != nil && event.Seq <= *since {
				continue
			}
			if event.Seq > maxReplayedSeq {
				maxReplayedSeq = event.Seq
			}
			if se, ok := toStreamEvent(event); ok {
				emit(event, se)
				if event.IsTerminal() {
					return
				}
			}
		}
	}

	for event := range eventCh {
		if event.Seq <= maxReplayedSeq {
			continue
		}
		if since != nil && event.Seq <= *since {
			continue
		}
		if se, ok := toStreamEvent(event); ok {
			emit(event, se)
			if event.IsTerminal() {
				return
			}
		}
	}
}

// POST /threads/{threadID}/commands -- Send streaming command
func (s *Server) handleThreadCommand(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	var cmd models.StreamingCommand
	if err := readJSON(r, &cmd); err != nil {
		writeError(w, http.StatusBadRequest, "invalid command")
		return
	}

	// Verify thread exists
	if _, err := s.store.GetThread(r.Context(), threadID); err != nil {
		handleStoreError(w, err)
		return
	}

	// Handle command based on method
	switch cmd.Method {
	case "run.start":
		run, err := s.runStartCommandCore(r, threadID, &cmd)
		writeCommandResult(w, cmd.ID, "run_start_failed", run, err)
	case "input.respond":
		run, err := s.inputRespondCommandCore(r, threadID, &cmd)
		writeCommandResult(w, cmd.ID, "resume_failed", run, err)
	default:
		writeJSON(w, http.StatusOK, models.StreamingCommandResponse{
			Type:    "error",
			ID:      cmd.ID,
			Error:   "unknown_method",
			Message: fmt.Sprintf("unknown command method: %s", cmd.Method),
		})
	}
}

// writeCommandResult formats a StreamingCommandResponse for the REST
// /threads/{id}/commands endpoint from a (run, err) pair. Shared shape with
// the WebSocket command dispatcher (see websocket.go), which formats the
// same core-function results as WS frames instead of an HTTP response body.
func writeCommandResult(w http.ResponseWriter, id int, failErrCode string, run *models.Run, err error) {
	if err != nil {
		writeJSON(w, http.StatusOK, models.StreamingCommandResponse{
			Type:    "error",
			ID:      id,
			Error:   failErrCode,
			Message: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, models.StreamingCommandResponse{
		Type: "success",
		ID:   id,
		Result: map[string]interface{}{
			"run_id": run.RunID,
		},
	})
}

// runStartCommandCore creates and enqueues a run from a "run.start" command's
// params. HTTP-independent so both the REST commands endpoint and the
// WebSocket handler can share it.
func (s *Server) runStartCommandCore(r *http.Request, threadID string, cmd *models.StreamingCommand) (*models.Run, error) {
	agentID, _ := cmd.Params["agent_id"].(string)
	input, _ := json.Marshal(cmd.Params["input"])

	req := &models.RunCreate{
		AgentID: agentID,
		Input:   input,
	}

	run, assignment, err := s.createRun(r, threadID, req)
	if err != nil {
		return nil, err
	}
	if err := s.enqueue(r.Context(), assignment); err != nil {
		return nil, err
	}
	return run, nil
}

// inputRespondCommandCore creates a resume run from an "input.respond"
// command's params. HTTP-independent, same sharing rationale as above.
func (s *Server) inputRespondCommandCore(r *http.Request, threadID string, cmd *models.StreamingCommand) (*models.Run, error) {
	response := cmd.Params["response"]
	responseJSON, _ := json.Marshal(response)

	// Find the agent_id from the interrupted run
	runs, _ := s.store.SearchRuns(r.Context(), &models.RunSearchRequest{
		ThreadID: threadID,
		Limit:    1,
	})
	agentID := ""
	if len(runs) > 0 {
		agentID = runs[0].AgentID
	}

	req := &models.RunCreate{
		AgentID:       agentID,
		ResumeCommand: responseJSON,
	}

	run, assignment, err := s.createRun(r, threadID, req)
	if err != nil {
		return nil, err
	}
	if err := s.enqueue(r.Context(), assignment); err != nil {
		return nil, err
	}
	return run, nil
}
