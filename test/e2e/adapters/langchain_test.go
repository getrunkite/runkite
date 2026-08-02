package adapters_test

import (
	"testing"
	"time"
)

// TestLangChainAdapter_RunsToCompletion proves the plain-LangChain
// adapter works through the real Runner Protocol end to end: a real
// control plane dispatches a job over real gRPC to a real
// langchain_adapter runner subprocess, which invokes a real (if
// deliberately slow) LangChain Runnable and streams results back.
func TestLangChainAdapter_RunsToCompletion(t *testing.T) {
	resp := postJSON(t, "/threads", map[string]interface{}{})
	var thread map[string]interface{}
	decodeJSON(t, resp, &thread)
	threadID := thread["thread_id"].(string)

	resp = postJSON(t, "/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "slow_langchain_agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "hello"}}},
	})
	var run map[string]interface{}
	decodeJSON(t, resp, &run)
	runID, _ := run["run_id"].(string)
	if runID == "" {
		t.Fatalf("no run_id in response: %v", run)
	}

	var finalRun map[string]interface{}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		getJSON(t, "/threads/"+threadID+"/runs/"+runID, &finalRun)
		if s, _ := finalRun["status"].(string); s == "success" || s == "error" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if finalRun["status"] != "success" {
		t.Fatalf("expected run to succeed, got %v\n--- runner log ---\n%s", finalRun, currentRunnerLog())
	}

	// Poll for thread.values (not only run status). StatusCallback writes
	// values before flipping status, but keep a short wait so a slow store
	// write cannot flake this assertion under CI load.
	var values map[string]interface{}
	var messages []interface{}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var threadState map[string]interface{}
		getJSON(t, "/threads/"+threadID, &threadState)
		values, _ = threadState["values"].(map[string]interface{})
		messages, _ = values["messages"].([]interface{})
		if len(messages) == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages (human + ai reply) in thread values, got %v\n--- runner log ---\n%s", values, currentRunnerLog())
	}
	lastMsg, _ := messages[1].(map[string]interface{})
	if content, _ := lastMsg["content"].(string); content == "" {
		t.Fatalf("expected a non-empty AI reply from the real Runnable, got %v", lastMsg)
	}
}

// TestLangChainAdapter_CancelMidExecution is the adapter equivalent of
// ../vg002_test.go's TestVG002_CancelMidExecution -- proves cancellation
// (generic_worker.run_cancellable) actually stops a real langchain_adapter
// runner mid-execution through a real gRPC WatchCancels signal, not just
// the unit-level cancel_event mock in python/tests/test_langchain_adapter.py.
func TestLangChainAdapter_CancelMidExecution(t *testing.T) {
	resp := postJSON(t, "/threads", map[string]interface{}{})
	var thread map[string]interface{}
	decodeJSON(t, resp, &thread)
	threadID := thread["thread_id"].(string)

	resp = postJSON(t, "/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "slow_langchain_agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "hello"}}},
	})
	var run map[string]interface{}
	decodeJSON(t, resp, &run)
	runID, _ := run["run_id"].(string)
	if runID == "" {
		t.Fatalf("no run_id in response: %v", run)
	}

	// slow_langchain_agent sleeps 6s -- cancelling at ~1s lands well
	// before natural completion.
	time.Sleep(1 * time.Second)

	cancelResp := postJSON(t, "/threads/"+threadID+"/runs/"+runID+"/cancel", map[string]interface{}{})
	cancelResp.Body.Close()
	if cancelResp.StatusCode != 200 {
		t.Fatalf("cancel request failed with status %d", cancelResp.StatusCode)
	}

	var finalRun map[string]interface{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		getJSON(t, "/threads/"+threadID+"/runs/"+runID, &finalRun)
		if s, _ := finalRun["status"].(string); s == "interrupted" || s == "success" || s == "error" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if finalRun["status"] != "interrupted" {
		t.Fatalf("expected run status 'interrupted' after cancel, got %v (this must NOT be 'success' -- that would mean cancel had no effect and the chain ran to completion)\n--- runner log ---\n%s", finalRun["status"], currentRunnerLog())
	}

	// Thread must recover (not stuck busy) -- same regression class VG-002
	// covers for the LangGraph runner.
	var threadAfter map[string]interface{}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getJSON(t, "/threads/"+threadID, &threadAfter)
		if s, _ := threadAfter["status"].(string); s != "busy" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if s, _ := threadAfter["status"].(string); s == "busy" {
		t.Fatalf("thread stuck busy after cancelled run -- would permanently block all future runs on this thread")
	}
}
