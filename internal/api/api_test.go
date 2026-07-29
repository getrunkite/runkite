package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/api"
	"github.com/sharanharsoor/runkite/internal/auth"
	"github.com/sharanharsoor/runkite/internal/connector"
	"github.com/sharanharsoor/runkite/internal/metrics"
	"github.com/sharanharsoor/runkite/internal/models"
	sqlitestore "github.com/sharanharsoor/runkite/internal/state/sqlite"
	"github.com/sharanharsoor/runkite/internal/transport"
	"github.com/sharanharsoor/runkite/internal/transport/inprocess"
)

// testEnv bundles a running httptest.Server with all its deps.
type testEnv struct {
	srv       *httptest.Server
	store     *sqlitestore.SQLiteStore
	queue     *inprocess.Queue
	broker    *inprocess.Broker
	cancel    *inprocess.CancelBus
	apiServer *api.Server
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	cancelBus := inprocess.NewCancelBus()

	apiServer := api.NewServer(store, queue, broker, cancelBus)
	srv := httptest.NewServer(apiServer.Handler())
	t.Cleanup(srv.Close)

	return &testEnv{srv: srv, store: store, queue: queue, broker: broker, cancel: cancelBus, apiServer: apiServer}
}

// --- JSON helpers ---

func postJSON(url string, body interface{}) (*http.Response, error) {
	b, _ := json.Marshal(body)
	return http.Post(url, "application/json", bytes.NewReader(b))
}

func patchJSON(url string, body interface{}) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PATCH", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func putJSON(url string, body interface{}) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func deleteReq(url string) (*http.Response, error) {
	req, _ := http.NewRequest("DELETE", url, nil)
	return http.DefaultClient.Do(req)
}

func deleteJSON(url string, body interface{}) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("DELETE", url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status %d, got %d; body=%s", want, resp.StatusCode, string(body))
	}
}

// seedAgent inserts an agent directly into the store for tests that need one.
func seedAgent(t *testing.T, env *testEnv, id, name string, meta map[string]interface{}) {
	t.Helper()
	if meta == nil {
		meta = map[string]interface{}{}
	}
	agent := &models.Agent{
		AgentID:      id,
		Name:         name,
		Metadata:     meta,
		Capabilities: map[string]interface{}{},
	}
	if err := env.store.UpsertAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	schema := &models.AgentSchema{
		AgentID:      id,
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
	}
	_ = env.store.UpsertAgentSchema(context.Background(), schema)
}

// ============================================================================
// 3.1 Agents (AP-001 .. AP-007)
// ============================================================================

func TestAP001_SearchAgentsReturnsAll(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", nil)
	seedAgent(t, env, "a2", "coder", nil)

	resp, _ := postJSON(env.srv.URL+"/agents/search", map[string]interface{}{})
	expectStatus(t, resp, 200)

	var agents []models.Agent
	json.Unmarshal(readBody(t, resp), &agents)
	if len(agents) < 2 {
		t.Fatalf("expected >= 2 agents, got %d", len(agents))
	}
}

func TestAP002_SearchAgentsNameFilter(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", nil)
	seedAgent(t, env, "a2", "coder", nil)

	resp, _ := postJSON(env.srv.URL+"/agents/search", map[string]interface{}{"name": "chat"})
	expectStatus(t, resp, 200)

	var agents []models.Agent
	json.Unmarshal(readBody(t, resp), &agents)
	if len(agents) != 1 || agents[0].Name != "chatbot" {
		t.Fatalf("expected 1 agent named chatbot, got %+v", agents)
	}
}

func TestAP003_SearchAgentsMetadataFilter(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", map[string]interface{}{"env": "prod"})
	seedAgent(t, env, "a2", "coder", map[string]interface{}{"env": "dev"})

	resp, _ := postJSON(env.srv.URL+"/agents/search", map[string]interface{}{
		"metadata": map[string]interface{}{"env": "prod"},
	})
	expectStatus(t, resp, 200)

	var agents []models.Agent
	json.Unmarshal(readBody(t, resp), &agents)
	if len(agents) != 1 || agents[0].AgentID != "a1" {
		t.Fatalf("expected 1 agent with env=prod, got %+v", agents)
	}
}

func TestAP004_SearchAgentsPagination(t *testing.T) {
	env := newTestEnv(t)
	for i := 0; i < 5; i++ {
		seedAgent(t, env, fmt.Sprintf("a%d", i), fmt.Sprintf("agent%d", i), nil)
	}

	resp, _ := postJSON(env.srv.URL+"/agents/search", map[string]interface{}{"limit": 2, "offset": 0})
	expectStatus(t, resp, 200)

	var agents []models.Agent
	json.Unmarshal(readBody(t, resp), &agents)
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
}

func TestAP005_GetAgent(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", nil)

	resp, err := http.Get(env.srv.URL + "/agents/a1")
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, 200)

	var agent models.Agent
	json.Unmarshal(readBody(t, resp), &agent)
	if agent.AgentID != "a1" {
		t.Fatalf("expected agent_id=a1, got %s", agent.AgentID)
	}
}

func TestAP006_GetAgentNotFound(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := http.Get(env.srv.URL + "/agents/nonexistent")
	expectStatus(t, resp, 404)
}

func TestAP007_GetAgentSchemas(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", nil)

	resp, _ := http.Get(env.srv.URL + "/agents/a1/schemas")
	expectStatus(t, resp, 200)

	var schema models.AgentSchema
	json.Unmarshal(readBody(t, resp), &schema)
	if schema.AgentID != "a1" {
		t.Fatalf("expected agent_id=a1, got %s", schema.AgentID)
	}
}

// ============================================================================
// 3.2 Threads (AP-010 .. AP-026)
// ============================================================================

func TestAP010_CreateThread(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := postJSON(env.srv.URL+"/threads", map[string]interface{}{
		"metadata": map[string]interface{}{"topic": "test"},
	})
	expectStatus(t, resp, 200)

	var thread models.Thread
	json.Unmarshal(readBody(t, resp), &thread)
	if thread.ThreadID == "" {
		t.Fatal("expected non-empty thread_id")
	}
	if thread.Status != models.ThreadStatusIdle {
		t.Fatalf("expected status=idle, got %s", thread.Status)
	}
}

func TestAP011_CreateThreadExplicitID(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := postJSON(env.srv.URL+"/threads", map[string]interface{}{
		"thread_id": "my-thread-123",
	})
	expectStatus(t, resp, 200)

	var thread models.Thread
	json.Unmarshal(readBody(t, resp), &thread)
	if thread.ThreadID != "my-thread-123" {
		t.Fatalf("expected thread_id=my-thread-123, got %s", thread.ThreadID)
	}
}

func TestAP012_CreateThreadDuplicateRaise(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "dup"})

	resp, _ := postJSON(env.srv.URL+"/threads", map[string]interface{}{
		"thread_id": "dup",
		"if_exists": "raise",
	})
	expectStatus(t, resp, 409)
}

func TestAP013_CreateThreadDuplicateDoNothing(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "dup"})

	resp, _ := postJSON(env.srv.URL+"/threads", map[string]interface{}{
		"thread_id": "dup",
		"if_exists": "do_nothing",
	})
	expectStatus(t, resp, 200)

	var thread models.Thread
	json.Unmarshal(readBody(t, resp), &thread)
	if thread.ThreadID != "dup" {
		t.Fatalf("expected existing thread returned")
	}
}

func TestAP014_GetThread(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})

	resp, _ := http.Get(env.srv.URL + "/threads/t1")
	expectStatus(t, resp, 200)

	var thread models.Thread
	json.Unmarshal(readBody(t, resp), &thread)
	if thread.Status != models.ThreadStatusIdle {
		t.Fatalf("expected status=idle, got %s", thread.Status)
	}
}

func TestAP015_GetThreadNotFound(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := http.Get(env.srv.URL + "/threads/nonexistent")
	expectStatus(t, resp, 404)
}

func TestAP016_DeleteThread(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})

	resp, _ := deleteReq(env.srv.URL + "/threads/t1")
	expectStatus(t, resp, 204)

	// Verify deleted
	resp2, _ := http.Get(env.srv.URL + "/threads/t1")
	expectStatus(t, resp2, 404)
}

func TestAP017_DeleteThreadNotFound(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := deleteReq(env.srv.URL + "/threads/nonexistent")
	expectStatus(t, resp, 404)
}

func TestAP018_PatchThread(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{
		"thread_id": "t1",
		"metadata":  map[string]interface{}{"a": "1"},
	})

	resp, _ := patchJSON(env.srv.URL+"/threads/t1", map[string]interface{}{
		"metadata": map[string]interface{}{"b": "2"},
	})
	expectStatus(t, resp, 200)

	var thread models.Thread
	json.Unmarshal(readBody(t, resp), &thread)
	if thread.Metadata["a"] != "1" || thread.Metadata["b"] != "2" {
		t.Fatalf("metadata not merged correctly: %+v", thread.Metadata)
	}
}

func TestAP020_SearchThreads(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t2"})

	resp, _ := postJSON(env.srv.URL+"/threads/search", map[string]interface{}{})
	expectStatus(t, resp, 200)

	var threads []models.Thread
	json.Unmarshal(readBody(t, resp), &threads)
	if len(threads) < 2 {
		t.Fatalf("expected >= 2 threads, got %d", len(threads))
	}
}

func TestAP021_SearchThreadsStatusFilter(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t2"})

	idle := models.ThreadStatusIdle
	resp, _ := postJSON(env.srv.URL+"/threads/search", map[string]interface{}{
		"status": idle,
	})
	expectStatus(t, resp, 200)

	var threads []models.Thread
	json.Unmarshal(readBody(t, resp), &threads)
	for _, th := range threads {
		if th.Status != models.ThreadStatusIdle {
			t.Fatalf("expected all idle threads, got %s", th.Status)
		}
	}
}

