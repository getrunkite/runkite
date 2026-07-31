package matrix

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// uniqueContent avoids a subtle cross-run cache collision: unlike
// sqlite_inprocess's fresh :memory: database, the postgres_redis/
// mysql_inprocess/mongo_redis backends point at the same persistent
// shared test database every run (see docker-compose.test.yml). echo_agent
// has a 1-hour LLM cache configured (examples/all_agents/langgraph.json's
// llm_cache), keyed on exact input content -- a fixed literal string
// would hit a leftover cache entry from an *earlier* matrix run within
// that hour and silently take the cache-hit SSE path
// (streamCacheHitRun's 2-event shape) instead of a full run, corrupting
// the golden fixture for reasons that have nothing to do with the
// scenario itself. A per-process-unique string sidesteps this
// entirely; NormalizedRun only captures event *types* and final status,
// never exact message content, so varying it doesn't affect what the
// golden fixture asserts.
var contentCounter atomic.Int64

func uniqueContent(label string) string {
	return fmt.Sprintf("%s (matrix run, pid=%d, seq=%d)", label, os.Getpid(), contentCounter.Add(1))
}

type ScenarioKind string

const (
	// ScenarioHappyPath: create a thread, run to completion, verify a
	// clean event sequence ending in success. Every framework's example
	// agent supports this.
	ScenarioHappyPath ScenarioKind = "happy_path"
	// ScenarioCancel: start a long-running agent, cancel it mid-flight,
	// verify the run lands on "interrupted" (not "success" -- that would
	// mean cancellation had no effect) and the thread recovers instead
	// of staying stuck "busy". LangGraph-only today (slow_agent).
	ScenarioCancel ScenarioKind = "cancel"
	// ScenarioHITL: run an agent that calls interrupt() and pauses for
	// human approval, verify the run reaches "interrupted" with the
	// interrupt payload visible, then resume it and verify it completes.
	// LangGraph-only today (approval_agent).
	ScenarioHITL ScenarioKind = "hitl"
)

// runScenario dispatches to the right scenario implementation and
// returns a NormalizedRun ready for golden comparison.
func runScenario(t *testing.T, c *cell, kind ScenarioKind) NormalizedRun {
	t.Helper()
	switch kind {
	case ScenarioHappyPath:
		return scenarioHappyPath(t, c)
	case ScenarioCancel:
		return scenarioCancel(t, c)
	case ScenarioHITL:
		return scenarioHITL(t, c)
	default:
		t.Fatalf("unknown scenario kind %q", kind)
		return NormalizedRun{}
	}
}

// scenarioHappyPath creates a thread and streams a run to completion via
// POST /threads/{id}/runs/stream (SSE inline in the response body, see
// ../vg001_test.go for the established pattern this mirrors), for every
// framework's single-shot example agent.
func scenarioHappyPath(t *testing.T, c *cell) NormalizedRun {
	t.Helper()
	threadID := c.createThread(t)
	resp := c.postJSON(t, "/threads/"+threadID+"/runs/stream", map[string]any{
		"agent_id": c.runner.AgentID,
		"input":    map[string]any{"messages": []map[string]string{{"role": "user", "content": uniqueContent("hello from the test matrix")}}},
	})
	defer resp.Body.Close()
	events := parseSSE(t, resp)
	return normalize(events)
}

// scenarioCancel starts slow_agent (a ~6s, 3-step graph), cancels it
// after ~2s, and verifies the run is interrupted rather than completing,
// the thread recovers instead of staying stuck "busy", and -- the real
// proof of recovery, not just a status-field check -- a second run on
// the same thread actually succeeds afterward. Exact mirror of
// ../vg002_test.go's proven sequence (including its "step": 0 input
// field and 2s cancel delay), run against every backend combination
// instead of just Postgres+Redis.
func scenarioCancel(t *testing.T, c *cell) NormalizedRun {
	t.Helper()
	threadID := c.createThread(t)
	resp := c.postJSON(t, "/threads/"+threadID+"/runs", map[string]any{
		"agent_id": c.runner.CancelAgentID,
		"input":    map[string]any{"messages": []map[string]string{{"role": "user", "content": "go"}}, "step": 0},
	})
	var run map[string]any
	c.decodeJSON(t, resp, &run)
	runID, _ := run["run_id"].(string)
	if runID == "" {
		t.Fatalf("no run_id in response: %v", run)
	}

	time.Sleep(2 * time.Second)
	cancelResp := c.postJSON(t, "/threads/"+threadID+"/runs/"+runID+"/cancel", map[string]any{})
	cancelResp.Body.Close()

	finalStatus := c.pollRunStatus(t, threadID, runID, 10*time.Second, "interrupted", "success", "error")

	// The thread must recover, not stay stuck "busy" -- same regression
	// class VG-002 guards against for the LangGraph runner directly.
	c.pollThreadNotBusy(t, threadID, 5*time.Second)

	// A second run on the same thread must succeed -- proves the thread
	// truly recovered, not just that its status field happens to read
	// non-busy (the exact check VG-002 itself relies on).
	resp = c.postJSON(t, "/threads/"+threadID+"/runs/wait", map[string]any{
		"agent_id": c.runner.AgentID,
		"input":    map[string]any{"messages": []map[string]string{{"role": "user", "content": "still alive?"}}},
	})
	var waitResult map[string]any
	c.decodeJSON(t, resp, &waitResult)
	recoveryRun, _ := waitResult["run"].(map[string]any)
	if recoveryRun == nil || recoveryRun["status"] != "success" {
		t.Fatalf("thread did not truly recover after cancel -- second run failed: %v\n%s", waitResult, c.diagnostics())
	}

	return NormalizedRun{
		EventTypes:  []string{"cancel_requested", "recovery_run_succeeded"},
		FinalStatus: finalStatus,
	}
}

