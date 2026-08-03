package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/getrunkite/runkite/internal/metrics"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/transport"
)

// GET /threads/{threadID}/websocket -- bidirectional streaming: one
// connection for both commands and events, for the chatbot use case.
//
// Client -> server frames are the same StreamingCommand JSON already used by
// POST /threads/{id}/commands ("run.start", "input.respond", "run.cancel").
// Server -> client frames are the same StreamingEvent JSON already used by
// the SSE stream. Reusing both shapes means one client-side parser works for
// SSE and WebSocket, and replayAndTail (streaming.go) guarantees identical
// replay/dedup semantics on both transports.
//
// coder/websocket's Conn permits concurrent calls to every method except
// Read/Reader, so each command's resulting run is streamed on its own
// goroutine writing directly to the connection -- no output-channel/single-
// writer plumbing needed, unlike most other WS libraries.
func (s *Server) handleThreadWebSocket(w http.ResponseWriter, r *http.Request) {
	threadID := r.PathValue("threadID")

	if _, err := s.store.GetThread(r.Context(), threadID); err != nil {
		handleStoreError(w, err)
		return
	}

	// When cors.allow_origins is configured, reuse it as OriginPatterns so
	// a browser cannot open this socket from an unlisted origin. Empty /
	// unset keeps coder/websocket's default (any Origin) -- fine for
	// server-to-server and same-origin Admin UI; token auth still applies.
	var acceptOpts *websocket.AcceptOptions
	if len(s.wsOriginPatterns) > 0 {
		acceptOpts = &websocket.AcceptOptions{OriginPatterns: s.wsOriginPatterns}
	}
	c, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		c.CloseNow()
	}()

	metrics.ActiveSSEConnections.Inc() // same "live streaming connection" signal SSE uses
	defer metrics.ActiveSSEConnections.Dec()

	req := r.WithContext(ctx)
	for {
		var cmd models.StreamingCommand
		if err := wsjson.Read(ctx, c, &cmd); err != nil {
			return // client disconnected, or ctx cancelled
		}
		s.dispatchWSCommand(ctx, req, c, threadID, &cmd, &wg)
	}
}

func (s *Server) dispatchWSCommand(ctx context.Context, r *http.Request, c *websocket.Conn, threadID string, cmd *models.StreamingCommand, wg *sync.WaitGroup) {
	switch cmd.Method {
	case "run.start":
		run, err := s.runStartCommandCore(r, threadID, cmd)
		if !wsWriteCommandResult(ctx, c, cmd.ID, "run_start_failed", run, err) {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.streamRunOverWS(ctx, c, run.RunID)
		}()

	case "input.respond":
		run, err := s.inputRespondCommandCore(r, threadID, cmd)
		if !wsWriteCommandResult(ctx, c, cmd.ID, "resume_failed", run, err) {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.streamRunOverWS(ctx, c, run.RunID)
		}()

	case "run.cancel":
		runID, _ := cmd.Params["run_id"].(string)
		// wait/rollback aren't wired into the WS command body -- always
		// today's original behavior (immediate response, never deletes
		// the run). Matches the REST handler's own defaults; extending
		// the WS command shape to accept these is a reasonable follow-up.
		run, err := s.cancelRunCore(ctx, runID, false, false)
		wsWriteCommandResult(ctx, c, cmd.ID, "cancel_failed", run, err)

	default:
		_ = wsjson.Write(ctx, c, models.StreamingCommandResponse{
			Type:    "error",
			ID:      cmd.ID,
			Error:   "unknown_method",
			Message: "unknown command method: " + cmd.Method,
		})
	}
}

// wsWriteCommandResult writes the ack for a dispatched command and reports
// whether it succeeded (false on error or a dead connection, so callers know
// not to bother starting an event-streaming goroutine for it).
func wsWriteCommandResult(ctx context.Context, c *websocket.Conn, id int, failErrCode string, run *models.Run, err error) bool {
	if err != nil {
		_ = wsjson.Write(ctx, c, models.StreamingCommandResponse{
			Type: "error", ID: id, Error: failErrCode, Message: err.Error(),
		})
		return false
	}
	writeErr := wsjson.Write(ctx, c, models.StreamingCommandResponse{
		Type: "success", ID: id, Result: map[string]interface{}{"run_id": run.RunID},
	})
	return writeErr == nil
}

// streamRunOverWS subscribes+replays a run's events onto the WebSocket
// connection, same semantics as the SSE stream (replayAndTail).
func (s *Server) streamRunOverWS(ctx context.Context, c *websocket.Conn, runID string) {
	eventCh, err := s.broker.Subscribe(ctx, runID)
	if err != nil {
		slog.Error("websocket: failed to subscribe to run", "run_id", runID, "error", err)
		return
	}
	replayAndTail(ctx, s.broker, runID, nil, nil, eventCh, func(_ *transport.RunEvent, se *models.StreamingEvent) {
		if err := wsjson.Write(ctx, c, se); err != nil {
			// Write failed (client gone) -- nothing more to do; the read
			// loop will notice on its own and cancel ctx, unblocking Replay/
			// Subscribe's context-aware waits for any other in-flight runs.
			return
		}
	})
}
