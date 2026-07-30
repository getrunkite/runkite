package e2e_test

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// TestVG001_ReactAgentToolCalls re-validates the VG-001 kill criterion --
// "the bridge can stream events cleanly from a real LangGraph
// graph without patching LangGraph internals" -- against the real
// production stack: real Postgres, real Redis, the actual `runkite` binary,
// and the actual Python runner, not the in-process spike harness this was
// originally proven against.
func TestVG001_ReactAgentToolCalls(t *testing.T) {
	resp := postJSON(t, "/threads", map[string]interface{}{})
	var thread map[string]interface{}
	decodeJSON(t, resp, &thread)
	threadID := thread["thread_id"].(string)

	resp = postJSON(t, "/threads/"+threadID+"/runs/stream", map[string]interface{}{
		"agent_id": "react_agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "gophers"}}},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	events := parseSSE(t, string(body))

	types := eventTypes(events)
	if len(events) == 0 {
		t.Fatalf("no SSE events received; raw body: %q", body)
	}
	if types[0] != "metadata" {
		t.Errorf("expected first event to be metadata, got %v", types)
	}
	if types[len(types)-1] != "end" {
		t.Errorf("expected last event to be end, got %v", types)
	}
	if !containsAll(types, []string{"lifecycle", "values", "end"}) {
		t.Fatalf("missing expected event types, got %v", types)
	}

	// The tool loop must have actually run: agent -> tools -> agent. Verify
	// by inspecting the final "values" event's messages for the fake tool's
	// deterministic output flowing through to the final AI response.
	var lastValues map[string]interface{}
	for _, e := range events {
		if e.Event == "values" {
			lastValues = e.Data
		}
	}
	if lastValues == nil {
		t.Fatal("no values event found")
	}
	msgsRaw, _ := json.Marshal(lastValues["messages"])
	msgs := string(msgsRaw)

	if !strings.Contains(msgs, "search") {
		t.Errorf("expected a tool_call to 'search' somewhere in message history, got: %s", msgs)
	}
	if !strings.Contains(msgs, "Based on my research") {
		t.Errorf("expected final AI response referencing tool result, got: %s", msgs)
	}
	if !strings.Contains(msgs, "42") {
		t.Errorf("expected the fake tool's deterministic result ('...answer is 42') to appear, got: %s", msgs)
	}

	// Final run status must be terminal-success via the real HTTP API.
	// The SSE stream closing only means the client has seen the runner's
	// terminal event; the runner's separate ReportStatus gRPC call (which
	// persists the run's status) can trail behind by a moment -- the same
	// documented eventual-consistency window as GET /threads/{id}/wait.
	// A well-behaved client polls rather than asserting immediately.
	var runs []map[string]interface{}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getJSON(t, "/threads/"+threadID+"/runs", &runs)
		if len(runs) == 1 {
			if s, _ := runs[0]["status"].(string); s == "success" || s == "error" || s == "interrupted" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(runs) != 1 {
		t.Fatalf("expected exactly 1 run on thread, got %d", len(runs))
	}
	if runs[0]["status"] != "success" {
		t.Errorf("expected run status success, got %v", runs[0]["status"])
	}
}
