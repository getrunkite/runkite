package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
)

// TestDispatchScheduledRun_UsesFixedThreadPerSchedule proves a
// cron-triggered run uses a deterministic "cron:<name>" thread, so a
// schedule's run history is browsable as one continuous thread, and that
// two dispatches of the SAME schedule land on the SAME thread.
func TestDispatchScheduledRun_UsesFixedThreadPerSchedule(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "report-agent")
	ctx := context.Background()

	run1, err := env.apiServer.DispatchScheduledRun(ctx, "daily-report", "report-agent",
		json.RawMessage(`{"type":"daily"}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("DispatchScheduledRun: %v", err)
	}
	if run1.ThreadID != "cron:daily-report" {
		t.Fatalf("expected fixed thread cron:daily-report, got %q", run1.ThreadID)
	}
	if run1.AgentID != "report-agent" {
		t.Errorf("expected agent_id report-agent, got %q", run1.AgentID)
	}

	// Simulate the first run completing so the thread isn't left "busy"
	// (TryClaimThread would otherwise reject the second dispatch below --
	// correctly so, since a real overlapping cron fire on a still-running
	// schedule SHOULD be rejected, same as any other thread).
	env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	env.apiServer.StatusCallback()(run1.RunID, "success", "")

	run2, err := env.apiServer.DispatchScheduledRun(ctx, "daily-report", "report-agent",
		json.RawMessage(`{"type":"daily"}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("DispatchScheduledRun (second fire): %v", err)
	}
	if run2.ThreadID != run1.ThreadID {
		t.Errorf("expected the same thread across fires of the same schedule, got %q vs %q", run2.ThreadID, run1.ThreadID)
	}
}

// TestDispatchScheduledRun_ActuallyEnqueues proves the dispatched run is a
// real job on the queue, not just a DB row -- a runner would actually pick
// it up.
func TestDispatchScheduledRun_ActuallyEnqueues(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "sync-agent")
	ctx := context.Background()

	run, err := env.apiServer.DispatchScheduledRun(ctx, "hourly-sync", "sync-agent",
		json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("DispatchScheduledRun: %v", err)
	}

	assignment, err := env.queue.Dequeue(ctx, "python-langgraph", 2*time.Second)
	if err != nil || assignment == nil || assignment.RunID != run.RunID {
		t.Fatalf("expected the scheduled run to be a real queued job: %v", err)
	}
}

// TestDispatchScheduledRun_DifferentSchedulesUseDifferentThreads proves
// isolation between schedules -- one schedule's history never leaks into
// another's thread.
func TestDispatchScheduledRun_DifferentSchedulesUseDifferentThreads(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "agent-x")
	ctx := context.Background()

	run1, err1 := env.apiServer.DispatchScheduledRun(ctx, "sched-a", "agent-x", json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err1 != nil {
		t.Fatalf("first dispatch: %v", err1)
	}
	run2, err2 := env.apiServer.DispatchScheduledRun(ctx, "sched-b", "agent-x", json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err2 != nil {
		t.Fatalf("second dispatch: %v", err2)
	}

	if run1.ThreadID == run2.ThreadID {
		t.Fatalf("expected different schedules to use different threads, both got %q", run1.ThreadID)
	}
}

// TestDispatchScheduledRun_RejectsOverlappingFire proves TS-009's
// thread-claim protection also applies to cron: if a previous fire's run
// on the same schedule is still in flight, a new fire for that schedule is
// rejected (not silently queued to run concurrently on the same thread).
func TestDispatchScheduledRun_RejectsOverlappingFire(t *testing.T) {
	env := newTestEnv(t)
	registerAgent(t, env, "agent-x")
	ctx := context.Background()

	_, err := env.apiServer.DispatchScheduledRun(ctx, "slow-job", "agent-x", json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	// First run never completes -- still "busy". A second fire for the
	// SAME schedule must be rejected, not double-dispatched.
	_, err = env.apiServer.DispatchScheduledRun(ctx, "slow-job", "agent-x", json.RawMessage(`{}`), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an overlapping fire on a still-busy schedule thread to be rejected")
	}
}

// TestHandleListCronSchedules_ReturnsRegisteredSchedules proves the
// GET /internal/cron introspection endpoint reflects what's actually in
// the store, the same operability contract GET /internal/connectors gives
// for connectors.
func TestHandleListCronSchedules_ReturnsRegisteredSchedules(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	if err := env.store.UpsertCronSchedule(ctx, &models.CronSchedule{
		Name: "nightly", AgentID: "report-agent", Expression: "0 2 * * *",
		Timezone: "UTC", Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`), Enabled: true,
	}); err != nil {
		t.Fatalf("UpsertCronSchedule: %v", err)
	}

	resp, err := http.Get(env.srv.URL + "/internal/cron")
	if err != nil {
		t.Fatalf("GET /internal/cron: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var schedules []models.CronSchedule
	if err := json.NewDecoder(resp.Body).Decode(&schedules); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(schedules) != 1 || schedules[0].Name != "nightly" || schedules[0].Expression != "0 2 * * *" {
		t.Fatalf("expected 1 schedule 'nightly', got %+v", schedules)
	}
}
