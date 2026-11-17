package main

import (
	"context"
	"testing"
	"time"

	cronlib "github.com/robfig/cron/v3"

	"github.com/getrunkite/runkite/internal/cronschedule"
	sqlitestore "github.com/getrunkite/runkite/internal/state/sqlite"
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

type cronNextFunc func(time.Time) time.Time

func (f cronNextFunc) Next(t time.Time) time.Time { return f(t) }

func TestCheckScheduleStopsOnZeroOrNonProgressingNext(t *testing.T) {
	last := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	now := last.Add(time.Hour)
	for _, tc := range []struct {
		name string
		next cronNextFunc
	}{
		{"zero", func(time.Time) time.Time { return time.Time{} }},
		{"same", func(t time.Time) time.Time { return t }},
		{"earlier", func(t time.Time) time.Time { return t.Add(-time.Minute) }},
	} {
		t.Run("initial_"+tc.name, func(t *testing.T) {
			calls := 0
			c := &compiledCronSchedule{schedule: cronNextFunc(func(t time.Time) time.Time {
				calls++
				if calls > 1 {
					panic("scheduler retried an unusable initial occurrence")
				}
				return tc.next(t)
			})}
			if due, fire := checkSchedule(c, last, now); due || !fire.IsZero() {
				t.Fatalf("invalid initial occurrence became due: %v %v", due, fire)
			}
		})
		t.Run("after_valid_fire_"+tc.name, func(t *testing.T) {
			calls := 0
			want := last.Add(time.Minute)
			c := &compiledCronSchedule{schedule: cronNextFunc(func(t time.Time) time.Time {
				calls++
				if calls > 2 {
					panic("scheduler kept iterating after an unusable next occurrence")
				}
				if calls == 1 {
					return want
				}
				return tc.next(t)
			})}
			if due, fire := checkSchedule(c, last, now); !due || !fire.Equal(want) {
				t.Fatalf("last valid fire was discarded: %v %v", due, fire)
			}
		})
	}
	calendar, err := cronschedule.Compile("0 0 31 2 *", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	boundedCalendar := cronNextFunc(func(t time.Time) time.Time {
		calls++
		if calls > 1 {
			panic("scheduler kept iterating an impossible real calendar")
		}
		return calendar.Schedule.Next(t)
	})
	if due, fire := checkSchedule(&compiledCronSchedule{schedule: boundedCalendar}, last, now); due || !fire.IsZero() {
		t.Fatalf("impossible calendar became due: %v %v", due, fire)
	}
}

func TestRelativeIntervalRetainsAnchorAcrossNotDuePolls(t *testing.T) {
	parsed, err := cronschedule.Compile("@every 1m", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	c := &compiledCronSchedule{schedule: parsed.Schedule, location: parsed.Location}
	start := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	anchor := start
	fires := 0
	for tick := 1; tick <= 8; tick++ {
		now := start.Add(time.Duration(tick) * cronTickInterval)
		due, fire := checkSchedule(c, anchor, now)
		if !due {
			anchor = anchorAfterNotDue(c, anchor, now)
			continue
		}
		fires++
		want := start.Add(time.Duration(fires) * time.Minute)
		if !fire.Equal(want) || !now.Equal(want) {
			t.Fatalf("interval lost its initial anchor: fire=%v now=%v want=%v", fire, now, want)
		}
		anchor = anchorAfterHandledFire(c, fire, now)
	}
	if fires != 2 {
		t.Fatalf("one-minute interval starved across fifteen-second polls: fires=%d", fires)
	}
	// Existing latest-only collapse still applies after an interval's missed polls.
	if due, fire := checkSchedule(c, start, start.Add(3*time.Minute+15*time.Second)); !due || !fire.Equal(start.Add(3*time.Minute)) {
		t.Fatalf("relative catch-up changed: due=%v fire=%v", due, fire)
	}
}

func TestCalendarNotDueAnchorStillAdvancesToNow(t *testing.T) {
	c := &compiledCronSchedule{schedule: mustParse(t, "0 9 * * *"), location: time.UTC}
	last := time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	for _, now := range []time.Time{last.Add(15 * time.Second), last.Add(-15 * time.Second)} {
		if due, _ := checkSchedule(c, last, now); due {
			t.Fatal("calendar fixture unexpectedly due")
		}
		if anchor := anchorAfterNotDue(c, last, now); !anchor.Equal(now) {
			t.Fatalf("calendar not-due bookkeeping changed: %v want %v", anchor, now)
		}
	}
}

func TestRelativeIntervalReplicasKeepSharedClaimPhaseAfterLatePolls(t *testing.T) {
	store := newCronTestStore(t)
	ctx := t.Context()
	start := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	const name = "shared-interval"
	if won, err := store.TryClaimCronFire(ctx, name, start); err != nil || !won {
		t.Fatalf("seed common durable anchor: won=%v err=%v", won, err)
	}
	parsed, err := cronschedule.Compile("@every 1m", "UTC")
	if err != nil {
		t.Fatal(err)
	}
	c := &compiledCronSchedule{schedule: parsed.Schedule, location: parsed.Location}
	anchors := [2]time.Time{
		initialAnchor(ctx, store, name, start.Add(10*time.Second)),
		initialAnchor(ctx, store, name, start.Add(20*time.Second)),
	}
	for _, round := range []struct {
		minute int
		leader int
	}{
		{1, 0}, // Replica A wins at :01, B loses at :08.
		{2, 1}, // Reverse the winner with the same late-poll offsets.
		{5, 0}, // Several missed fires still collapse to the same latest fire.
	} {
		want := start.Add(time.Duration(round.minute) * time.Minute)
		if round.minute <= 2 {
			for replica := range anchors {
				now := want.Add(-7 * time.Second)
				if due, _ := checkSchedule(c, anchors[replica], now); due {
					t.Fatalf("replica %d fired before the interval boundary", replica)
				}
				anchors[replica] = anchorAfterNotDue(c, anchors[replica], now)
			}
		}
		for position, replica := range []int{round.leader, 1 - round.leader} {
			now := want.Add(time.Duration(1+7*position) * time.Second)
			due, fire := checkSchedule(c, anchors[replica], now)
			if !due || !fire.Equal(want) {
				t.Fatalf("replica %d phase drifted: due=%v fire=%v want=%v", replica, due, fire, want)
			}
			won, err := store.TryClaimCronFire(ctx, name, fire)
			if err != nil || won != (position == 0) {
				t.Fatalf("replica %d claim: won=%v err=%v", replica, won, err)
			}
			// All consumed outcomes use this same production helper: claim loss,
			// successful dispatch, and overlap rejection that retains its claim.
			anchors[replica] = anchorAfterHandledFire(c, fire, now)
		}
		if !anchors[0].Equal(want) || !anchors[1].Equal(want) {
			t.Fatalf("consumed claim lost the common phase: %v", anchors)
		}
		latest, found, err := store.GetLastCronFireTime(ctx, name)
		if err != nil || !found || !latest.Equal(want) {
			t.Fatalf("durable claim phase changed: %v %v %v", latest, found, err)
		}
		if next := c.schedule.Next(anchors[0]); !next.Equal(want.Add(time.Minute)) || !next.Equal(c.schedule.Next(anchors[1])) {
			t.Fatalf("replicas no longer agree on the following boundary: %v", next)
		}
	}
}

func TestCalendarHandledFireAnchorStillAdvancesToNow(t *testing.T) {
	c := &compiledCronSchedule{schedule: mustParse(t, "0 9 * * *"), location: time.UTC}
	fire := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	now := fire.Add(8 * time.Second)
	if got := anchorAfterHandledFire(c, fire, now); !got.Equal(now) {
		t.Fatalf("calendar consumed-fire bookkeeping changed: %v want %v", got, now)
	}
}