// scenarioHITL runs approval_agent, which calls LangGraph's interrupt()
// and pauses -- verifies the run reaches "interrupted" (visible via
// input.requested + a terminal end{status:interrupted} in the SSE
// stream), then resumes via POST /runs/wait with {command:{resume:true}}
// (the actual resume convention -- NOT a fresh run with a decision
// payload) and verifies it completes. Exact mirror of the proven
// ../vg003_test.go sequence, run against every backend combination.
func scenarioHITL(t *testing.T, c *cell) NormalizedRun {
	t.Helper()
	threadID := c.createThread(t)
	resp := c.postJSON(t, "/threads/"+threadID+"/runs/stream", map[string]any{
		"agent_id": c.runner.ApprovalAgent,
		"input": map[string]any{
			"messages": []map[string]string{{"role": "user", "content": "send the email"}},
			"approved": false,
		},
	})
	events := parseSSE(t, resp)
	resp.Body.Close()
	types := eventTypesOf(events)

	if !containsAll(types, []string{"lifecycle", "input.requested"}) {
		t.Fatalf("expected an interrupt (lifecycle + input.requested), got %v\n%s", types, c.diagnostics())
	}
	hasInterruptedEnd := false
	for _, e := range events {
		if e.Event == "end" && e.Data["status"] == "interrupted" {
			hasInterruptedEnd = true
		}
	}
	if !hasInterruptedEnd {
		t.Fatalf("expected run to end with status=interrupted, got events %v\n%s", types, c.diagnostics())
	}

	c.pollThreadNotBusy(t, threadID, 5*time.Second)

	resumeResp := c.postJSON(t, "/threads/"+threadID+"/runs/wait", map[string]any{
		"agent_id": c.runner.ApprovalAgent,
		"command":  map[string]any{"resume": true},
	})
	var result map[string]any
	c.decodeJSON(t, resumeResp, &result)
	resumedRun, _ := result["run"].(map[string]any)
	finalStatus, _ := resumedRun["status"].(string)
	if finalStatus != "success" {
		t.Fatalf("expected resumed run to succeed, got %v\n%s", result, c.diagnostics())
	}

	return NormalizedRun{
		EventTypes:  []string{"lifecycle", "input.requested", "end", "resume_via_wait"},
		FinalStatus: finalStatus,
	}
}

func eventTypesOf(events []sseEvent) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Event
	}
	return types
}

func containsAll(haystack, needles []string) bool {
	set := make(map[string]bool, len(haystack))
	for _, h := range haystack {
		set[h] = true
	}
	for _, n := range needles {
		if !set[n] {
			return false
		}
	}
	return true
}

func (c *cell) createThread(t *testing.T) string {
	t.Helper()
	resp := c.postJSON(t, "/threads", map[string]any{})
	var thread map[string]any
	c.decodeJSON(t, resp, &thread)
	id, _ := thread["thread_id"].(string)
	if id == "" {
		t.Fatalf("no thread_id in response: %v", thread)
	}
	return id
}

func (c *cell) pollRunStatus(t *testing.T, threadID, runID string, timeout time.Duration, terminalStatuses ...string) string {
	t.Helper()
	terminal := make(map[string]bool, len(terminalStatuses))
	for _, s := range terminalStatuses {
		terminal[s] = true
	}
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		c.getJSON(t, "/threads/"+threadID+"/runs/"+runID, &last)
		if s, _ := last["status"].(string); terminal[s] {
			return s
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("run %s never reached a terminal status within %s, last seen: %v\n%s", runID, timeout, last, c.diagnostics())
	return ""
}

func (c *cell) pollThreadNotBusy(t *testing.T, threadID string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last map[string]any
	for time.Now().Before(deadline) {
		c.getJSON(t, "/threads/"+threadID, &last)
		if s, _ := last["status"].(string); s != "busy" {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("thread %s stuck busy after cancel -- would permanently block future runs\n%s", threadID, c.diagnostics())
}

// sseEvent mirrors ../e2e_test.go's own parseSSE type -- duplicated
// rather than exported/shared across packages since e2e_test.go is
// package e2e_test (unexported) and this matrix is deliberately a
// separate, independently runnable package (own TestMain, own ports;
// see spec.go's doc comment).
type sseEvent struct {
	Event string
	Data  map[string]any
}

func parseSSE(t *testing.T, resp *http.Response) []sseEvent {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read SSE body: %v", err)
	}
	var events []sseEvent
	for _, block := range strings.Split(string(body), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev sseEvent
		for _, line := range strings.Split(block, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev.Event = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				json.Unmarshal([]byte(v), &ev.Data)
			}
		}
		if ev.Event != "" {
			events = append(events, ev)
		}
	}
	return events
}