func TestAP022_SearchThreadsMetadataFilter(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{
		"thread_id": "t1",
		"metadata":  map[string]interface{}{"team": "alpha"},
	})
	postJSON(env.srv.URL+"/threads", map[string]interface{}{
		"thread_id": "t2",
		"metadata":  map[string]interface{}{"team": "beta"},
	})

	resp, _ := postJSON(env.srv.URL+"/threads/search", map[string]interface{}{
		"metadata": map[string]interface{}{"team": "alpha"},
	})
	expectStatus(t, resp, 200)

	var threads []models.Thread
	json.Unmarshal(readBody(t, resp), &threads)
	if len(threads) != 1 || threads[0].ThreadID != "t1" {
		t.Fatalf("expected 1 thread with team=alpha, got %+v", threads)
	}
}

func TestAP023_CopyThread(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{
		"thread_id": "t1",
		"metadata":  map[string]interface{}{"topic": "original"},
	})

	resp, _ := postJSON(env.srv.URL+"/threads/t1/copy", map[string]interface{}{})
	expectStatus(t, resp, 200)

	var copy models.Thread
	json.Unmarshal(readBody(t, resp), &copy)
	if copy.ThreadID == "t1" {
		t.Fatal("copy should have a different thread_id")
	}
	if copy.Metadata["topic"] != "original" {
		t.Fatalf("copy metadata should match original, got %+v", copy.Metadata)
	}
}

func TestAP019_PatchThreadValuesCreatesHistoryEntry(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})

	resp0, _ := http.Get(env.srv.URL + "/threads/t1/history")
	var before []models.ThreadState
	json.Unmarshal(readBody(t, resp0), &before)
	if len(before) != 0 {
		t.Fatalf("expected empty history before patch, got %d entries", len(before))
	}

	resp, _ := patchJSON(env.srv.URL+"/threads/t1", map[string]interface{}{
		"values": map[string]interface{}{"msg": "patched"},
	})
	expectStatus(t, resp, 200)

	resp2, _ := http.Get(env.srv.URL + "/threads/t1/history")
	expectStatus(t, resp2, 200)
	var after []models.ThreadState
	json.Unmarshal(readBody(t, resp2), &after)
	if len(after) != 1 {
		t.Fatalf("expected 1 history entry after PATCH with values, got %d", len(after))
	}
	if after[0].Values["msg"] != "patched" {
		t.Fatalf("expected history entry values to reflect the patch, got %+v", after[0].Values)
	}
}

// TestCheckpoint_BackgroundRunPollPattern guards against a regression where
// checkpoint history and thread.values were only ever populated by the
// create-and-wait/create-and-stream HTTP handlers. A client using the
// equally-valid "create in background, then poll for status" pattern (no
// wait, no stream) got zero history and zero final values, because nothing
// in that path ever persisted a checkpoint. Checkpoint persistence must
// happen in StatusCallback -- fired for every run via ReportStatus,
// regardless of how the client observes it -- not in the HTTP handlers.
func TestCheckpoint_BackgroundRunPollPattern(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := postJSON(env.srv.URL+"/threads/bg-thread/runs", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, resp, 200)
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	ctx := context.Background()
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job in queue: %v", err)
	}
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: assignment.RunID + "_evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(`{"messages":[{"role":"ai","content":"hi"}]}`),
		Ts: time.Now().UnixMilli(),
	})
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: assignment.RunID + "_evt_2", Seq: 2, Method: "end",
		Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
	})

	// Simulate the runner's ReportStatus RPC arriving (this is what a real
	// runner does after StreamEvents completes).
	env.apiServer.StatusCallback()(assignment.RunID, "success", "")

	// No wait(), no stream() -- just poll for terminal status like a
	// background job consumer would.
	respRun, _ := http.Get(env.srv.URL + "/threads/bg-thread/runs/" + run.RunID)
	expectStatus(t, respRun, 200)
	var polled models.Run
	json.Unmarshal(readBody(t, respRun), &polled)
	if polled.Status != models.RunStatusSuccess {
		t.Fatalf("expected run status success, got %s", polled.Status)
	}

	respHist, _ := http.Get(env.srv.URL + "/threads/bg-thread/history")
	expectStatus(t, respHist, 200)
	var hist []models.ThreadState
	json.Unmarshal(readBody(t, respHist), &hist)
	if len(hist) != 1 {
		t.Fatalf("expected 1 checkpoint after background run completes, got %d", len(hist))
	}

	respState, _ := http.Get(env.srv.URL + "/threads/bg-thread/state")
	expectStatus(t, respState, 200)
	var ts models.ThreadState
	json.Unmarshal(readBody(t, respState), &ts)
	if ts.Values["messages"] == nil {
		t.Fatalf("expected thread.state.values to be populated, got %+v", ts.Values)
	}
}

func TestAP024_ThreadHistory(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})

	// Fresh thread with no runs has no checkpoints → empty history
	resp, _ := http.Get(env.srv.URL + "/threads/t1/history")
	expectStatus(t, resp, 200)

	var history []models.ThreadState
	json.Unmarshal(readBody(t, resp), &history)
	if len(history) != 0 {
		t.Fatalf("expected empty history for fresh thread, got %d entries", len(history))
	}

	// POST /threads/t1/state to create a checkpoint, then verify history
	resp2, _ := postJSON(env.srv.URL+"/threads/t1/state", map[string]interface{}{
		"values": map[string]interface{}{"msg": "hello"},
	})
	expectStatus(t, resp2, 200)

	var updateResp models.ThreadUpdateStateResponse
	json.Unmarshal(readBody(t, resp2), &updateResp)
	if updateResp.Checkpoint.CheckpointID == "" {
		t.Fatal("expected checkpoint_id in update response")
	}

	// GET state should return the checkpoint
	resp3, _ := http.Get(env.srv.URL + "/threads/t1/state")
	expectStatus(t, resp3, 200)

	var state models.ThreadState
	json.Unmarshal(readBody(t, resp3), &state)
	if state.Values["msg"] != "hello" {
		t.Errorf("state values = %v, want msg=hello", state.Values)
	}

	// History should now have one entry
	resp4, _ := http.Get(env.srv.URL + "/threads/t1/history")
	expectStatus(t, resp4, 200)

	json.Unmarshal(readBody(t, resp4), &history)
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry after update, got %d", len(history))
	}
}

func TestAP025_ThreadHistoryLimit(t *testing.T) {
	env := newTestEnv(t)
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})

	// Create 5 checkpoints
	for i := 0; i < 5; i++ {
		postJSON(env.srv.URL+"/threads/t1/state", map[string]interface{}{
			"values": map[string]interface{}{"i": i},
		})
	}

	// GET with limit=2 query param
	resp, _ := http.Get(env.srv.URL + "/threads/t1/history?limit=2")
	expectStatus(t, resp, 200)
	var history []*models.ThreadState
	json.Unmarshal(readBody(t, resp), &history)
	if len(history) != 2 {
		t.Fatalf("GET limit=2: expected 2 entries, got %d", len(history))
	}

	// POST with limit in body
	resp2, _ := postJSON(env.srv.URL+"/threads/t1/history", map[string]interface{}{
		"limit": 3,
	})
	expectStatus(t, resp2, 200)
	json.Unmarshal(readBody(t, resp2), &history)
	if len(history) != 3 {
		t.Fatalf("POST limit=3: expected 3 entries, got %d", len(history))
	}
}

func TestAP026_ThreadHistoryBefore(t *testing.T) {
	env := newTestEnv(t)
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})

	// Create 3 checkpoints and capture their IDs
	cpIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		resp, _ := postJSON(env.srv.URL+"/threads/t1/state", map[string]interface{}{
			"values": map[string]interface{}{"step": i},
		})
		var updateResp models.ThreadUpdateStateResponse
		json.Unmarshal(readBody(t, resp), &updateResp)
		cpIDs[i] = updateResp.Checkpoint.CheckpointID
	}

	// GET with before=<last checkpoint> should return the first 2
	resp, _ := http.Get(env.srv.URL + "/threads/t1/history?before=" + cpIDs[2])
	expectStatus(t, resp, 200)
	var history []*models.ThreadState
	json.Unmarshal(readBody(t, resp), &history)
	if len(history) != 2 {
		t.Fatalf("GET before=%s: expected 2 entries, got %d", cpIDs[2], len(history))
	}

	// POST with before in body (SDK format)
	resp2, _ := postJSON(env.srv.URL+"/threads/t1/history", map[string]interface{}{
		"before": map[string]interface{}{"checkpoint_id": cpIDs[1]},
	})
	expectStatus(t, resp2, 200)
	json.Unmarshal(readBody(t, resp2), &history)
	if len(history) != 1 {
		t.Fatalf("POST before=%s: expected 1 entry, got %d", cpIDs[1], len(history))
	}
}

// ============================================================================
// 3.3 Runs — Background (AP-030 .. AP-042)
// ============================================================================

func TestAP030_CreateBackgroundRun(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{
		"agent_id": "test-agent",
		"input":    map[string]interface{}{"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}}},
	})
	expectStatus(t, resp, 200)

	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	if run.Status != models.RunStatusPending {
		t.Fatalf("expected status=pending, got %s", run.Status)
	}
	if run.RunID == "" {
		t.Fatal("expected non-empty run_id")
	}
}

func TestAP031_GetRun(t *testing.T) {
	env := newTestEnv(t)

	resp1, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	resp2, _ := http.Get(env.srv.URL + "/runs/" + run.RunID)
	expectStatus(t, resp2, 200)

	var fetched models.Run
	json.Unmarshal(readBody(t, resp2), &fetched)
	if fetched.RunID != run.RunID {
		t.Fatalf("expected run_id=%s, got %s", run.RunID, fetched.RunID)
	}
}

