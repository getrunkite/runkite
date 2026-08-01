package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/hooks"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/transport"
)

type testSink struct {
	mu     sync.Mutex
	events []hooks.Event
}

func (s *testSink) Handle(_ context.Context, event hooks.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *testSink) snapshot() []hooks.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]hooks.Event, len(s.events))
	copy(out, s.events)
	return out
}

func waitForHookCount(t *testing.T, sink *testSink, want int) []hooks.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if events := sink.snapshot(); len(events) >= want {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d hook events, got %d", want, len(sink.snapshot()))
	return nil
}

// TestHooks_RunStartFiresOnCreate proves createRun (the single choke point
// for REST/WS/streaming-command run starts) fires on_run_start with the
// right run/thread/agent identifiers.
func TestHooks_RunStartFiresOnCreate(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "hook-agent")
	sink := &testSink{}
	d := hooks.NewDispatcher()
	d.Register(sink, hooks.RunStart)
	env.apiServer.SetHookDispatcher(d)

	resp, _ := postJSON(env.srv.URL+"/threads/hook-thread/runs", map[string]interface{}{"agent_id": "hook-agent"})
	expectStatus(t, resp, 200)
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)

	events := waitForHookCount(t, sink, 1)
	if events[0].Type != hooks.RunStart {
		t.Fatalf("expected RunStart, got %+v", events[0])
	}
	if events[0].RunID != run.RunID || events[0].ThreadID != "hook-thread" || events[0].AgentID != "hook-agent" {
		t.Errorf("hook event fields wrong: %+v", events[0])
	}
}

// TestHooks_RunCompleteFiresOnSuccess proves StatusCallback fires
// on_run_complete (not on_error/on_interrupt) for a successful run.
func TestHooks_RunCompleteFiresOnSuccess(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "hook-agent-2")
	sink := &testSink{}
	d := hooks.NewDispatcher()
	d.Register(sink) // subscribe to everything
	env.apiServer.SetHookDispatcher(d)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "hook-agent-2"})
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)

	env.apiServer.StatusCallback()(run.RunID, "success", "")

	events := waitForHookCount(t, sink, 2) // run_start + run_complete
	var gotComplete bool
	for _, e := range events {
		if e.Type == hooks.RunComplete {
			gotComplete = true
			if e.Data["status"] != "success" {
				t.Errorf("expected data.status=success, got %+v", e.Data)
			}
		}
		if e.Type == hooks.Error || e.Type == hooks.Interrupt {
			t.Errorf("unexpected hook type for a successful run: %s", e.Type)
		}
	}
	if !gotComplete {
		t.Fatal("expected a RunComplete hook event")
	}
}

// TestHooks_ErrorAndInterruptMapCorrectly proves StatusCallback maps
// status=error -> hooks.Error and status=interrupted -> hooks.Interrupt,
// not both firing as generic "complete".
func TestHooks_ErrorAndInterruptMapCorrectly(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "hook-agent-3")
	sink := &testSink{}
	d := hooks.NewDispatcher()
	d.Register(sink, hooks.Error, hooks.Interrupt)
	env.apiServer.SetHookDispatcher(d)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "hook-agent-3"})
	var run1 models.Run
	json.Unmarshal(readBody(t, resp), &run1)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	env.apiServer.StatusCallback()(run1.RunID, "error", "boom")

	resp2, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "hook-agent-3"})
	var run2 models.Run
	json.Unmarshal(readBody(t, resp2), &run2)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	env.apiServer.StatusCallback()(run2.RunID, "interrupted", "")

	events := waitForHookCount(t, sink, 2)
	byType := map[hooks.EventType]hooks.Event{}
	for _, e := range events {
		byType[e.Type] = e
	}
	errEvent, ok := byType[hooks.Error]
	if !ok || errEvent.RunID != run1.RunID || errEvent.Data["error"] != "boom" {
		t.Errorf("expected Error hook for run1 with error message, got %+v", byType)
	}
	intEvent, ok := byType[hooks.Interrupt]
	if !ok || intEvent.RunID != run2.RunID {
		t.Errorf("expected Interrupt hook for run2, got %+v", byType)
	}
}

// TestHooks_ToolCallFiresFromEventStream proves a runner-emitted RunEvent
// with method="tool_call" is surfaced as an on_tool_call hook, without the
// control plane needing to understand any framework-specific message shape.
func TestHooks_ToolCallFiresFromEventStream(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "tool-agent")
	sink := &testSink{}
	d := hooks.NewDispatcher()
	d.Register(sink, hooks.ToolCall)
	env.apiServer.SetHookDispatcher(d)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/threads/tool-call-thread/runs", map[string]interface{}{"agent_id": "tool-agent"})
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)

	env.broker.Publish(ctx, run.RunID, &transport.RunEvent{
		EventID: "evt_1", Seq: 1, Method: "tool_call",
		Namespace: []string{}, Data: json.RawMessage(`{"tool":"search","args":{"q":"weather"}}`), Ts: time.Now().UnixMilli(),
	})

	events := waitForHookCount(t, sink, 1)
	if events[0].RunID != run.RunID || events[0].ThreadID != "tool-call-thread" || events[0].AgentID != "tool-agent" {
		t.Errorf("tool_call hook fields wrong: %+v", events[0])
	}
	if events[0].Data["tool"] != "search" {
		t.Errorf("expected tool_call data to carry the event payload, got %+v", events[0].Data)
	}
}

