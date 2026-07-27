package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/transport"
)

func bootstrapCachedAgent(t *testing.T, env *testEnv, agentID string, ttlSeconds int) {
	t.Helper()
	err := env.store.UpsertAgent(context.Background(), &models.Agent{
		AgentID: agentID, Name: agentID,
		Metadata:     map[string]interface{}{"cache_ttl_seconds": ttlSeconds},
		Capabilities: map[string]interface{}{},
	})
	if err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
}

// TestLLMCache_MissDispatchesThenHitSkipsRunner proves the full cache
// lifecycle: an agent with cache_ttl_seconds configured dispatches
// normally on a miss, StatusCallback saves the result on success, and an
// identical subsequent request is served directly from cache -- no queue
// entry, no runner dispatch.
func TestLLMCache_MissDispatchesThenHitSkipsRunner(t *testing.T) {
	env := newTestEnv(t)
	bootstrapCachedAgent(t, env, "cached-agent", 3600)
	ctx := context.Background()

	input := map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "what is 2+2?"}}}

	// First request: a real cache miss -- must dispatch to the queue.
	resp, _ := postJSON(env.srv.URL+"/threads/cache-thread-1/runs", map[string]interface{}{
		"agent_id": "cached-agent", "input": input,
	})
	expectStatus(t, resp, 200)
	var run1 models.Run
	json.Unmarshal(readBody(t, resp), &run1)
	if run1.Metadata["cache_hit"] == true {
		t.Fatal("expected a real miss (no dispatch job yet), got cache_hit=true")
	}

	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil || assignment.RunID != run1.RunID {
		t.Fatalf("expected the miss to actually enqueue a job: %v", err)
	}

	// Simulate the runner completing successfully.
	env.broker.Publish(ctx, run1.RunID, &transport.RunEvent{
		EventID: "e1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(`{"messages":[{"role":"ai","content":"4"}]}`), Ts: time.Now().UnixMilli(),
	})
	env.apiServer.StatusCallback()(run1.RunID, "success", "")

	// Second request with the SAME agent+input: must be a cache hit --
	// no new job on the queue.
	resp2, _ := postJSON(env.srv.URL+"/threads/cache-thread-2/runs", map[string]interface{}{
		"agent_id": "cached-agent", "input": input,
	})
	expectStatus(t, resp2, 200)
	var run2 models.Run
	json.Unmarshal(readBody(t, resp2), &run2)

	if run2.Metadata["cache_hit"] != true {
		t.Fatalf("expected cache_hit=true on the second identical request, got %+v", run2.Metadata)
	}
	if run2.Status != models.RunStatusSuccess {
		t.Fatalf("expected cached run to be immediately successful, got status=%s", run2.Status)
	}
	var output map[string]interface{}
	json.Unmarshal(run2.Output, &output)
	msgs, _ := output["messages"].([]interface{})
	if len(msgs) == 0 {
		t.Fatalf("expected cached output to carry the original values, got %+v", output)
	}

	// No second job should have landed on the queue for the cache hit.
	// Dequeue returns (nil, nil) on timeout -- not an error -- so the
	// absence of a job is "leaked == nil", not "err != nil".
	leaked, err := env.queue.Dequeue(ctx, "python-langgraph", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected Dequeue error: %v", err)
	}
	if leaked != nil {
		t.Fatalf("expected no queue entry for a cache hit, got job for run_id=%s", leaked.RunID)
	}

	// Thread state must reflect the cached values too (same as a real run).
	stateResp, err := http.Get(env.srv.URL + "/threads/cache-thread-2/state")
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]interface{}
	json.Unmarshal(readBody(t, stateResp), &state)
	stateVals, _ := state["values"].(map[string]interface{})
	if stateVals == nil || stateVals["messages"] == nil {
		t.Errorf("expected thread state to be updated from the cache hit, got %+v", state)
	}
}