func TestAP032_GetRunNotFound(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := http.Get(env.srv.URL + "/runs/nonexistent")
	expectStatus(t, resp, 404)
}

func TestAP033_SearchRuns(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "test"})
	postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "test2"})

	resp, _ := postJSON(env.srv.URL+"/runs/search", map[string]interface{}{})
	expectStatus(t, resp, 200)

	var runs []models.Run
	json.Unmarshal(readBody(t, resp), &runs)
	if len(runs) < 2 {
		t.Fatalf("expected >= 2 runs, got %d", len(runs))
	}
}

func TestAP034_DeleteRun(t *testing.T) {
	env := newTestEnv(t)

	// Create a run that we can delete (set to terminal status first)
	resp1, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	// Mark as completed so delete works
	env.store.UpdateRunStatus(context.Background(), run.RunID, models.RunStatusSuccess, nil, "")

	resp2, _ := deleteReq(env.srv.URL + "/runs/" + run.RunID)
	expectStatus(t, resp2, 204)
}

func TestAP036_CancelRun(t *testing.T) {
	env := newTestEnv(t)

	resp1, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	resp2, _ := postJSON(env.srv.URL+"/runs/"+run.RunID+"/cancel", map[string]interface{}{})
	expectStatus(t, resp2, 200)

	var cancelled models.Run
	json.Unmarshal(readBody(t, resp2), &cancelled)
	if cancelled.Status != models.RunStatusInterrupted {
		t.Fatalf("expected status=interrupted, got %s", cancelled.Status)
	}
}

// ============================================================================
// Thread Runs (TS-001 .. TS-012)
// ============================================================================

func TestTS001_CreateThreadRun(t *testing.T) {
	env := newTestEnv(t)

	// Create thread first
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})

	resp, _ := postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{
		"agent_id": "test-agent",
	})
	expectStatus(t, resp, 200)

	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	if run.Status != models.RunStatusPending {
		t.Fatalf("expected status=pending, got %s", run.Status)
	}
	if run.ThreadID != "t1" {
		t.Fatalf("expected thread_id=t1, got %s", run.ThreadID)
	}
}

func TestTS004_ListThreadRuns(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})
	postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{"agent_id": "test"})

	// Reset thread to idle so second run can be created
	env.store.SetThreadStatus(context.Background(), "t1", models.ThreadStatusIdle)
	postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{"agent_id": "test2"})

	resp, _ := http.Get(env.srv.URL + "/threads/t1/runs")
	expectStatus(t, resp, 200)

	var runs []models.Run
	json.Unmarshal(readBody(t, resp), &runs)
	if len(runs) < 2 {
		t.Fatalf("expected >= 2 runs, got %d", len(runs))
	}
}

func TestTS005_GetThreadRunVerifiesThread(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t2"})

	resp1, _ := postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	// Access run via correct thread — should work
	resp2, _ := http.Get(env.srv.URL + "/threads/t1/runs/" + run.RunID)
	expectStatus(t, resp2, 200)
	readBody(t, resp2) // consume body

	// Access same run via wrong thread — should 404
	resp3, _ := http.Get(env.srv.URL + "/threads/t2/runs/" + run.RunID)
	expectStatus(t, resp3, 404)
}

func TestTS008_CancelThreadRunVerifiesThread(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t2"})

	resp1, _ := postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	// Cancel via wrong thread — should 404
	resp2, _ := postJSON(env.srv.URL+"/threads/t2/runs/"+run.RunID+"/cancel", map[string]interface{}{})
	expectStatus(t, resp2, 404)
}

func TestTS009_ConcurrentRunReject409(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "t1"})

	// First run — should succeed
	resp1, _ := postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, resp1, 200)
	readBody(t, resp1) // consume

	// Second run while first is still running — should 409
	resp2, _ := postJSON(env.srv.URL+"/threads/t1/runs", map[string]interface{}{"agent_id": "test2"})
	expectStatus(t, resp2, 409)
}

func TestTS011_RunAutoCreatesThread(t *testing.T) {
	env := newTestEnv(t)

	// Create run on nonexistent thread with if_not_exists=create (default)
	resp, _ := postJSON(env.srv.URL+"/threads/auto-thread/runs", map[string]interface{}{
		"agent_id": "test",
	})
	expectStatus(t, resp, 200)

	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	if run.ThreadID != "auto-thread" {
		t.Fatalf("expected thread_id=auto-thread, got %s", run.ThreadID)
	}

	// Thread should now exist
	resp2, _ := http.Get(env.srv.URL + "/threads/auto-thread")
	expectStatus(t, resp2, 200)
	readBody(t, resp2)
}

func TestTS012_RunRejectNonexistentThread(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := postJSON(env.srv.URL+"/threads/no-such-thread/runs", map[string]interface{}{
		"agent_id":      "test",
		"if_not_exists": "reject",
	})
	expectStatus(t, resp, 404)
}

// ============================================================================
// 3.3 Runs - Streaming (AP-042)
// ============================================================================

func TestAP042_StreamRun(t *testing.T) {
	env := newTestEnv(t)

	// Simulate a runner by publishing events after a run is created
	go func() {
		time.Sleep(100 * time.Millisecond)
		// Find the run in the queue
		ctx := context.Background()
		assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		// Publish events
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID:   assignment.RunID + "_evt_1",
			Seq:       1,
			Method:    "values",
			Namespace: []string{},
			Data:      json.RawMessage(`{"messages":[{"role":"ai","content":"hello"}]}`),
			Ts:        time.Now().UnixMilli(),
		})
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID:   assignment.RunID + "_evt_2",
			Seq:       2,
			Method:    "end",
			Namespace: []string{},
			Data:      json.RawMessage(`{"status":"success"}`),
			Ts:        time.Now().UnixMilli(),
		})
	}()

	resp, _ := postJSON(env.srv.URL+"/threads/stream-thread/runs/stream", map[string]interface{}{
		"agent_id": "test",
	})
	expectStatus(t, resp, 200)

	body := readBody(t, resp)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "event: metadata") {
		t.Fatal("expected metadata SSE event")
	}
	if !strings.Contains(bodyStr, "event: values") {
		t.Fatal("expected values SSE event")
	}
	if !strings.Contains(bodyStr, "event: end") {
		t.Fatal("expected end SSE event")
	}
}

func TestAP040_WaitForRun(t *testing.T) {
	env := newTestEnv(t)

	// Simulate a runner
	go func() {
		time.Sleep(100 * time.Millisecond)
		ctx := context.Background()
		assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID:   assignment.RunID + "_evt_1",
			Seq:       1,
			Method:    "values",
			Namespace: []string{},
			Data:      json.RawMessage(`{"answer":"42"}`),
			Ts:        time.Now().UnixMilli(),
		})
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID:   assignment.RunID + "_evt_2",
			Seq:       2,
			Method:    "end",
			Namespace: []string{},
			Data:      json.RawMessage(`{"status":"success"}`),
			Ts:        time.Now().UnixMilli(),
		})
	}()

	resp, _ := postJSON(env.srv.URL+"/threads/wait-thread/runs/wait", map[string]interface{}{
		"agent_id": "test",
	})
	expectStatus(t, resp, 200)

	var result models.RunWaitResponse
	json.Unmarshal(readBody(t, resp), &result)
	if result.Values["answer"] != "42" {
		t.Fatalf("expected values.answer=42, got %+v", result.Values)
	}
}

// ============================================================================
// 3.6 Store (AP-070 .. AP-081)
// ============================================================================

func TestAP070_PutItem(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"users", "alice"},
		"key":       "profile",
		"value":     map[string]interface{}{"name": "Alice", "age": 30},
	})
	expectStatus(t, resp, 204)
}

func TestAP071_PutItemUpdate(t *testing.T) {
	env := newTestEnv(t)

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"users", "alice"},
		"key":       "profile",
		"value":     map[string]interface{}{"name": "Alice"},
	})

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"users", "alice"},
		"key":       "profile",
		"value":     map[string]interface{}{"name": "Alice Updated"},
	})

	resp, _ := http.Get(env.srv.URL + "/store/items?namespace=users,alice&key=profile")
	expectStatus(t, resp, 200)

	var item models.StoreItem
	json.Unmarshal(readBody(t, resp), &item)
	if item.Value["name"] != "Alice Updated" {
		t.Fatalf("expected updated name, got %+v", item.Value)
	}
}

func TestAP072_GetItem(t *testing.T) {
	env := newTestEnv(t)

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"ns1"},
		"key":       "k1",
		"value":     map[string]interface{}{"data": "hello"},
	})

	resp, _ := http.Get(env.srv.URL + "/store/items?namespace=ns1&key=k1")
	expectStatus(t, resp, 200)

	var item models.StoreItem
	json.Unmarshal(readBody(t, resp), &item)
	if item.Key != "k1" || item.Value["data"] != "hello" {
		t.Fatalf("unexpected item: %+v", item)
	}
}

func TestAP073_GetItemNotFound(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := http.Get(env.srv.URL + "/store/items?namespace=ns1&key=nonexistent")
	expectStatus(t, resp, 404)
}

func TestAP074_DeleteItem(t *testing.T) {
	env := newTestEnv(t)

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"ns1"},
		"key":       "k1",
		"value":     map[string]interface{}{"data": "hello"},
	})

	resp, _ := deleteJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"ns1"},
		"key":       "k1",
	})
	expectStatus(t, resp, 204)

	// Verify deleted
	resp2, _ := http.Get(env.srv.URL + "/store/items?namespace=ns1&key=k1")
	expectStatus(t, resp2, 404)
}

func TestAP075_DeleteItemNotFound(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := deleteJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"ns1"},
		"key":       "nonexistent",
	})
	expectStatus(t, resp, 404)
}