// TestHooks_CancelFiresInterrupt proves the REST cancel path (which sets
// terminal status directly, independent of the runner ever reporting back)
// fires on_interrupt -- a real gap found via live end-to-end testing where
// cancelRunCore never dispatched any hook at all.
func TestHooks_CancelFiresInterrupt(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "cancel-hook-agent")
	sink := &testSink{}
	d := hooks.NewDispatcher()
	d.Register(sink, hooks.Interrupt)
	env.apiServer.SetHookDispatcher(d)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "cancel-hook-agent"})
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)

	cancelResp, _ := postJSON(env.srv.URL+"/runs/"+run.RunID+"/cancel", nil)
	expectStatus(t, cancelResp, 200)

	events := waitForHookCount(t, sink, 1)
	if events[0].Type != hooks.Interrupt || events[0].RunID != run.RunID {
		t.Fatalf("expected Interrupt hook for cancelled run, got %+v", events[0])
	}
}

// TestHooks_CancelThenStatusCallback_FiresExactlyOnce proves that if a run
// is cancelled AND the runner separately reports its own final status
// afterwards (a real, expected race -- the runner may not learn about the
// cancel until mid-flight), the terminal hook fires exactly once, not
// twice. A webhook consumer must never see a duplicate delivery for the
// same run's completion.
func TestHooks_CancelThenStatusCallback_FiresExactlyOnce(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "race-hook-agent")
	sink := &testSink{}
	d := hooks.NewDispatcher()
	d.Register(sink, hooks.Interrupt, hooks.RunComplete, hooks.Error)
	env.apiServer.SetHookDispatcher(d)
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "race-hook-agent"})
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)

	cancelResp, _ := postJSON(env.srv.URL+"/runs/"+run.RunID+"/cancel", nil)
	expectStatus(t, cancelResp, 200)
	waitForHookCount(t, sink, 1)

	// Runner didn't get the memo in time and reports its own status anyway.
	env.apiServer.StatusCallback()(run.RunID, "success", "")

	// Give any (incorrect) second dispatch a moment to land before asserting.
	time.Sleep(100 * time.Millisecond)
	events := sink.snapshot()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 terminal hook dispatch, got %d: %+v", len(events), events)
	}
	if events[0].Type != hooks.Interrupt {
		t.Errorf("expected the FIRST (cancel) outcome to win, got %s", events[0].Type)
	}
}

type denyGate struct{ reason string }

func (g denyGate) Decide(context.Context, hooks.Event) hooks.Decision {
	return hooks.Decision{Allow: false, Reason: g.reason}
}

type allowGate struct{ saw atomic.Bool }

func (g *allowGate) Decide(_ context.Context, ev hooks.Event) hooks.Decision {
	g.saw.Store(true)
	if ev.Type != hooks.BeforeRun {
		return hooks.Decision{Allow: false, Reason: "wrong type"}
	}
	if ev.AgentID == "" || ev.ThreadID == "" {
		return hooks.Decision{Allow: false, Reason: "missing ids"}
	}
	return hooks.Decision{Allow: true}
}

// TestPreflight_DenyBlocksCreate proves a sync Gate deny returns 403 and
// leaves no run row, no auto-created thread (even for a brand-new
// thread_id with if_not_exists=create), and no observational run_start.
func TestPreflight_DenyBlocksCreate(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "pf-deny-agent")
	sink := &testSink{}
	d := hooks.NewDispatcher()
	d.Register(sink, hooks.RunStart)
	d.RegisterGate(denyGate{reason: "pii detected"})
	env.apiServer.SetHookDispatcher(d)

	threadID := "pf-deny-brand-new-" + t.Name()
	resp, _ := postJSON(env.srv.URL+"/threads/"+threadID+"/runs", map[string]interface{}{
		"agent_id": "pf-deny-agent",
		"input":    map[string]interface{}{"messages": []map[string]string{{"role": "user", "content": "secret"}}},
	})
	expectStatus(t, resp, 403)
	body := string(readBody(t, resp))
	if !strings.Contains(body, "pii detected") {
		t.Fatalf("expected deny reason in body, got %s", body)
	}

	time.Sleep(100 * time.Millisecond)
	if n := len(sink.snapshot()); n != 0 {
		t.Fatalf("run_start must not fire on deny, got %d events", n)
	}
	// Gate runs before "ensure thread exists" — a deny must not leave an
	// idle thread behind for a thread_id that never existed.
	thResp, err := http.Get(env.srv.URL + "/threads/" + threadID)
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, thResp, 404)
}

// TestPreflight_AllowProceeds proves gates that allow still create the run
// and fire observational run_start afterward.
func TestPreflight_AllowProceeds(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "pf-allow-agent")
	sink := &testSink{}
	gate := &allowGate{}
	d := hooks.NewDispatcher()
	d.Register(sink, hooks.RunStart)
	d.RegisterGate(gate)
	env.apiServer.SetHookDispatcher(d)

	resp, _ := postJSON(env.srv.URL+"/threads/pf-allow/runs", map[string]interface{}{"agent_id": "pf-allow-agent"})
	expectStatus(t, resp, 200)
	if !gate.saw.Load() {
		t.Fatal("preflight gate was not consulted")
	}
	waitForHookCount(t, sink, 1)
}

// TestHooks_NoDispatcherIsSafe proves the default (no SetHookDispatcher
// call, matching production with no webhooks configured) never breaks run
// creation or completion.
func TestHooks_NoDispatcherIsSafe(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "no-hooks-agent")
	ctx := context.Background()

	resp, _ := postJSON(env.srv.URL+"/runs", map[string]interface{}{"agent_id": "no-hooks-agent"})
	expectStatus(t, resp, 200)
	var run models.Run
	json.Unmarshal(readBody(t, resp), &run)
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	env.apiServer.StatusCallback()(run.RunID, "success", "")
}
