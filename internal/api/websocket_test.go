package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/transport"
)

func wsURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

// TestWebSocket_RunStartStreamsEventsOnSameConnection proves the master
// plan's "one connection for both commands and events" claim: a single
// WebSocket carries the run.start command, its ack, and every RunEvent for
// the run it started, with no separate SSE request needed.
func TestWebSocket_RunStartStreamsEventsOnSameConnection(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ws-thread-1"})

	c, _, err := websocket.Dial(ctx, wsURL(env.srv.URL)+"/threads/ws-thread-1/websocket", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if err := wsjson.Write(ctx, c, models.StreamingCommand{
		ID:     1,
		Method: "run.start",
		Params: map[string]interface{}{"agent_id": "test", "input": map[string]interface{}{"x": 1}},
	}); err != nil {
		t.Fatalf("write command: %v", err)
	}

	// Ack must arrive first, with a run_id.
	var ack models.StreamingCommandResponse
	if err := wsjson.Read(ctx, c, &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Type != "success" {
		t.Fatalf("expected success ack, got %+v", ack)
	}
	runID, _ := ack.Result["run_id"].(string)
	if runID == "" {
		t.Fatal("ack missing run_id")
	}

	// Simulate the runner: dequeue the job it was assigned, then publish events.
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil || assignment.RunID != runID {
		t.Fatalf("expected queued job for run %s: %v", runID, err)
	}
	env.broker.Publish(ctx, runID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(`{"step":1}`), Ts: time.Now().UnixMilli(),
	})
	env.broker.Publish(ctx, runID, &transport.RunEvent{
		EventID: "evt_2", Seq: 2, Method: "end",
		Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
	})

	// Same connection must now deliver both events, in order, without a
	// second request of any kind.
	var got []models.StreamingEvent
	for len(got) < 2 {
		var se models.StreamingEvent
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := wsjson.Read(readCtx, c, &se)
		cancel()
		if err != nil {
			t.Fatalf("read event %d: %v", len(got), err)
		}
		got = append(got, se)
	}

	if got[0].Method != "values" {
		t.Fatalf("expected first event method=values, got %+v", got[0])
	}
	if got[1].Method != "end" {
		t.Fatalf("expected second event method=end, got %+v", got[1])
	}

	c.Close(websocket.StatusNormalClosure, "")
}

// TestWebSocket_RunCancel proves "run.cancel" over the same connection
// actually cancels the run (queue.Cancel + status flips to interrupted),
// matching the REST cancel endpoint's behavior.
func TestWebSocket_RunCancel(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ws-thread-2"})

	c, _, err := websocket.Dial(ctx, wsURL(env.srv.URL)+"/threads/ws-thread-2/websocket", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	wsjson.Write(ctx, c, models.StreamingCommand{
		ID: 1, Method: "run.start",
		Params: map[string]interface{}{"agent_id": "test"},
	})
	var ack models.StreamingCommandResponse
	wsjson.Read(ctx, c, &ack)
	runID, _ := ack.Result["run_id"].(string)
	if runID == "" {
		t.Fatal("ack missing run_id")
	}
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)

	if err := wsjson.Write(ctx, c, models.StreamingCommand{
		ID: 2, Method: "run.cancel",
		Params: map[string]interface{}{"run_id": runID},
	}); err != nil {
		t.Fatalf("write cancel: %v", err)
	}

	var cancelAck models.StreamingCommandResponse
	if err := wsjson.Read(ctx, c, &cancelAck); err != nil {
		t.Fatalf("read cancel ack: %v", err)
	}
	if cancelAck.Type != "success" {
		t.Fatalf("expected success cancel ack, got %+v", cancelAck)
	}

	run, err := env.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != models.RunStatusInterrupted {
		t.Fatalf("expected run status interrupted after run.cancel, got %s", run.Status)
	}

	c.Close(websocket.StatusNormalClosure, "")
}

// TestWebSocket_UnknownMethod proves unrecognized commands get an error ack
// without killing the connection (client can keep sending other commands).
func TestWebSocket_UnknownMethod(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ws-thread-3"})

	c, _, err := websocket.Dial(ctx, wsURL(env.srv.URL)+"/threads/ws-thread-3/websocket", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	wsjson.Write(ctx, c, models.StreamingCommand{ID: 1, Method: "bogus.method"})

	var resp models.StreamingCommandResponse
	if err := wsjson.Read(ctx, c, &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	if resp.Type != "error" || resp.Error != "unknown_method" {
		t.Fatalf("expected unknown_method error, got %+v", resp)
	}

	// Connection must still be usable afterwards.
	wsjson.Write(ctx, c, models.StreamingCommand{
		ID: 2, Method: "run.start", Params: map[string]interface{}{"agent_id": "test"},
	})
	var ack models.StreamingCommandResponse
	if err := wsjson.Read(ctx, c, &ack); err != nil {
		t.Fatalf("read second ack: %v", err)
	}
	if ack.Type != "success" {
		t.Fatalf("expected connection to survive unknown method, got %+v", ack)
	}

	c.Close(websocket.StatusNormalClosure, "")
}

// TestWebSocket_ThreadNotFound proves connecting to a nonexistent thread is
// rejected at handshake time (HTTP 404), not silently accepted.
func TestWebSocket_ThreadNotFound(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	_, _, err := websocket.Dial(ctx, wsURL(env.srv.URL)+"/threads/does-not-exist/websocket", nil)
	if err == nil {
		t.Fatal("expected dial to fail for nonexistent thread")
	}
}