func TestAP076_SearchItemsByNamespace(t *testing.T) {
	env := newTestEnv(t)

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"team", "alpha"},
		"key":       "doc1",
		"value":     map[string]interface{}{"title": "A"},
	})
	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"team", "beta"},
		"key":       "doc2",
		"value":     map[string]interface{}{"title": "B"},
	})

	resp, _ := postJSON(env.srv.URL+"/store/items/search", map[string]interface{}{
		"namespace_prefix": []string{"team", "alpha"},
	})
	expectStatus(t, resp, 200)

	var result models.StoreSearchResponse
	json.Unmarshal(readBody(t, resp), &result)
	if len(result.Items) != 1 || result.Items[0].Key != "doc1" {
		t.Fatalf("expected 1 item (doc1), got %+v", result.Items)
	}
}

func TestAP077_SearchItemsWithFilter(t *testing.T) {
	env := newTestEnv(t)

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"items"},
		"key":       "k1",
		"value":     map[string]interface{}{"status": "active", "score": 10},
	})
	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"items"},
		"key":       "k2",
		"value":     map[string]interface{}{"status": "archived", "score": 5},
	})

	resp, _ := postJSON(env.srv.URL+"/store/items/search", map[string]interface{}{
		"namespace_prefix": []string{"items"},
		"filter":           map[string]interface{}{"status": "active"},
	})
	expectStatus(t, resp, 200)

	var result models.StoreSearchResponse
	json.Unmarshal(readBody(t, resp), &result)
	if len(result.Items) != 1 || result.Items[0].Key != "k1" {
		t.Fatalf("expected 1 item (k1) with status=active, got %+v", result.Items)
	}
}

func TestAP078_SearchItemsPagination(t *testing.T) {
	env := newTestEnv(t)

	for i := 0; i < 5; i++ {
		putJSON(env.srv.URL+"/store/items", map[string]interface{}{
			"namespace": []string{"page"},
			"key":       fmt.Sprintf("k%d", i),
			"value":     map[string]interface{}{"i": i},
		})
	}

	resp, _ := postJSON(env.srv.URL+"/store/items/search", map[string]interface{}{
		"namespace_prefix": []string{"page"},
		"limit":            2,
		"offset":           0,
	})
	expectStatus(t, resp, 200)

	var result models.StoreSearchResponse
	json.Unmarshal(readBody(t, resp), &result)
	if len(result.Items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(result.Items))
	}
}

func TestAP079_ListNamespaces(t *testing.T) {
	env := newTestEnv(t)

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"a", "b"},
		"key":       "k1",
		"value":     map[string]interface{}{},
	})
	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"c", "d"},
		"key":       "k2",
		"value":     map[string]interface{}{},
	})

	resp, _ := postJSON(env.srv.URL+"/store/namespaces", map[string]interface{}{})
	expectStatus(t, resp, 200)

	var namespaces [][]string
	json.Unmarshal(readBody(t, resp), &namespaces)
	if len(namespaces) < 2 {
		t.Fatalf("expected >= 2 namespaces, got %d", len(namespaces))
	}
}

func TestAP080_ListNamespacesWithPrefix(t *testing.T) {
	env := newTestEnv(t)

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"org", "team1"},
		"key":       "k1",
		"value":     map[string]interface{}{},
	})
	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"org", "team2"},
		"key":       "k2",
		"value":     map[string]interface{}{},
	})
	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"other"},
		"key":       "k3",
		"value":     map[string]interface{}{},
	})

	resp, _ := postJSON(env.srv.URL+"/store/namespaces", map[string]interface{}{
		"prefix": []string{"org"},
	})
	expectStatus(t, resp, 200)

	var namespaces [][]string
	json.Unmarshal(readBody(t, resp), &namespaces)
	if len(namespaces) != 2 {
		t.Fatalf("expected 2 namespaces under org/, got %d: %+v", len(namespaces), namespaces)
	}
}

func TestAP081_StoreComplexJSON(t *testing.T) {
	env := newTestEnv(t)

	complexValue := map[string]interface{}{
		"nested": map[string]interface{}{
			"deep": map[string]interface{}{
				"array": []interface{}{1, "two", true, nil},
			},
		},
		"unicode": "こんにちは 🎉",
		"empty":   map[string]interface{}{},
	}

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"complex"},
		"key":       "deep",
		"value":     complexValue,
	})

	resp, _ := http.Get(env.srv.URL + "/store/items?namespace=complex&key=deep")
	expectStatus(t, resp, 200)

	var item models.StoreItem
	json.Unmarshal(readBody(t, resp), &item)

	// Check nested structure survived
	nested, ok := item.Value["nested"].(map[string]interface{})
	if !ok {
		t.Fatalf("nested field lost structure: %+v", item.Value)
	}
	deep, ok := nested["deep"].(map[string]interface{})
	if !ok {
		t.Fatalf("deep field lost structure: %+v", nested)
	}
	arr, ok := deep["array"].([]interface{})
	if !ok || len(arr) != 4 {
		t.Fatalf("array lost: %+v", deep)
	}

	// Unicode preserved
	if item.Value["unicode"] != "こんにちは 🎉" {
		t.Fatalf("unicode not preserved: %+v", item.Value["unicode"])
	}
}

// TestStore_InternalRoutesMirrorClientRoutes proves /internal/store/* (used
// by proxy-mode runners authenticating with a runner token instead of a
// client credential -- see RunkiteStore in python/runkite_runner/store.py)
// is the exact same store as the client-facing /store/*, not a parallel one.
func TestStore_InternalRoutesMirrorClientRoutes(t *testing.T) {
	env := newTestEnv(t)

	// Write via the internal route, read via the client route.
	resp, _ := putJSON(env.srv.URL+"/internal/store/items", map[string]interface{}{
		"namespace": []string{"runner-ns"},
		"key":       "k1",
		"value":     map[string]interface{}{"v": 1},
	})
	expectStatus(t, resp, 204)

	resp, _ = http.Get(env.srv.URL + "/store/items?namespace=runner-ns&key=k1")
	expectStatus(t, resp, 200)
	var item models.StoreItem
	json.Unmarshal(readBody(t, resp), &item)
	if item.Value["v"] != float64(1) {
		t.Fatalf("expected value written via /internal/store to be visible via /store, got %+v", item.Value)
	}

	// Write via the client route, read via the internal route.
	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"runner-ns"},
		"key":       "k1",
		"value":     map[string]interface{}{"v": 2},
	})
	resp, _ = http.Get(env.srv.URL + "/internal/store/items?namespace=runner-ns&key=k1")
	expectStatus(t, resp, 200)
	json.Unmarshal(readBody(t, resp), &item)
	if item.Value["v"] != float64(2) {
		t.Fatalf("expected value written via /store to be visible via /internal/store, got %+v", item.Value)
	}

	// Search and list-namespaces also work on the internal route.
	resp, _ = postJSON(env.srv.URL+"/internal/store/items/search", map[string]interface{}{
		"namespace_prefix": []string{"runner-ns"},
	})
	expectStatus(t, resp, 200)

	resp, _ = postJSON(env.srv.URL+"/internal/store/namespaces", map[string]interface{}{})
	expectStatus(t, resp, 200)
}

// ============================================================================
// Health check
// ============================================================================

func TestHealthCheck(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := http.Get(env.srv.URL + "/health")
	expectStatus(t, resp, 200)

	var body map[string]string
	json.Unmarshal(readBody(t, resp), &body)
	if body["status"] != "ok" {
		t.Fatalf("expected status=ok, got %s", body["status"])
	}
}

// ============================================================================
// Config loader tests
// ============================================================================

// ============================================================================
// SDK Compatibility (/assistants aliases)
// ============================================================================

func TestSDK_AssistantsSearchReturnsSDKShape(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", map[string]interface{}{"env": "test"})

	resp, _ := postJSON(env.srv.URL+"/assistants/search", map[string]interface{}{})
	expectStatus(t, resp, 200)

	var assistants []map[string]interface{}
	json.Unmarshal(readBody(t, resp), &assistants)
	if len(assistants) == 0 {
		t.Fatal("expected assistants from /assistants/search")
	}
	a := assistants[0]
	// SDK expects assistant_id, not agent_id
	if _, ok := a["assistant_id"]; !ok {
		t.Fatal("response missing assistant_id field")
	}
	if _, ok := a["graph_id"]; !ok {
		t.Fatal("response missing graph_id field")
	}
	if _, ok := a["agent_id"]; ok {
		t.Fatal("response should not contain agent_id (SDK compat)")
	}
}

func TestSDK_AssistantsGetReturnsSDKShape(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", nil)

	resp, _ := http.Get(env.srv.URL + "/assistants/a1")
	expectStatus(t, resp, 200)

	var a map[string]interface{}
	json.Unmarshal(readBody(t, resp), &a)
	if a["assistant_id"] != "a1" {
		t.Fatalf("expected assistant_id=a1, got %v", a["assistant_id"])
	}
	if a["graph_id"] != "a1" {
		t.Fatalf("expected graph_id=a1, got %v", a["graph_id"])
	}
}

func TestSDK_AssistantsSchemasReturnsSDKShape(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", nil)

	resp, _ := http.Get(env.srv.URL + "/assistants/a1/schemas")
	expectStatus(t, resp, 200)

	var s map[string]interface{}
	json.Unmarshal(readBody(t, resp), &s)
	if s["graph_id"] != "a1" {
		t.Fatalf("expected graph_id=a1, got %v", s["graph_id"])
	}
	if _, ok := s["agent_id"]; ok {
		t.Fatal("schemas response should not contain agent_id (SDK compat)")
	}
}

// ============================================================================
// Store namespace dot-separated parsing (SDK compat)
// ============================================================================