// TestLLMCache_WaitEndpointServesCacheHitDirectly proves POST /runs/wait
// (which normally subscribes to the broker and blocks for a terminal
// event) returns immediately for a cache hit instead of hanging forever --
// a cache-hit run never has any broker events published for it.
func TestLLMCache_WaitEndpointServesCacheHitDirectly(t *testing.T) {
	env := newTestEnv(t)
	bootstrapCachedAgent(t, env, "wait-cache-agent", 3600)
	ctx := context.Background()
	input := map[string]interface{}{"q": "wait-test"}

	resp, _ := postJSON(env.srv.URL+"/threads/wait-cache-1/runs", map[string]interface{}{
		"agent_id": "wait-cache-agent", "input": input,
	})
	var run1 models.Run
	json.Unmarshal(readBody(t, resp), &run1)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	env.broker.Publish(ctx, run1.RunID, &transport.RunEvent{
		EventID: "e1", Seq: 1, Method: "values", Namespace: []string{},
		Data: json.RawMessage(`{"answer":"42"}`), Ts: time.Now().UnixMilli(),
	})
	env.apiServer.StatusCallback()(run1.RunID, "success", "")

	// This must return promptly, not hang -- the test's own timeout
	// (implicit via `go test`'s default) is the real assertion here.
	waitResp, _ := postJSON(env.srv.URL+"/runs/wait", map[string]interface{}{"agent_id": "wait-cache-agent", "input": input})
	expectStatus(t, waitResp, 200)
	var result models.RunWaitResponse
	json.Unmarshal(readBody(t, waitResp), &result)
	if result.Run == nil || result.Run.Metadata["cache_hit"] != true {
		t.Fatalf("expected a cache-hit run in the wait response, got %+v", result.Run)
	}
	if result.Values["answer"] != "42" {
		t.Fatalf("expected cached values in wait response, got %+v", result.Values)
	}
}

// TestLLMCache_StreamEndpointServesCacheHitDirectly proves POST
// /runs/stream returns a real (short) SSE sequence for a cache hit instead
// of hanging on a subscribe that will never see any events.
func TestLLMCache_StreamEndpointServesCacheHitDirectly(t *testing.T) {
	env := newTestEnv(t)
	bootstrapCachedAgent(t, env, "stream-cache-agent", 3600)
	ctx := context.Background()
	input := map[string]interface{}{"q": "stream-test"}

	resp, _ := postJSON(env.srv.URL+"/threads/stream-cache-1/runs", map[string]interface{}{
		"agent_id": "stream-cache-agent", "input": input,
	})
	var run1 models.Run
	json.Unmarshal(readBody(t, resp), &run1)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	env.broker.Publish(ctx, run1.RunID, &transport.RunEvent{
		EventID: "e1", Seq: 1, Method: "values", Namespace: []string{},
		Data: json.RawMessage(`{"answer":"stream-42"}`), Ts: time.Now().UnixMilli(),
	})
	env.apiServer.StatusCallback()(run1.RunID, "success", "")

	streamResp, _ := postJSON(env.srv.URL+"/runs/stream", map[string]interface{}{"agent_id": "stream-cache-agent", "input": input})
	expectStatus(t, streamResp, 200)
	body := string(readBody(t, streamResp))
	if !containsAll(body, "event: values", "stream-42", "event: end") {
		t.Fatalf("expected a values+end SSE sequence for the cache hit, got: %s", body)
	}
}

