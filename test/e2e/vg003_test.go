package e2e_test

import (
	"io"
	"strings"
	"testing"
	"time"
)

// TestVG003_HITLInterruptResume re-validates the master plan's VG-003 kill
// criterion -- interrupt() -> human approval -> Command(resume=...) works
// end to end -- against the real production stack (real Postgres for
// thread/run metadata and checkpoints, real Redis for the event broker,
// real gRPC bridge). The runner is started with POSTGRES_DSN set, so it
// uses checkpoint dual-mode's direct-Postgres AsyncPostgresSaver (see
// python/runkite_runner/checkpoint.py), not an in-memory checkpointer --
// this test alone doesn't prove restart-survival though, since the same
// runner process handles both the interrupt and the resume. See
// TestVG003b_ResumeSurvivesRunnerRestart for that.
func TestVG003_HITLInterruptResume(t *testing.T) {
	resp := postJSON(t, "/threads", map[string]interface{}{})
	var thread map[string]interface{}
	decodeJSON(t, resp, &thread)
	threadID := thread["thread_id"].(string)

	// Run 1: interrupt.
	resp = postJSON(t, "/threads/"+threadID+"/runs/stream", map[string]interface{}{
		"agent_id": "approval_agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "send the email"}}, "approved": false},
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	events := parseSSE(t, string(body))
	types := eventTypes(events)

	if !containsAll(types, []string{"lifecycle", "input.requested"}) {
		t.Fatalf("expected an interrupt (lifecycle + input.requested), got %v", types)
	}

	hasInterruptedEnd := false
	for _, e := range events {
		if e.Event == "end" && e.Data["status"] == "interrupted" {
			hasInterruptedEnd = true
		}
	}
	if !hasInterruptedEnd {
		t.Fatalf("expected run to end with status=interrupted, got events %v", types)
	}

	// Thread must be available again (idle/interrupted, not stuck busy)
	// before resuming.
	var threadState map[string]interface{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getJSON(t, "/threads/"+threadID, &threadState)
		if s, _ := threadState["status"].(string); s != "busy" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Run 2: resume with approval.
	resp = postJSON(t, "/threads/"+threadID+"/runs/wait", map[string]interface{}{
		"agent_id": "approval_agent",
		"command":  map[string]interface{}{"resume": true},
	})
	var result map[string]interface{}
	decodeJSON(t, resp, &result)

	run, _ := result["run"].(map[string]interface{})
	if run == nil || run["status"] != "success" {
		t.Fatalf("expected resumed run to succeed, got %v", result)
	}

	values, _ := result["values"].(map[string]interface{})
	if values == nil {
		t.Fatalf("expected values in resume response, got %v", result)
	}
	msgs, _ := values["messages"].([]interface{})
	if len(msgs) == 0 {
		t.Fatalf("expected messages in resumed run's final values, got %v", values)
	}
	last, _ := msgs[len(msgs)-1].(map[string]interface{})
	content, _ := last["content"].(string)
	if !strings.Contains(content, "sent") && !strings.Contains(content, "successfully") {
		t.Fatalf("expected final message to confirm the action completed, got: %q", content)
	}
}