func TestSDK_StoreGetDotNamespace(t *testing.T) {
	env := newTestEnv(t)

	putJSON(env.srv.URL+"/store/items", map[string]interface{}{
		"namespace": []string{"sdk", "test"},
		"key":       "k1",
		"value":     map[string]interface{}{"v": "ok"},
	})

	// SDK sends dot-separated namespace
	resp, _ := http.Get(env.srv.URL + "/store/items?namespace=sdk.test&key=k1")
	expectStatus(t, resp, 200)

	var item models.StoreItem
	json.Unmarshal(readBody(t, resp), &item)
	if item.Value["v"] != "ok" {
		t.Fatalf("expected v=ok via dot namespace, got %+v", item.Value)
	}
}

// ============================================================================
// AP-035: DELETE active run returns 422
// ============================================================================

func TestAP035_DeleteActiveRunRejects422(t *testing.T) {
	env := newTestEnv(t)

	resp1, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	// Try to delete while still pending
	resp2, _ := deleteReq(env.srv.URL + "/runs/" + run.RunID)
	expectStatus(t, resp2, 422)
}

// ============================================================================
// AP-041: Wait for already-completed run returns immediately
// ============================================================================

func TestAP041_WaitCompletedRunImmediate(t *testing.T) {
	env := newTestEnv(t)

	resp1, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	// Mark completed
	env.store.UpdateRunStatus(context.Background(), run.RunID, models.RunStatusSuccess, []byte(`{"done":true}`), "")

	resp2, _ := http.Get(env.srv.URL + "/runs/" + run.RunID + "/wait")
	expectStatus(t, resp2, 200)

	var result models.RunWaitResponse
	json.Unmarshal(readBody(t, resp2), &result)
	if result.Run.Status != models.RunStatusSuccess {
		t.Fatalf("expected status=success, got %s", result.Run.Status)
	}
}

// ============================================================================
// Mid-run SSE Replay: attach to an in-flight run and receive past events
// ============================================================================

func TestStreamExistingRun_MidRunReplay(t *testing.T) {
	env := newTestEnv(t)

	// Create a run in background
	resp, _ := postJSON(env.srv.URL+"/threads/replay-thread/runs", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, resp, 200)
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	ctx := context.Background()
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job in queue: %v", err)
	}

	// Publish 2 events BEFORE a client attaches
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(`{"step":1}`), Ts: time.Now().UnixMilli(),
	})
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: "evt_2", Seq: 2, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(`{"step":2}`), Ts: time.Now().UnixMilli(),
	})

	// Now a client attaches mid-run — publish the terminal event async so the
	// stream handler isn't stuck forever
	go func() {
		time.Sleep(100 * time.Millisecond)
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: "evt_3", Seq: 3, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()

	streamResp, _ := http.Get(env.srv.URL + "/runs/" + run.RunID + "/stream")
	expectStatus(t, streamResp, 200)

	body := string(readBody(t, streamResp))

	// Must contain ALL events — including the two published before attach
	if !strings.Contains(body, `"step":1`) {
		t.Error("missing replayed event step:1")
	}
	if !strings.Contains(body, `"step":2`) {
		t.Error("missing replayed event step:2")
	}
	if !strings.Contains(body, "event: end") {
		t.Error("missing terminal end event")
	}
}

func TestWaitExistingRun_MidRunReplay(t *testing.T) {
	env := newTestEnv(t)

	// Create a run in background
	resp, _ := postJSON(env.srv.URL+"/threads/wait-replay/runs", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, resp, 200)
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	ctx := context.Background()
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job in queue: %v", err)
	}

	// Publish values event BEFORE client calls wait
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(`{"messages":[{"role":"ai","content":"early"}]}`), Ts: time.Now().UnixMilli(),
	})

	// Terminal event arrives shortly after client calls wait
	go func() {
		time.Sleep(100 * time.Millisecond)
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: "evt_2", Seq: 2, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()

	waitResp, _ := http.Get(env.srv.URL + "/runs/" + run.RunID + "/wait")
	expectStatus(t, waitResp, 200)

	var result models.RunWaitResponse
	json.Unmarshal(readBody(t, waitResp), &result)

	// Wait response MUST include the values from the event published before attach
	if result.Values == nil {
		t.Fatal("expected values in wait response, got nil")
	}
	msgs, ok := result.Values["messages"]
	if !ok || msgs == nil {
		t.Fatal("expected messages in values, got nil")
	}
}

func TestStreamCompletedRun_FullReplay(t *testing.T) {
	env := newTestEnv(t)

	// Create run, publish events, complete it, THEN stream
	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	ctx := context.Background()
	assignment, _ := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)

	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "values",
		Namespace: []string{}, Data: json.RawMessage(`{"final":"data"}`), Ts: time.Now().UnixMilli(),
	})
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: "evt_2", Seq: 2, Method: "end",
		Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
	})

	// Mark run terminal
	env.store.UpdateRunStatus(ctx, run.RunID, models.RunStatusSuccess, nil, "")

	// Stream AFTER run is already done
	streamResp, _ := http.Get(env.srv.URL + "/runs/" + run.RunID + "/stream")
	expectStatus(t, streamResp, 200)

	body := string(readBody(t, streamResp))
	if !strings.Contains(body, `"final":"data"`) {
		t.Error("completed run stream should replay historical values event")
	}
	if !strings.Contains(body, "event: end") {
		t.Error("completed run stream should contain end event")
	}
}

// TestSSE_SurvivesMetricsMiddlewareWrapping guards against a critical
// regression where wrapping apiServer.Handler() with metrics.HTTPMiddleware
// (exactly what cmd/serve.go does in production -- every request goes
// through the metrics middleware before reaching the API) silently broke
// every SSE endpoint. The metrics middleware's responseWriter didn't expose
// Flush(), so the handlers' `w.(http.Flusher)` assertions failed and every
// streaming response returned 200 with correct headers but zero body,
// forever, with no visible error. This test specifically exercises the
// *composed* stack the way it actually runs in production, not just
// apiServer.Handler() in isolation -- that distinction is exactly what let
// the regression through undetected.
func TestSSE_SurvivesMetricsMiddlewareWrapping(t *testing.T) {
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	queue := inprocess.NewQueue()
	broker := inprocess.NewBroker()
	apiServer := api.NewServer(store, queue, broker, inprocess.NewCancelBus())

	// Mirror cmd/serve.go: wrap the whole handler with the metrics middleware.
	wrapped := metrics.HTTPMiddleware(apiServer.Handler())
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)

	go func() {
		ctx := context.Background()
		assignment, err := queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_1", Seq: 1, Method: "values",
			Namespace: []string{}, Data: json.RawMessage(`{"messages":[{"role":"ai","content":"hi"}]}`),
			Ts: time.Now().UnixMilli(),
		})
		broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_2", Seq: 2, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()

	resp, err := postJSON(srv.URL+"/threads/wrapped-thread/runs/stream", map[string]interface{}{"agent_id": "test"})
	if err != nil {
		t.Fatal(err)
	}
	body := string(readBody(t, resp))

	if !strings.Contains(body, "event: metadata") || !strings.Contains(body, "event: values") || !strings.Contains(body, "event: end") {
		t.Fatalf("SSE body missing expected events through the composed (metrics-wrapped) stack -- got: %q", body)
	}
}

// ============================================================================
// TS-002: POST /threads/{id}/runs/stream
// ============================================================================

func TestTS002_CreateAndStreamThreadRun(t *testing.T) {
	env := newTestEnv(t)

	go func() {
		time.Sleep(100 * time.Millisecond)
		ctx := context.Background()
		assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_1", Seq: 1, Method: "values",
			Namespace: []string{}, Data: json.RawMessage(`{"msg":"hi"}`), Ts: time.Now().UnixMilli(),
		})
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_2", Seq: 2, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ts2"})
	resp, _ := postJSON(env.srv.URL+"/threads/ts2/runs/stream", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, resp, 200)

	body := string(readBody(t, resp))
	if !strings.Contains(body, "event: end") {
		t.Fatal("expected end SSE event in thread run stream")
	}
}

// ============================================================================
// TS-003: POST /threads/{id}/runs/wait
// ============================================================================

func TestTS003_CreateAndWaitThreadRun(t *testing.T) {
	env := newTestEnv(t)

	go func() {
		time.Sleep(100 * time.Millisecond)
		ctx := context.Background()
		assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_1", Seq: 1, Method: "values",
			Namespace: []string{}, Data: json.RawMessage(`{"result":"done"}`), Ts: time.Now().UnixMilli(),
		})
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_2", Seq: 2, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ts3"})
	resp, _ := postJSON(env.srv.URL+"/threads/ts3/runs/wait", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, resp, 200)

	var result models.RunWaitResponse
	json.Unmarshal(readBody(t, resp), &result)
	if result.Values["result"] != "done" {
		t.Fatalf("expected result=done, got %+v", result.Values)
	}
}

// ============================================================================
// TS-006: GET /threads/{id}/runs/{run_id}/stream (attach to existing)
// ============================================================================