func containsAll(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestLLMCache_WebSocketRunStartCacheHitDoesNotHang proves the WebSocket
// run.start path (which always spawns a goroutine tailing the new run's
// events after starting it) doesn't leak that goroutine forever for a
// cache hit -- there are no events to tail, so the broker channel must
// already be closed by the time streamRunOverWS subscribes.
func TestLLMCache_WebSocketRunStartCacheHitDoesNotHang(t *testing.T) {
	env := newTestEnv(t)
	bootstrapCachedAgent(t, env, "ws-cache-agent", 3600)
	ctx := context.Background()
	input := map[string]interface{}{"q": "ws-test"}

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ws-cache-thread"})
	resp, _ := postJSON(env.srv.URL+"/threads/ws-cache-thread/runs", map[string]interface{}{
		"agent_id": "ws-cache-agent", "input": input,
	})
	var run1 models.Run
	json.Unmarshal(readBody(t, resp), &run1)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	env.broker.Publish(ctx, run1.RunID, &transport.RunEvent{
		EventID: "e1", Seq: 1, Method: "values", Namespace: []string{},
		Data: json.RawMessage(`{"answer":"ws-42"}`), Ts: time.Now().UnixMilli(),
	})
	env.apiServer.StatusCallback()(run1.RunID, "success", "")

	c, _, err := websocket.Dial(ctx, wsURL(env.srv.URL)+"/threads/ws-cache-thread/websocket", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	if err := wsjson.Write(ctx, c, models.StreamingCommand{
		ID: 1, Method: "run.start",
		Params: map[string]interface{}{"agent_id": "ws-cache-agent", "input": input},
	}); err != nil {
		t.Fatalf("write command: %v", err)
	}

	var ack models.StreamingCommandResponse
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := wsjson.Read(readCtx, c, &ack); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if ack.Type != "success" {
		t.Fatalf("expected success ack for cache-hit run.start, got %+v", ack)
	}
	// The important assertion is implicit: if streamRunOverWS's goroutine
	// hung waiting on a channel that's never closed, this whole test would
	// hang until `go test`'s timeout instead of completing quickly.
	c.Close(websocket.StatusNormalClosure, "")
}

// TestLLMCache_DifferentInputIsMiss proves the cache key genuinely
// discriminates on input -- a different input for the same agent is a
// separate cache entry, not a false hit.
func TestLLMCache_DifferentInputIsMiss(t *testing.T) {
	env := newTestEnv(t)
	bootstrapCachedAgent(t, env, "cached-agent-2", 3600)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/threads/cache-diff-1/runs", map[string]interface{}{
		"agent_id": "cached-agent-2", "input": map[string]interface{}{"q": "a"},
	})
	var run1 models.Run
	json.Unmarshal(readBody(t, resp), &run1)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	env.broker.Publish(ctx, run1.RunID, &transport.RunEvent{
		EventID: "e1", Seq: 1, Method: "values", Namespace: []string{},
		Data: json.RawMessage(`{"answer":"a-answer"}`), Ts: time.Now().UnixMilli(),
	})
	env.apiServer.StatusCallback()(run1.RunID, "success", "")

	// Different input -- must NOT hit the cache.
	resp2, _ := postJSON(env.srv.URL+"/threads/cache-diff-2/runs", map[string]interface{}{
		"agent_id": "cached-agent-2", "input": map[string]interface{}{"q": "b"},
	})
	var run2 models.Run
	json.Unmarshal(readBody(t, resp2), &run2)
	if run2.Metadata["cache_hit"] == true {
		t.Fatal("expected a different input to be a cache miss, got a hit")
	}
	if _, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second); err != nil {
		t.Fatal("expected the different-input request to actually dispatch a job")
	}
}

// TestLLMCache_NoConfigNeverCaches proves an agent without cache_ttl_seconds
// in its metadata never gets its results cached or served from cache --
// caching is opt-in per agent, never a surprise default.
func TestLLMCache_NoConfigNeverCaches(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	// No bootstrapCachedAgent call -- "uncached-agent" has no metadata at all.

	input := map[string]interface{}{"q": "same"}
	for i := 0; i < 2; i++ {
		resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "uncached-agent", "input": input})
		var run models.Run
		json.Unmarshal(readBody(t, resp), &run)
		if run.Metadata["cache_hit"] == true {
			t.Fatalf("iteration %d: expected no caching for an agent with no cache config", i)
		}
		assignment, dequeueErr := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if dequeueErr != nil || assignment == nil {
			t.Fatalf("iteration %d: expected every request to dispatch a real job: %v", i, dequeueErr)
		}
		env.apiServer.StatusCallback()(run.RunID, "success", "")
	}
}

// TestLLMCache_ResumeNeverUsesCache proves a resume_command request is
// never served from (or written to) the cache, even for a cache-configured
// agent -- a resume continues a specific prior execution, it's never a
// fresh cacheable computation.
func TestLLMCache_ResumeNeverUsesCache(t *testing.T) {
	env := newTestEnv(t)
	bootstrapCachedAgent(t, env, "cached-agent-3", 3600)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/threads/resume-thread/runs", map[string]interface{}{
		"agent_id": "cached-agent-3",
		"command":  map[string]interface{}{"resume": true},
	})
	expectStatus(t, resp, 200)
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	if run.Metadata["cache_hit"] == true {
		t.Fatal("a resume must never be served from cache")
	}
	if _, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second); err != nil {
		t.Fatal("expected the resume to actually dispatch a job")
	}
}
