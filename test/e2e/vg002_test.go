package e2e_test

import (
	"testing"
	"time"
)

// TestVG002_CancelMidExecution re-validates the VG-002 kill criterion --
// cancel mid-execution actually stops the runner and the
// thread doesn't get stuck -- against the real production stack. slow_agent
// is 3 sequential 2s steps (~6s total); cancelling at ~2s should land
// during/after step 1, well before natural completion.
func TestVG002_CancelMidExecution(t *testing.T) {
	resp := postJSON(t, "/threads", map[string]interface{}{})
	var thread map[string]interface{}
	decodeJSON(t, resp, &thread)
	threadID := thread["thread_id"].(string)

	resp = postJSON(t, "/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "slow_agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "go"}}, "step": 0},
	})
	var run map[string]interface{}
	decodeJSON(t, resp, &run)
	runID, _ := run["run_id"].(string)
	if runID == "" {
		t.Fatalf("no run_id in response: %v", run)
	}

	time.Sleep(2 * time.Second)

	cancelResp := postJSON(t, "/threads/"+threadID+"/runs/"+runID+"/cancel", map[string]interface{}{})
	cancelResp.Body.Close()
	if cancelResp.StatusCode != 200 {
		t.Fatalf("cancel request failed with status %d", cancelResp.StatusCode)
	}

	// Poll for terminal status (real gRPC cancel signal -> runner stop ->
	// ReportStatus round trip takes a moment).
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
		t.Fatalf("expected run status 'interrupted' after cancel, got %v (this must NOT be 'success' -- that would mean cancel had no effect and the agent ran to completion)", finalRun["status"])
	}

	// The thread-stuck-busy-forever regression found earlier this project:
	// after a run reaches a terminal state (including via cancel), the
	// thread must return to idle/interrupted, not remain permanently busy.
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

	// A second run on the same thread must succeed -- proves the thread
	// truly recovered, not just that its status field happens to read
	// non-busy.
	resp = postJSON(t, "/threads/"+threadID+"/runs/wait", map[string]interface{}{
		"agent_id": "echo_agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "still alive?"}}},
	})
	var waitResult map[string]interface{}
	decodeJSON(t, resp, &waitResult)
	runResult, _ := waitResult["run"].(map[string]interface{})
	if runResult == nil || runResult["status"] != "success" {
		t.Fatalf("expected second run on the thread to succeed after cancel, got %v", waitResult)
	}
}