func TestTS006_StreamExistingThreadRun(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ts6"})
	resp1, _ := postJSON(env.srv.URL+"/threads/ts6/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	// Mark as success so streamExistingRun returns the terminal event immediately
	env.store.UpdateRunStatus(context.Background(), run.RunID, models.RunStatusSuccess, nil, "")

	resp2, _ := http.Get(env.srv.URL + "/threads/ts6/runs/" + run.RunID + "/stream")
	expectStatus(t, resp2, 200)

	body := string(readBody(t, resp2))
	if !strings.Contains(body, "event: end") {
		t.Fatal("expected end event for completed run stream")
	}
}

// ============================================================================
// TS-007: GET /threads/{id}/runs/{run_id}/wait (wait for existing)
// ============================================================================

func TestTS007_WaitExistingThreadRun(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ts7"})
	resp1, _ := postJSON(env.srv.URL+"/threads/ts7/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	// Mark as success
	env.store.UpdateRunStatus(context.Background(), run.RunID, models.RunStatusSuccess, []byte(`{"ok":true}`), "")

	resp2, _ := http.Get(env.srv.URL + "/threads/ts7/runs/" + run.RunID + "/wait")
	expectStatus(t, resp2, 200)

	var result models.RunWaitResponse
	json.Unmarshal(readBody(t, resp2), &result)
	if result.Run.Status != models.RunStatusSuccess {
		t.Fatalf("expected success, got %s", result.Run.Status)
	}
}

// TestTS_CreateAndWaitPersistsStatusBeforeResponding is
// TestTS_WaitPersistsStatusBeforeResponding's counterpart for the OTHER
// code path with the identical bug: POST /threads/{id}/runs/wait
// (waitForRun, the create-and-wait-in-one-call variant), not GET
// .../wait on an already-existing run (waitForExistingRun). Same root
// cause, same fix, needs its own coverage since it's a separate function.
func TestTS_CreateAndWaitPersistsStatusBeforeResponding(t *testing.T) {
	env := newTestEnv(t)

	go func() {
		time.Sleep(100 * time.Millisecond)
		ctx := context.Background()
		assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		// Deliberately no UpdateRunStatus/StatusCallback call here --
		// isolates the exact gap between "terminal event observed" and
		// "ReportStatus RPC processed" that caused the real bug.
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_1", Seq: 1, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "create-wait-persist"})
	waitResp, _ := postJSON(env.srv.URL+"/threads/create-wait-persist/runs/wait", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, waitResp, 200)

	var waitResult models.RunWaitResponse
	json.Unmarshal(readBody(t, waitResp), &waitResult)
	if waitResult.Run.Status != models.RunStatusSuccess {
		t.Fatalf("expected create-and-wait to report success, got %s", waitResult.Run.Status)
	}

	getResp, _ := http.Get(env.srv.URL + "/threads/create-wait-persist/runs/" + waitResult.Run.RunID)
	var gotRun models.Run
	json.Unmarshal(readBody(t, getResp), &gotRun)
	if gotRun.Status != models.RunStatusSuccess {
		t.Fatalf("expected a plain GET right after create-and-wait to see the persisted status, got %q", gotRun.Status)
	}
}

// TestTS_CreateAndWaitResetsThreadStatusBeforeResponding is
// TestTS_WaitResetsThreadStatusBeforeResponding's counterpart for
// waitForRun (POST /threads/{id}/runs/wait) -- same fix, same rationale,
// separate function needing its own coverage.
func TestTS_CreateAndWaitResetsThreadStatusBeforeResponding(t *testing.T) {
	env := newTestEnv(t)

	go func() {
		time.Sleep(100 * time.Millisecond)
		ctx := context.Background()
		assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
		if err != nil || assignment == nil {
			return
		}
		env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
			EventID: assignment.RunID + "_evt_1", Seq: 1, Method: "end",
			Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
		})
	}()

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "create-wait-thread-reset"})
	waitResp, _ := postJSON(env.srv.URL+"/threads/create-wait-thread-reset/runs/wait", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, waitResp, 200)

	resp2, _ := postJSON(env.srv.URL+"/threads/create-wait-thread-reset/runs", map[string]interface{}{"agent_id": "test"})
	if resp2.StatusCode != 200 {
		t.Fatalf("expected a second run on the same thread right after create-and-wait to succeed, got status %d: %s", resp2.StatusCode, readBody(t, resp2))
	}
}

// TestTS_StatusCallbackDoesNotClobberNewerRunClaim is the regression for
// the race the /wait-early-idle fix makes reachable: /wait sets thread
// idle, client immediately creates run B (TryClaimThread -> busy), then
// run A's late StatusCallback must NOT SetThreadStatus(idle) or B loses
// its claim while still executing -- two concurrent runs on one thread.
func TestTS_StatusCallbackDoesNotClobberNewerRunClaim(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "clobber-thread"})
	resp1, _ := postJSON(env.srv.URL+"/threads/clobber-thread/runs", map[string]interface{}{"agent_id": "test"})
	var run1 models.Run
	json.Unmarshal(readBody(t, resp1), &run1)

	ctx := context.Background()
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job in queue: %v", err)
	}
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "end",
		Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
	})

	waitResp, _ := http.Get(env.srv.URL + "/threads/clobber-thread/runs/" + run1.RunID + "/wait")
	expectStatus(t, waitResp, 200)

	resp2, _ := postJSON(env.srv.URL+"/threads/clobber-thread/runs", map[string]interface{}{"agent_id": "test"})
	if resp2.StatusCode != 200 {
		t.Fatalf("second run create failed: %d %s", resp2.StatusCode, readBody(t, resp2))
	}
	var run2 models.Run
	json.Unmarshal(readBody(t, resp2), &run2)

	// Late ReportStatus for run1 -- must not release the thread out from
	// under run2.
	env.apiServer.StatusCallback()(run1.RunID, "success", "")

	thread, err := env.store.GetThread(ctx, "clobber-thread")
	if err != nil {
		t.Fatal(err)
	}
	if thread.Status != models.ThreadStatusBusy {
		t.Fatalf("expected thread to stay busy for run2 after late StatusCallback for run1, got %q", thread.Status)
	}
	got2, err := env.store.GetRun(ctx, run2.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != models.RunStatusPending && got2.Status != models.RunStatusRunning {
		t.Fatalf("run2 should still be in-flight, got %q", got2.Status)
	}
}

// TestTS_WaitResetsThreadStatusBeforeResponding is the regression for a
// real gap found in a follow-up review: persisting the RUN's status
// (TestTS_WaitPersistsStatusBeforeResponding's fix) wasn't enough on its
// own. createRunCtx's TryClaimThread checks the THREAD's own status,
// which otherwise stays "busy" until StatusCallback's later
// SetThreadStatus call -- so a fast client doing
// wait-then-immediately-create-the-next-run on the same thread (a
// completely normal chat-turn pattern) could get a spurious 409, even
// though /wait just told it the previous run succeeded. Reproduces the
// exact gap: publishes the terminal event directly (StatusCallback never
// runs in this test), then asserts a second run can be created on the
// same thread immediately after /wait returns.
func TestTS_WaitResetsThreadStatusBeforeResponding(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "wait-thread-reset"})
	resp1, _ := postJSON(env.srv.URL+"/threads/wait-thread-reset/runs", map[string]interface{}{"agent_id": "test"})
	var run1 models.Run
	json.Unmarshal(readBody(t, resp1), &run1)

	ctx := context.Background()
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job in queue: %v", err)
	}

	// Terminal event reaches the broker -- StatusCallback (the runner's
	// separate ReportStatus RPC) deliberately never runs in this test,
	// isolating the exact gap.
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "end",
		Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
	})

	waitResp, _ := http.Get(env.srv.URL + "/threads/wait-thread-reset/runs/" + run1.RunID + "/wait")
	expectStatus(t, waitResp, 200)

	// The regression check: a second run on the SAME thread, created
	// immediately after /wait returns, must succeed -- not 409, even
	// though StatusCallback (which would normally reset the thread to
	// idle) has not run in this test.
	resp2, _ := postJSON(env.srv.URL+"/threads/wait-thread-reset/runs", map[string]interface{}{"agent_id": "test"})
	if resp2.StatusCode != 200 {
		t.Fatalf("expected a second run on the same thread right after /wait to succeed, got status %d: %s", resp2.StatusCode, readBody(t, resp2))
	}

	thread, err := env.store.GetThread(ctx, "wait-thread-reset")
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if thread.Status != models.ThreadStatusBusy {
		t.Errorf("expected thread to be busy again (claimed by the second run), got %q", thread.Status)
	}
}

// TestTS_WaitPersistsStatusBeforeResponding is the regression for a real
// bug found via load testing (bench/loadgen against a live control
// plane): waitForExistingRun derived the terminal status from observing
// the event stream (the runner's "end" event, published before its
// separate ReportStatus RPC arrives) and patched it into the in-memory
// Run struct ONLY for this one response -- never writing it back. A
// client that called /wait, then immediately did a plain GET on the same
// run (a completely reasonable thing to do), could see "pending" even
// though /wait had just told it "success", because StatusCallback (the
// actual writer, fired by ReportStatus) hadn't run yet. Reproduced
// reliably under concurrent load in bench/loadgen; this test reproduces
// it deterministically by publishing the terminal event directly to the
// broker WITHOUT ever calling UpdateRunStatus/StatusCallback, mirroring
// the real timing gap between "event streamed" and "ReportStatus
// processed."
func TestTS_WaitPersistsStatusBeforeResponding(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "wait-persist"})
	resp1, _ := postJSON(env.srv.URL+"/threads/wait-persist/runs", map[string]interface{}{"agent_id": "test"})
	var run models.Run
	json.Unmarshal(readBody(t, resp1), &run)

	ctx := context.Background()
	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job in queue: %v", err)
	}

	// Terminal event reaches the broker -- but UpdateRunStatus/
	// StatusCallback (the runner's separate ReportStatus RPC) never
	// runs in this test, deliberately, to isolate exactly the gap that
	// caused the real bug.
	env.broker.Publish(ctx, assignment.RunID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "end",
		Namespace: []string{}, Data: json.RawMessage(`{"status":"success"}`), Ts: time.Now().UnixMilli(),
	})

	waitResp, _ := http.Get(env.srv.URL + "/threads/wait-persist/runs/" + run.RunID + "/wait")
	expectStatus(t, waitResp, 200)
	var waitResult models.RunWaitResponse
	json.Unmarshal(readBody(t, waitResp), &waitResult)
	if waitResult.Run.Status != models.RunStatusSuccess {
		t.Fatalf("expected /wait to report success, got %s", waitResult.Run.Status)
	}

	// The actual regression check: a plain GET immediately afterward
	// must see the SAME status /wait just reported, not "pending".
	getResp, _ := http.Get(env.srv.URL + "/threads/wait-persist/runs/" + run.RunID)
	var gotRun models.Run
	json.Unmarshal(readBody(t, getResp), &gotRun)
	if gotRun.Status != models.RunStatusSuccess {
		t.Fatalf("expected a plain GET right after /wait to see the persisted status, got %q (StatusCallback hasn't even run yet in this test)", gotRun.Status)
	}
}

