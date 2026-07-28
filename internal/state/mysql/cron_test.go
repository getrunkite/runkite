package mysql_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sharanharsoor/runkite/internal/models"
)

// Mirrors the shared conformance suite's runCronScheduleTests.

func TestCron_UpsertAndListRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sched := &models.CronSchedule{
		Name: "daily-report", AgentID: "report-agent", Expression: "0 9 * * *",
		Timezone: "America/New_York", Input: json.RawMessage(`{"type":"daily"}`),
		Config: json.RawMessage(`{}`), Enabled: true,
	}
	if err := s.UpsertCronSchedule(ctx, sched); err != nil {
		t.Fatalf("UpsertCronSchedule: %v", err)
	}

	list, err := s.ListCronSchedules(ctx)
	if err != nil {
		t.Fatalf("ListCronSchedules: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 schedule, got %d", len(list))
	}
	got := list[0]
	if got.Name != "daily-report" || got.AgentID != "report-agent" || got.Expression != "0 9 * * *" {
		t.Errorf("schedule fields wrong: %+v", got)
	}
	if got.Timezone != "America/New_York" || !got.Enabled {
		t.Errorf("timezone/enabled wrong: %+v", got)
	}
	var input map[string]interface{}
	json.Unmarshal(got.Input, &input)
	if input["type"] != "daily" {
		t.Errorf("input not preserved: %s", got.Input)
	}
}

func TestCron_UpsertUpdatesExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertCronSchedule(ctx, &models.CronSchedule{
		Name: "sched-1", AgentID: "agent-a", Expression: "* * * * *", Enabled: true,
		Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("UpsertCronSchedule 1: %v", err)
	}
	if err := s.UpsertCronSchedule(ctx, &models.CronSchedule{
		Name: "sched-1", AgentID: "agent-b", Expression: "0 * * * *", Enabled: false,
		Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("UpsertCronSchedule 2: %v", err)
	}

	list, err := s.ListCronSchedules(ctx)
	if err != nil {
		t.Fatalf("ListCronSchedules: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected upsert to update, not duplicate: got %d schedules", len(list))
	}
	if list[0].AgentID != "agent-b" || list[0].Expression != "0 * * * *" || list[0].Enabled {
		t.Errorf("expected updated fields, got %+v", list[0])
	}
}

func TestCron_Delete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.UpsertCronSchedule(ctx, &models.CronSchedule{
		Name: "to-delete", AgentID: "a", Expression: "* * * * *",
		Input: json.RawMessage(`{}`), Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("UpsertCronSchedule: %v", err)
	}
	if err := s.DeleteCronSchedule(ctx, "to-delete"); err != nil {
		t.Fatalf("DeleteCronSchedule: %v", err)
	}
	list, err := s.ListCronSchedules(ctx)
	if err != nil {
		t.Fatalf("ListCronSchedules: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected schedule deleted, got %d remaining", len(list))
	}
}

func TestCron_ClaimFireExactlyOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fireTime := time.Now().UTC().Truncate(time.Minute)

	won, err := s.TryClaimCronFire(ctx, "sched-x", fireTime)
	if err != nil {
		t.Fatalf("TryClaimCronFire: %v", err)
	}
	if !won {
		t.Fatal("expected the first claim to win")
	}

	wonAgain, err := s.TryClaimCronFire(ctx, "sched-x", fireTime)
	if err != nil {
		t.Fatalf("TryClaimCronFire (second attempt): %v", err)
	}
	if wonAgain {
		t.Fatal("expected the second claim for the same (schedule, fire_time) to lose")
	}

	nextFireTime := fireTime.Add(time.Minute)
	wonNext, err := s.TryClaimCronFire(ctx, "sched-x", nextFireTime)
	if err != nil {
		t.Fatalf("TryClaimCronFire (next fire time): %v", err)
	}
	if !wonNext {
		t.Fatal("expected a claim for a different fire_time to succeed independently")
	}
}

// TestCron_ConcurrentClaimOnlyOneWins directly exercises the empirical
// claim underlying TryClaimCronFire's design (verified live before
// writing the implementation): MySQL's INSERT ... ON DUPLICATE KEY
// UPDATE reports RowsAffected()=1 for a genuine insert and =0 for a
// no-op update onto an existing row, so N concurrent instances racing
// to claim the exact same (schedule, fire_time) tick must produce
// exactly one winner, never zero and never more than one.
func TestCron_ConcurrentClaimOnlyOneWins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fireTime := time.Now().UTC().Truncate(time.Minute)

	const concurrency = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := s.TryClaimCronFire(ctx, "sched-concurrent", fireTime)
			if err != nil {
				t.Errorf("TryClaimCronFire: %v", err)
				return
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("expected exactly 1 winner among %d concurrent claims, got %d", concurrency, wins)
	}
}

func TestCron_ClaimIndependentPerSchedule(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fireTime := time.Now().UTC().Truncate(time.Minute)

	won1, err := s.TryClaimCronFire(ctx, "sched-a", fireTime)
	if err != nil {
		t.Fatalf("TryClaimCronFire (sched-a): %v", err)
	}
	won2, err := s.TryClaimCronFire(ctx, "sched-b", fireTime)
	if err != nil {
		t.Fatalf("TryClaimCronFire (sched-b): %v", err)
	}
	if !won1 || !won2 {
		t.Fatalf("expected independent schedules to claim the same fire_time independently: won1=%v won2=%v", won1, won2)
	}
}

func TestCron_ReleaseAllowsReclaim(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	fireTime := time.Now().UTC().Truncate(time.Minute)

	won, err := s.TryClaimCronFire(ctx, "sched-retry", fireTime)
	if err != nil || !won {
		t.Fatalf("initial claim: won=%v err=%v", won, err)
	}
	if err := s.ReleaseCronClaim(ctx, "sched-retry", fireTime); err != nil {
		t.Fatalf("ReleaseCronClaim: %v", err)
	}
	wonAgain, err := s.TryClaimCronFire(ctx, "sched-retry", fireTime)
	if err != nil {
		t.Fatalf("TryClaimCronFire after release: %v", err)
	}
	if !wonAgain {
		t.Fatal("expected claim to succeed again after ReleaseCronClaim")
	}
}

func TestCron_LastFireTimeNoneForNeverClaimed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, found, err := s.GetLastCronFireTime(ctx, "never-fired")
	if err != nil {
		t.Fatalf("GetLastCronFireTime: %v", err)
	}
	if found {
		t.Fatal("expected found=false for a schedule with no claims yet")
	}
}

func TestCron_LastFireTimeReturnsMostRecent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	t1 := time.Now().UTC().Truncate(time.Minute)
	t2 := t1.Add(time.Minute)
	t3 := t1.Add(2 * time.Minute)

	// Claim out of order to prove this is a MAX, not "last inserted".
	if _, err := s.TryClaimCronFire(ctx, "multi-fire", t2); err != nil {
		t.Fatalf("TryClaimCronFire(t2): %v", err)
	}
	if _, err := s.TryClaimCronFire(ctx, "multi-fire", t1); err != nil {
		t.Fatalf("TryClaimCronFire(t1): %v", err)
	}
	if _, err := s.TryClaimCronFire(ctx, "multi-fire", t3); err != nil {
		t.Fatalf("TryClaimCronFire(t3): %v", err)
	}

	last, found, err := s.GetLastCronFireTime(ctx, "multi-fire")
	if err != nil {
		t.Fatalf("GetLastCronFireTime: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if !last.Equal(t3) {
		t.Errorf("expected the latest claimed fire_time %v, got %v", t3, last)
	}
}
