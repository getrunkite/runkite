package main

import (
	"context"
	"testing"
	"time"

	cronlib "github.com/robfig/cron/v3"

	sqlitestore "github.com/runkite/runkite/internal/state/sqlite"
)

func mustParse(t *testing.T, expr string) cronlib.Schedule {
	t.Helper()
	s, err := cronlib.ParseStandard(expr)
	if err != nil {
		t.Fatalf("ParseStandard(%q): %v", expr, err)
	}
	return s
}

func TestCheckSchedule_NotDueYet(t *testing.T) {
	c := &compiledCronSchedule{schedule: mustParse(t, "0 9 * * *"), location: time.UTC} // daily at 09:00

	last := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 8, 30, 0, 0, time.UTC) // before 09:00
	due, _ := checkSchedule(c, last, now)
	if due {
		t.Fatal("expected not due before the scheduled time")
	}
}

func TestCheckSchedule_DueExactlyOnce(t *testing.T) {
	c := &compiledCronSchedule{schedule: mustParse(t, "0 9 * * *"), location: time.UTC}

	last := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 1, 9, 5, 0, 0, time.UTC) // just past 09:00
	due, fireTime := checkSchedule(c, last, now)
	if !due {
		t.Fatal("expected due once the scheduled time has passed")
	}
	want := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if !fireTime.Equal(want) {
		t.Errorf("fireTime = %v, want %v", fireTime, want)
	}
}

// TestCheckSchedule_CatchesUpToLatestOnly proves multiple missed fires
// (e.g. the process was down for 3 days) collapse to a single dispatch for
// the LATEST due time, not a backlog of every missed day.
func TestCheckSchedule_CatchesUpToLatestOnly(t *testing.T) {
	c := &compiledCronSchedule{schedule: mustParse(t, "0 9 * * *"), location: time.UTC}

	last := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 1, 4, 12, 0, 0, 0, time.UTC) // 3 missed 09:00 fires in between
	due, fireTime := checkSchedule(c, last, now)
	if !due {
		t.Fatal("expected due after multiple missed fires")
	}
	want := time.Date(2026, 1, 4, 9, 0, 0, 0, time.UTC) // the LATEST missed fire, not Jan 1/2/3
	if !fireTime.Equal(want) {
		t.Errorf("fireTime = %v, want latest missed fire %v (not an earlier one)", fireTime, want)
	}
}

func TestCheckSchedule_TimezoneAffectsFireTime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable in this environment: %v", err)
	}
	c := &compiledCronSchedule{schedule: mustParse(t, "0 9 * * *"), location: ny}

	// 09:00 America/New_York on a fixed winter date (EST, UTC-5) is 14:00 UTC.
	last := time.Date(2026, 1, 1, 8, 0, 0, 0, ny)
	now := time.Date(2026, 1, 1, 9, 30, 0, 0, ny)
	due, fireTime := checkSchedule(c, last, now)
	if !due {
		t.Fatal("expected due at 09:30 local for a 09:00 local schedule")
	}
	if fireTime.UTC().Hour() != 14 {
		t.Errorf("expected 09:00 America/New_York to be 14:00 UTC in January (EST), got %v UTC", fireTime.UTC())
	}
}

// TestCheckSchedule_EveryMinuteFiresOncePerTick proves a frequent (every
// minute) schedule fires exactly once per due check, not a burst, when
// only a single minute has elapsed.
func TestCheckSchedule_EveryMinuteFiresOncePerTick(t *testing.T) {
	c := &compiledCronSchedule{schedule: mustParse(t, "* * * * *"), location: time.UTC}

	last := time.Date(2026, 1, 1, 10, 0, 30, 0, time.UTC)
	now := time.Date(2026, 1, 1, 10, 1, 15, 0, time.UTC)
	due, fireTime := checkSchedule(c, last, now)
	if !due {
		t.Fatal("expected due after a full minute elapsed")
	}
	want := time.Date(2026, 1, 1, 10, 1, 0, 0, time.UTC)
	if !fireTime.Equal(want) {
		t.Errorf("fireTime = %v, want %v", fireTime, want)
	}
}

func newCronTestStore(t *testing.T) *sqlitestore.SQLiteStore {
	t.Helper()
	store, err := sqlitestore.New("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestInitialAnchor_NeverFiredAnchorsToNow proves a brand new schedule
// (no prior claim) starts counting from "now", NOT some lookback window --
// the real bug found live: a freshly-registered "0 0 * * *" schedule
// immediately fired for that day's already-passed midnight before this
// distinction existed.
func TestInitialAnchor_NeverFiredAnchorsToNow(t *testing.T) {
	store := newCronTestStore(t)
	now := time.Date(2026, 1, 1, 17, 43, 0, 0, time.UTC) // well past a "0 0 * * *" midnight fire

	anchor := initialAnchor(context.Background(), store, "brand-new-schedule", now)
	if !anchor.Equal(now) {
		t.Fatalf("expected a never-fired schedule to anchor to now (%v), got %v", now, anchor)
	}

	// Prove the practical consequence: checking a midnight schedule right
	// after this anchor must NOT be due (no surprise immediate fire).
	c := &compiledCronSchedule{schedule: mustParse(t, "0 0 * * *"), location: time.UTC}
	due, _ := checkSchedule(c, anchor, now.Add(15*time.Second))
	if due {
		t.Fatal("expected a brand new schedule to NOT immediately fire for an occurrence that passed before it was registered")
	}
}

// TestInitialAnchor_PreviouslyFiredAnchorsToLastFire proves a restarting
// schedule (has a prior claim) anchors to that last fire time, so a
// missed fire during downtime is still caught up via checkSchedule.
func TestInitialAnchor_PreviouslyFiredAnchorsToLastFire(t *testing.T) {
	store := newCronTestStore(t)
	ctx := context.Background()

	lastFire := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	won, err := store.TryClaimCronFire(ctx, "restarting-schedule", lastFire)
	if err != nil || !won {
		t.Fatalf("seed claim: won=%v err=%v", won, err)
	}

	now := time.Date(2026, 1, 2, 9, 5, 0, 0, time.UTC) // process was down through the next day's fire
	anchor := initialAnchor(ctx, store, "restarting-schedule", now)
	if !anchor.Equal(lastFire) {
		t.Fatalf("expected anchor to be the last claimed fire time %v, got %v", lastFire, anchor)
	}

	// Prove the practical consequence: the missed daily fire IS caught up.
	c := &compiledCronSchedule{schedule: mustParse(t, "0 9 * * *"), location: time.UTC}
	due, fireTime := checkSchedule(c, anchor, now)
	if !due {
		t.Fatal("expected the missed fire during downtime to be caught up")
	}
	want := time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	if !fireTime.Equal(want) {
		t.Errorf("fireTime = %v, want %v", fireTime, want)
	}
}