// ============================================================================
// TS-010: Sequential runs on same thread see accumulated state
// ============================================================================

func TestTS010_SequentialRunsSameThread(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "ts10"})

	// First run
	resp1, _ := postJSON(env.srv.URL+"/threads/ts10/runs", map[string]interface{}{"agent_id": "test"})
	expectStatus(t, resp1, 200)
	readBody(t, resp1)

	// Complete first run and reset thread
	runs, _ := env.store.SearchRuns(context.Background(), &models.RunSearchRequest{ThreadID: "ts10", Limit: 1})
	if len(runs) > 0 {
		env.store.UpdateRunStatus(context.Background(), runs[0].RunID, models.RunStatusSuccess, nil, "")
	}
	env.store.SetThreadStatus(context.Background(), "ts10", models.ThreadStatusIdle)

	// Second run should succeed on the same thread
	resp2, _ := postJSON(env.srv.URL+"/threads/ts10/runs", map[string]interface{}{"agent_id": "test2"})
	expectStatus(t, resp2, 200)

	var run2 models.Run
	json.Unmarshal(readBody(t, resp2), &run2)
	if run2.ThreadID != "ts10" {
		t.Fatalf("expected thread_id=ts10, got %s", run2.ThreadID)
	}
}

// ============================================================================
// TS-009 Race Regression: concurrent goroutines prove TryClaimThread is atomic
// ============================================================================

func TestTS009_RaceRegression(t *testing.T) {
	env := newTestEnv(t)

	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "race"})

	const N = 20
	results := make(chan int, N)

	// Fire N concurrent run-create requests
	for i := 0; i < N; i++ {
		go func() {
			resp, err := postJSON(env.srv.URL+"/threads/race/runs", map[string]interface{}{
				"agent_id": fmt.Sprintf("agent-%d", i),
			})
			if err != nil {
				results <- 0
				return
			}
			code := resp.StatusCode
			readBody(t, resp)
			results <- code
		}()
	}

	var ok, conflict int
	for i := 0; i < N; i++ {
		code := <-results
		switch code {
		case 200:
			ok++
		case 409:
			conflict++
		}
	}

	if ok != 1 {
		t.Fatalf("expected exactly 1 success (200), got %d successes and %d conflicts", ok, conflict)
	}
	if conflict != N-1 {
		t.Fatalf("expected %d conflicts (409), got %d", N-1, conflict)
	}
}

// ============================================================================
// Config loader tests
// ============================================================================

// newTestEnvWithConnectors creates a test env with a connector registry attached.
func newTestEnvWithConnectors(t *testing.T, configs map[string]connector.ConnectorConfig) *testEnv {
	t.Helper()
	env := newTestEnv(t)
	reg := connector.NewRegistry(configs)
	env.apiServer.SetConnectorRegistry(reg)
	// Rebuild server with updated handler
	env.srv.Close()
	env.srv = httptest.NewServer(env.apiServer.Handler())
	t.Cleanup(env.srv.Close)
	return env
}

// ============================================================================
// Connector Registry API
// ============================================================================

func TestConnector_ListConnectors(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"salesforce": {Auth: connector.AuthConfig{Type: "api_key", APIKey: "sf-key"}},
		"snowflake":  {Auth: connector.AuthConfig{Type: "bearer", BearerToken: "snow-tok"}},
	})

	resp, _ := http.Get(env.srv.URL + "/internal/connectors")
	expectStatus(t, resp, 200)

	var list []map[string]interface{}
	json.Unmarshal(readBody(t, resp), &list)
	if len(list) != 2 {
		t.Fatalf("expected 2 connectors, got %d", len(list))
	}
	// Should be sorted
	if list[0]["name"] != "salesforce" {
		t.Fatalf("expected first connector = salesforce, got %v", list[0]["name"])
	}
	if list[1]["name"] != "snowflake" {
		t.Fatalf("expected second connector = snowflake, got %v", list[1]["name"])
	}
}

// TestConnector_ListConnectors_MCPShowsProxyPathNotRawURL is a
// regression test for a real bypass found on review (plans/pending_items.md
// item 17): handleGetConnector (the single-connector GET) was fixed to
// show the proxy path instead of the raw downstream MCP URL, but
// handleListConnectors (this one) leaked the raw URL through the same
// runner-token-reachable trust boundary -- missed the first time because
// TestConnector_ListConnectors above never configured a connector with
// MCP set, so it never exercised this field at all.
func TestConnector_ListConnectors_MCPShowsProxyPathNotRawURL(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"sf": {Auth: connector.AuthConfig{Type: "api_key", APIKey: "k"}, MCP: &connector.MCPConfig{URL: "https://sf-mcp.internal/sse"}},
	})

	resp, _ := http.Get(env.srv.URL + "/internal/connectors")
	expectStatus(t, resp, 200)
	body := readBody(t, resp)

	var list []map[string]interface{}
	json.Unmarshal(body, &list)
	if len(list) != 1 || list[0]["mcp"] != "/internal/connectors/sf/mcp" {
		t.Fatalf("expected the proxy path in the list response, got %v", list)
	}
	if strings.Contains(string(body), "sf-mcp.internal") {
		t.Fatal("connector list must not expose the raw downstream MCP URL")
	}
}

func TestConnector_ListConnectorsEmpty(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := http.Get(env.srv.URL + "/internal/connectors")
	expectStatus(t, resp, 200)

	var list []interface{}
	json.Unmarshal(readBody(t, resp), &list)
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}
}

func TestConnector_SessionAPIKey(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"myapi": {Auth: connector.AuthConfig{Type: "api_key", APIKey: "the-key"}},
	})

	resp, _ := postJSON(env.srv.URL+"/internal/connectors/myapi/session", map[string]interface{}{})
	expectStatus(t, resp, 200)

	var sess connector.SessionResponse
	json.Unmarshal(readBody(t, resp), &sess)
	if sess.Credentials["access_token"] != "the-key" {
		t.Fatalf("expected access_token=the-key, got %s", sess.Credentials["access_token"])
	}
}

// TestConnector_SessionOmitsCredentialsWhenMCPConfigured proves the fix
// for a real bypass found on review (plans/pending_items.md item 17):
// the MCP proxy alone wasn't a complete fix while the session response
// ALSO handed out the raw downstream access_token -- a misbehaving or
// compromised agent could just take that token and call the real server
// directly, skipping the proxy's tool allow/deny enforcement entirely.
func TestConnector_SessionOmitsCredentialsWhenMCPConfigured(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"sf": {
			Auth: connector.AuthConfig{Type: "api_key", APIKey: "raw-secret-should-not-leak"},
			MCP:  &connector.MCPConfig{URL: "https://sf-mcp.internal/sse"},
		},
	})

	resp, _ := postJSON(env.srv.URL+"/internal/connectors/sf/session", map[string]interface{}{})
	expectStatus(t, resp, 200)
	body := readBody(t, resp)

	var sess connector.SessionResponse
	json.Unmarshal(body, &sess)
	if sess.Credentials != nil {
		t.Fatalf("expected no credentials in the session response for an MCP-proxied connector, got %v", sess.Credentials)
	}
	if strings.Contains(string(body), "raw-secret-should-not-leak") {
		t.Fatal("raw downstream credential leaked into the session response despite MCP being configured")
	}
}

// TestConnector_SessionIncludesCredentialsWhenNoMCP proves the fix above
// didn't overreach -- a connector with no MCP endpoint at all (pure
// direct-API-token use, the only way the runner could ever use it) must
// still get its credentials normally.
func TestConnector_SessionIncludesCredentialsWhenNoMCP(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"myapi": {Auth: connector.AuthConfig{Type: "api_key", APIKey: "the-key"}},
	})

	resp, _ := postJSON(env.srv.URL+"/internal/connectors/myapi/session", map[string]interface{}{})
	expectStatus(t, resp, 200)
	var sess connector.SessionResponse
	json.Unmarshal(readBody(t, resp), &sess)
	if sess.Credentials["access_token"] != "the-key" {
		t.Fatalf("expected credentials for a non-MCP connector, got %v", sess.Credentials)
	}
}

func TestConnector_SessionNotFound(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"myapi": {Auth: connector.AuthConfig{Type: "api_key", APIKey: "k"}},
	})

	resp, _ := postJSON(env.srv.URL+"/internal/connectors/unknown/session", map[string]interface{}{})
	expectStatus(t, resp, 404)
}

func TestConnector_GetConnectorInfo(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"sf": {
			Auth:   connector.AuthConfig{Type: "api_key", APIKey: "secret-should-not-appear"},
			MCP:    &connector.MCPConfig{URL: "https://sf-mcp.internal/sse"},
			Errors: map[string]string{"INVALID_SESSION": "Session expired"},
		},
	})

	resp, _ := http.Get(env.srv.URL + "/internal/connectors/sf")
	expectStatus(t, resp, 200)

	var info map[string]interface{}
	json.Unmarshal(readBody(t, resp), &info)
	if info["name"] != "sf" {
		t.Fatalf("expected name=sf, got %v", info["name"])
	}
	if info["type"] != "api_key" {
		t.Fatalf("expected type=api_key, got %v", info["type"])
	}
	// The raw downstream MCP URL must NOT be exposed here -- this
	// endpoint is reachable with just a runner token, so leaking it
	// would let a misbehaving agent bypass the proxy's tool
	// allow/deny enforcement by connecting directly (found on review,
	// plans/pending_items.md item 17). The proxy path is shown instead.
	if info["mcp"] != "/internal/connectors/sf/mcp" {
		t.Fatalf("expected the proxy path, not the raw downstream URL, got %v", info["mcp"])
	}
	body, _ := json.Marshal(info)
	if strings.Contains(string(body), "sf-mcp.internal") {
		t.Fatal("connector info must not expose the raw downstream MCP URL")
	}
	// Secrets should NOT appear in response
	if strings.Contains(string(body), "secret-should-not-appear") {
		t.Fatal("connector info must not expose secrets")
	}
}

