package api_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
)

// postCommand posts a StreamingCommand to /threads/{threadID}/commands and
// decodes the StreamingCommandResponse -- the REST equivalent of the
// WebSocket command dispatcher already tested in websocket_test.go, used
// here because these tests don't need a live connection.
func postCommand(t *testing.T, env *testEnv, threadID string, cmd models.StreamingCommand) models.StreamingCommandResponse {
	t.Helper()
	resp, err := postJSON(env.srv.URL+"/threads/"+threadID+"/commands", cmd)
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	var out models.StreamingCommandResponse
	if err := json.Unmarshal(readBody(t, resp), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// interruptThread starts a run, dequeues it (so it has an assignment,
// matching a real in-flight run), then cancels it -- the simplest real
// path to an "interrupted" run/thread in this test harness (mirrors
// TestWebSocket_RunCancel's own proof that run.cancel flips both the run
// and its thread to interrupted).
func interruptThread(t *testing.T, env *testEnv, threadID, agentID string) string {
	t.Helper()
	ack := postCommand(t, env, threadID, models.StreamingCommand{
		ID: 1, Method: "run.start", Params: map[string]interface{}{"agent_id": agentID},
	})
	runID, _ := ack.Result["run_id"].(string)
	if runID == "" {
		t.Fatalf("run.start ack missing run_id: %+v", ack)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second); err != nil {
		t.Fatalf("dequeue: %v", err)
	}

	// run.cancel is only wired on the WebSocket command dispatcher, not
	// the REST /threads/{id}/commands endpoint used above for run.start
	// -- the plain REST cancel endpoint does the identical
	// cancelRunSingle work (see TestWebSocket_RunCancel's own comment).
	cancelResp, err := postJSON(env.srv.URL+"/runs/"+runID+"/cancel", nil)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	expectStatus(t, cancelResp, 200)
	run, err := env.store.GetRun(context.Background(), runID)
	if err != nil || run.Status != models.RunStatusInterrupted {
		t.Fatalf("expected run interrupted after cancel, got run=%+v err=%v", run, err)
	}
	return runID
}

// TestInputRespond_ResumesInterruptedRun_Succeeds is the happy path: a
// genuinely interrupted thread accepts input.respond and creates a new
// pending run carrying the resume command.
func TestInputRespond_ResumesInterruptedRun_Succeeds(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "test")
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "respond-thread-1"})

	interruptThread(t, env, "respond-thread-1", "test")

	resp := postCommand(t, env, "respond-thread-1", models.StreamingCommand{
		ID: 3, Method: "input.respond", Params: map[string]interface{}{"response": "yes"},
	})
	if resp.Type != "success" {
		t.Fatalf("expected input.respond to succeed against an interrupted thread, got %+v", resp)
	}
	newRunID, _ := resp.Result["run_id"].(string)
	if newRunID == "" {
		t.Fatal("expected a new run_id in the resume ack")
	}
	newRun, err := env.store.GetRun(context.Background(), newRunID)
	if err != nil || newRun.Status != models.RunStatusPending {
		t.Fatalf("expected a new pending run, got run=%+v err=%v", newRun, err)
	}
}

// TestInputRespond_NothingInterrupted_ReturnsCleanConflict: responding
// on a thread that was never interrupted (fresh thread, zero runs) must
// fail cleanly, not silently proceed with an empty agent_id that only
// fails later, confusingly, at agent lookup.
func TestInputRespond_NothingInterrupted_ReturnsCleanConflict(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "test")
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "respond-thread-2"})

	resp := postCommand(t, env, "respond-thread-2", models.StreamingCommand{
		ID: 1, Method: "input.respond", Params: map[string]interface{}{"response": "yes"},
	})
	if resp.Type != "error" || resp.Error != "resume_failed" {
		t.Fatalf("expected a clean resume_failed error for a thread with nothing interrupted, got %+v", resp)
	}
}

// TestInputRespond_RetryAfterAlreadyResumed_ReturnsCleanConflict covers
// a STALE respond: once the interrupt has already been answered (thread
// now has a pending/running resume, no longer interrupted), a
// duplicate/late respond -- e.g. a client retry after a dropped
// connection -- must not create a second, spurious resume run reusing
// the old response against whatever run is now latest.
func TestInputRespond_RetryAfterAlreadyResumed_ReturnsCleanConflict(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "test")
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "respond-thread-3"})

	interruptThread(t, env, "respond-thread-3", "test")

	first := postCommand(t, env, "respond-thread-3", models.StreamingCommand{
		ID: 3, Method: "input.respond", Params: map[string]interface{}{"response": "yes"},
	})
	if first.Type != "success" {
		t.Fatalf("expected the first respond to succeed, got %+v", first)
	}

	// The thread is now "busy" (a new pending/running resume owns it,
	// no longer interrupted) -- a retry of the exact same respond must
	// be rejected, not silently treated as answering something new.
	retry := postCommand(t, env, "respond-thread-3", models.StreamingCommand{
		ID: 4, Method: "input.respond", Params: map[string]interface{}{"response": "yes"},
	})
	if retry.Type != "error" {
		t.Fatalf("expected a stale respond retry to be rejected, got %+v", retry)
	}
}

// TestInputRespond_ConcurrentRespond_ExactlyOneSucceeds proves two
// genuinely concurrent input.respond calls against the SAME interrupted
// thread can't both create a resume run -- TryClaimThread's own atomicity
// (inside createRunCtx) ensures exactly one wins.
func TestInputRespond_ConcurrentRespond_ExactlyOneSucceeds(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "test")
	postJSON(env.srv.URL+"/threads", map[string]interface{}{"thread_id": "respond-thread-4"})

	interruptThread(t, env, "respond-thread-4", "test")

	var wg sync.WaitGroup
	results := make([]models.StreamingCommandResponse, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = postCommand(t, env, "respond-thread-4", models.StreamingCommand{
				ID: idx + 10, Method: "input.respond", Params: map[string]interface{}{"response": "yes"},
			})
		}(i)
	}
	wg.Wait()

	successes, errs := 0, 0
	for _, r := range results {
		if r.Type == "success" {
			successes++
		} else {
			errs++
		}
	}
	if successes != 1 || errs != 1 {
		t.Fatalf("expected exactly 1 success and 1 clean error for concurrent respond, got successes=%d errs=%d (%+v)", successes, errs, results)
	}
}