// TestConnector_PreWarmFromAgentMetadata guards against a regression where
// RunAssignment.ConnectorNeeds was hardcoded to an empty slice in createRun,
// making the entire pre-warm code path unreachable dead code regardless of
// how many connectors were registered. connector_needs must come from the
// agent's own declared config (models.Agent.Metadata["connector_needs"]),
// set at bootstrap time from langgraph.json, not from the client request.
func TestConnector_PreWarmFromAgentMetadata(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"salesforce": {Auth: connector.AuthConfig{Type: "api_key", APIKey: "sf-key"}},
	})

	// Simulate what bootstrapAgents does when langgraph.json declares
	// connector_needs for this agent.
	agent := &models.Agent{
		AgentID:  "needs-sf",
		Name:     "needs-sf",
		Metadata: map[string]interface{}{"connector_needs": []string{"salesforce"}},
	}
	if err := env.store.UpsertAgent(context.Background(), agent); err != nil {
		t.Fatal(err)
	}

	resp, _ := postJSON(env.srv.URL+"/threads/sf-thread/runs", map[string]interface{}{"agent_id": "needs-sf"})
	expectStatus(t, resp, 200)
	readBody(t, resp)

	assignment, err := env.queue.Dequeue(context.Background(), "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil {
		t.Fatalf("expected job in queue: %v", err)
	}
	if len(assignment.ConnectorNeeds) != 1 || assignment.ConnectorNeeds[0] != "salesforce" {
		t.Fatalf("expected connector_needs=[salesforce] on the dispatched assignment, got %v", assignment.ConnectorNeeds)
	}
}

// ============================================================================
// Connector MCP Proxy — makes IsToolAllowed a real enforcement gate
// (plans/pending_items.md item 17), tested end to end through the actual
// HTTP handler + a fake downstream MCP server, not just the registry
// method directly (internal/connector/mcpproxy_test.go covers that).
// ============================================================================

func TestConnector_MCPProxy_AllowedToolReachesDownstream(t *testing.T) {
	var toolCallHits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)
		if req["method"] == "tools/call" {
			atomic.AddInt32(&toolCallHits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": req["id"], "result": map[string]interface{}{"content": []interface{}{}},
		})
	}))
	t.Cleanup(fake.Close)

	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"sf": {
			Auth:  connector.AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:   &connector.MCPConfig{URL: fake.URL},
			Tools: &connector.ToolFilter{Allow: []string{"query"}},
		},
	})

	resp, _ := postJSON(env.srv.URL+"/internal/connectors/sf/mcp", map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]interface{}{"name": "query"},
	})
	expectStatus(t, resp, 200)
	body := readBody(t, resp)
	if atomic.LoadInt32(&toolCallHits) != 1 {
		t.Fatalf("expected downstream to be hit once for an allowed tool, got %d", toolCallHits)
	}
	var rpcResp map[string]interface{}
	json.Unmarshal(body, &rpcResp)
	if rpcResp["error"] != nil {
		t.Fatalf("expected no JSON-RPC error for an allowed tool, got %v", rpcResp["error"])
	}
}

func TestConnector_MCPProxy_DeniedToolNeverReachesDownstream(t *testing.T) {
	var toolCallHits int32
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&toolCallHits, 1)
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(fake.Close)

	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"sf": {
			Auth:  connector.AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:   &connector.MCPConfig{URL: fake.URL},
			Tools: &connector.ToolFilter{Allow: []string{"query"}},
		},
	})

	resp, _ := postJSON(env.srv.URL+"/internal/connectors/sf/mcp", map[string]interface{}{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]interface{}{"name": "deleteEverything"},
	})
	expectStatus(t, resp, 200) // JSON-RPC errors are still HTTP 200
	body := readBody(t, resp)
	if atomic.LoadInt32(&toolCallHits) != 0 {
		t.Fatalf("downstream MCP server must never be contacted for a denied tool call, got %d hits", toolCallHits)
	}
	var rpcResp map[string]interface{}
	json.Unmarshal(body, &rpcResp)
	if rpcResp["error"] == nil {
		t.Fatalf("expected a JSON-RPC error for a denied tool call, got %s", body)
	}
}

func TestConnector_MCPProxy_UnknownConnector(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"sf": {Auth: connector.AuthConfig{Type: "api_key", APIKey: "k"}},
	})
	resp, _ := postJSON(env.srv.URL+"/internal/connectors/nope/mcp", map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	expectStatus(t, resp, 404)
}

func TestConnector_MCPProxy_SessionURLPointsAtProxyNotRawDownstream(t *testing.T) {
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"sf": {
			Auth: connector.AuthConfig{Type: "api_key", APIKey: "k"},
			MCP:  &connector.MCPConfig{URL: "https://raw-downstream.internal/mcp"},
		},
	})
	resp, _ := postJSON(env.srv.URL+"/internal/connectors/sf/session", map[string]interface{}{})
	expectStatus(t, resp, 200)
	var sess connector.SessionResponse
	json.Unmarshal(readBody(t, resp), &sess)
	if sess.MCP == nil || sess.MCP.URL != "/internal/connectors/sf/mcp" {
		t.Fatalf("expected session to point at the proxy path, got %+v", sess.MCP)
	}
}

// ============================================================================
// Auth Integration: API key middleware wrapping the full API server
// ============================================================================

func TestAuth_APIKeyMiddleware(t *testing.T) {
	env := newTestEnv(t)
	seedAgent(t, env, "a1", "chatbot", nil)

	// Wrap handler with API key auth
	provider := auth.NewAPIKeyProvider(map[string]auth.APIKeyEntry{
		"test-key": {Name: "Tester", Permissions: []string{"read", "write"}},
	})
	env.srv.Close()
	env.srv = httptest.NewServer(auth.Middleware(provider, nil, nil, env.apiServer.Handler()))
	t.Cleanup(env.srv.Close)

	// 401 without key
	resp, _ := http.Get(env.srv.URL + "/agents/a1")
	expectStatus(t, resp, 401)
	readBody(t, resp)

	// 200 with key
	req, _ := http.NewRequest("GET", env.srv.URL+"/agents/a1", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	resp2, _ := http.DefaultClient.Do(req)
	expectStatus(t, resp2, 200)
	readBody(t, resp2)

	// /health always public (no key needed)
	resp3, _ := http.Get(env.srv.URL + "/health")
	expectStatus(t, resp3, 200)
	readBody(t, resp3)

	// /internal/* always public (no key needed)
	resp4, _ := http.Get(env.srv.URL + "/internal/connectors")
	expectStatus(t, resp4, 200)
	readBody(t, resp4)
}

func TestConnector_GetConnectorNotFound(t *testing.T) {
	env := newTestEnv(t)

	resp, _ := http.Get(env.srv.URL + "/internal/connectors/nope")
	expectStatus(t, resp, 404)
}

func TestConnector_ErrorTaxonomyInSessionFailure(t *testing.T) {
	// Register a connector with error mappings and an auth type that will fail
	// (oauth2_client_credentials with a bogus token_url).
	env := newTestEnvWithConnectors(t, map[string]connector.ConnectorConfig{
		"sf": {
			Auth: connector.AuthConfig{
				Type:         "oauth2_client_credentials",
				TokenURL:     "http://127.0.0.1:1/nonexistent", // will fail to connect
				ClientID:     "id",
				ClientSecret: "secret",
			},
			Errors: map[string]string{
				"connection refused": "Salesforce is currently unreachable. Try again later.",
			},
		},
	})

	resp, _ := postJSON(env.srv.URL+"/internal/connectors/sf/session", map[string]interface{}{})
	expectStatus(t, resp, 502)

	var errResp models.ErrorResponse
	json.Unmarshal(readBody(t, resp), &errResp)
	if !strings.Contains(errResp.Message, "Salesforce is currently unreachable") {
		t.Fatalf("expected error taxonomy message, got: %s", errResp.Message)
	}
}

func TestAG001_AgentBootstrapFromConfig(t *testing.T) {
	env := newTestEnv(t)

	// Simulate what cmd/main.go does: register agents from a config
	agent := &models.Agent{
		AgentID:      "my_chatbot",
		Name:         "my_chatbot",
		Description:  "from langgraph.json",
		Metadata:     map[string]interface{}{"source": "test"},
		Capabilities: map[string]interface{}{},
	}
	env.store.UpsertAgent(context.Background(), agent)
	env.store.UpsertAgentSchema(context.Background(), &models.AgentSchema{
		AgentID:      "my_chatbot",
		InputSchema:  map[string]interface{}{"type": "object"},
		OutputSchema: map[string]interface{}{"type": "object"},
	})

	// Now verify via HTTP API
	resp, _ := postJSON(env.srv.URL+"/agents/search", map[string]interface{}{})
	expectStatus(t, resp, 200)

	var agents []models.Agent
	json.Unmarshal(readBody(t, resp), &agents)
	found := false
	for _, a := range agents {
		if a.AgentID == "my_chatbot" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected to find my_chatbot in agent search results")
	}

	// GET by ID
	resp2, _ := http.Get(env.srv.URL + "/agents/my_chatbot")
	expectStatus(t, resp2, 200)

	var a models.Agent
	json.Unmarshal(readBody(t, resp2), &a)
	if a.AgentID != "my_chatbot" {
		t.Fatalf("expected agent_id=my_chatbot, got %s", a.AgentID)
	}
}
