package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	cronlib "github.com/robfig/cron/v3"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/cronschedule"
	"github.com/getrunkite/runkite/internal/models"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// bootstrapCronSchedules reads the "cron" section from every discovered
// langgraph.json (same multi-file merge as bootstrapAgents -- schedules are
// per-agent-config, just like the agents themselves, unlike the single-
// first-file convention auth/rate_limit/webhooks/custom_routes use).
func bootstrapCronSchedules(store state.Store, configPath string) {
	paths := config.FindLangGraphJSON(configPath)
	ctx := context.Background()
	for _, path := range paths {
		cfg, err := config.LoadLangGraphJSON(path)
		if err != nil {
			continue // already logged by bootstrapAgents
		}
		for name, entry := range cfg.Cron {
			enabled := true
			if entry.Enabled != nil {
				enabled = *entry.Enabled
			}
			input, _ := json.Marshal(entry.Input)
			cfgJSON, _ := json.Marshal(entry.Config)
			sched := &models.CronSchedule{
				Name: name, AgentID: entry.AgentID, Expression: entry.Expression,
				Timezone: entry.Timezone, Input: input, Config: cfgJSON, Enabled: enabled,
			}
			if err := store.UpsertCronSchedule(ctx, sched); err != nil {
				slog.Error("failed to register cron schedule", "name", name, "error", err)
				continue
			}
			slog.Info("registered cron schedule", "name", name, "agent_id", entry.AgentID,
				"expression", entry.Expression, "enabled", enabled)
		}
	}
}

// compiledCronSchedule caches a parsed cron.Schedule + resolved
// time.Location so the scheduler loop doesn't re-parse the expression and
// re-resolve the timezone on every tick.
type compiledCronSchedule struct {
	expression string
	timezone   string
	schedule   cronlib.Schedule
	location   *time.Location
}

// scheduleKey identifies a schedule uniquely across every tenant --
// schedule names are only unique WITHIN a tenant (two tenants can both
// have a "daily-report"), so the scheduler's bookkeeping maps must key on
// (tenant, name), never name alone.
type scheduleKey struct {
	tenantID string
	name     string
}

// checkSchedule reports whether c is due to fire, and the fire time to
// claim/dispatch, given the last time it was checked and the current time.
// Extracted from runCronScheduler's tick loop specifically so this core
// "is it due, and which fire time" decision is unit-testable without a
// real ticker, goroutine, or state.Store.
//
// If more than one fire was missed since last (e.g. the process was down,
// or a tick was skipped), only the single latest due time is returned --
// not a backlog of every missed one. A catch-up storm is not what a 15s
// poller's users expect from "cron", unlike a durable job scheduler
// explicitly designed to backfill missed runs.
func checkSchedule(c *compiledCronSchedule, last, now time.Time) (due bool, fireTime time.Time) {
	next := c.schedule.Next(last)
	if next.IsZero() || !next.After(last) || next.After(now) {
		return false, time.Time{}
	}
	fireTime = next
	for {
		n := c.schedule.Next(fireTime)
		if n.IsZero() || !n.After(fireTime) || n.After(now) {
			break
		}
		fireTime = n
	}
	return true, fireTime
}

// A relative interval must retain its starting point until due. Advancing it
// on every poll would perpetually postpone intervals longer than one tick.
// Calendar schedules keep their existing not-due bookkeeping.
func anchorAfterNotDue(c *compiledCronSchedule, last, now time.Time) time.Time {
	if _, relative := c.schedule.(cronlib.ConstantDelaySchedule); relative {
		return last
	}
	return now
}

// Consuming an interval fire must retain its phase, regardless of the local
// poll time or whether this replica won its claim. Fresh schedules can still
// start with different replica-local anchors; this does not create a global epoch.
func anchorAfterHandledFire(c *compiledCronSchedule, fireTime, now time.Time) time.Time {
	if _, relative := c.schedule.(cronlib.ConstantDelaySchedule); relative {
		return fireTime
	}
	return now
}

// initialAnchor picks the starting point for a freshly-compiled schedule's
// lastChecked bookkeeping, extracted from runCronScheduler's compile branch
// specifically so this decision is unit-testable against a real store
// without a ticker or goroutine:
//   - The schedule has fired before (state.Store.GetLastCronFireTime) ->
//     anchor to that last fire time, so a restart (or a new replica
//     joining) catches up to the single latest fire missed since then via
//     checkSchedule's collapse-to-latest logic.
//   - The schedule has never fired -> anchor to now. Nothing to catch up
//     on, and anchoring to anything earlier would make a brand new
//     schedule surprise-fire once immediately if its expression's most
//     recent occurrence already passed before it was ever registered
//     (confirmed live: a schedule for "0 0 * * *" added at 17:43 fired
//     immediately for that day's already-passed midnight before this
//     function existed).
func initialAnchor(ctx context.Context, store state.Store, scheduleName string, now time.Time) time.Time {
	lastFire, found, err := store.GetLastCronFireTime(ctx, scheduleName)
	if err != nil {
		slog.Warn("cron: failed to look up last fire time, anchoring to now", "name", scheduleName, "error", err)
		return now
	}
	if !found {
		return now
	}
	return lastFire.In(now.Location())
}

// cronTickInterval is how often the scheduler loop checks whether any
// schedule is due. Well under cron's finest standard resolution (1 minute)
// so a fire is never missed by more than this margin.
const cronTickInterval = 15 * time.Second

// runCronScheduler polls every cronTickInterval, and for each enabled
// schedule whose next fire time has arrived, atomically claims it
// (state.Store.TryClaimCronFire) before dispatching. Replicas sharing the store
// retain the fire after accepted or uncertain delivery. Only a proven safe
// rejection releases it for retry. This is not exactly-once execution. Runs
// until ctx is canceled.
func runCronScheduler(ctx context.Context, store state.Store, apiServer *api.Server) {
	ticker := time.NewTicker(cronTickInterval)
	defer ticker.Stop()

	compiled := map[scheduleKey]*compiledCronSchedule{}
	// lastChecked anchors each schedule's Next() computation; see
	// initialAnchor for how a freshly-compiled schedule's starting point
	// is chosen.
	lastChecked := map[scheduleKey]time.Time{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// System context: the scheduler must service every tenant's
		// schedules, not just one -- there's no "caller" here to scope
		// to. Each schedule's OWN tenant (sched.TenantID) is used below
		// for everything else in this tick, never this system context.
		schedules, err := store.ListCronSchedules(tenant.SystemContext(ctx))
		if err != nil {
			slog.Error("cron: failed to list schedules", "error", err)
			continue
		}

		for _, sched := range schedules {
			if !sched.Enabled {
				continue
			}
			key := scheduleKey{tenantID: sched.TenantID, name: sched.Name}
			// Every store call below acts on this ONE schedule's own
			// tenant -- claims, last-fire lookups, and the dispatched
			// run all need to land in sched.TenantID, not "default" and
			// not every tenant at once (system context is only for the
			// ListCronSchedules call above).
			schedCtx := tenant.WithContext(ctx, sched.TenantID)

			c, ok := compiled[key]
			if !ok || c.expression != sched.Expression || c.timezone != sched.Timezone {
				parsed, parseErr := cronschedule.Compile(sched.Expression, sched.Timezone)
				if parsed.ConfiguredTimezoneError != nil {
					if sched.Timezone != "" {
						slog.Warn("cron: unknown timezone, falling back to UTC", "tenant", sched.TenantID, "name", sched.Name, "timezone", sched.Timezone, "error", parsed.ConfiguredTimezoneError)
					}
				}
				if parseErr != nil {
					slog.Error("cron: invalid expression, skipping schedule", "tenant", sched.TenantID, "name", sched.Name, "expression", sched.Expression, "error", parseErr)
					continue
				}
				c = &compiledCronSchedule{expression: sched.Expression, timezone: sched.Timezone, schedule: parsed.Schedule, location: parsed.Location}
				compiled[key] = c
				lastChecked[key] = initialAnchor(schedCtx, store, sched.Name, time.Now().In(c.location))
				continue // compile this tick; first due-check is the next tick
			}

			now := time.Now().In(c.location)
			due, fireTime := checkSchedule(c, lastChecked[key], now)
			if !due {
				lastChecked[key] = anchorAfterNotDue(c, lastChecked[key], now)
				continue
			}

			won, claimErr := store.TryClaimCronFire(schedCtx, sched.Name, fireTime)
			if claimErr != nil {
				slog.Error("cron: claim failed", "tenant", sched.TenantID, "name", sched.Name, "fire_time", fireTime, "error", claimErr)
				continue // leave lastChecked so the next tick retries
			}
			if !won {
				// Another control-plane instance's scheduler loop already
				// claimed and is dispatching this exact fire.
				lastChecked[key] = anchorAfterHandledFire(c, fireTime, now)
				continue
			}

			if dispatchClaimedCronFire(schedCtx, store, apiServer, sched, fireTime) {
				lastChecked[key] = anchorAfterHandledFire(c, fireTime, now)
			}
		}
	}
}

type cronRunDispatcher interface {
	DispatchScheduledRun(context.Context, string, string, json.RawMessage, json.RawMessage) (*models.Run, error)
}

// dispatchClaimedCronFire reports whether the caller should consume this fire's
// anchor. False preserves the anchor for a subsequent claim/retry attempt.
func dispatchClaimedCronFire(ctx context.Context, store state.Store, dispatcher cronRunDispatcher, sched *models.CronSchedule, fireTime time.Time) bool {
	run, dispatchErr := dispatcher.DispatchScheduledRun(ctx, sched.Name, sched.AgentID, sched.Input, sched.Config)
	if dispatchErr == nil {
		slog.Info("cron: dispatched scheduled run", "tenant", sched.TenantID, "name", sched.Name, "run_id", run.RunID, "fire_time", fireTime)
		return true
	}
	runID := ""
	if run != nil {
		runID = run.RunID
	}
	// Inspect the scheduled disposition before its underlying error chain. A
	// queue acknowledgement error may itself wrap ErrConflict without proving
	// that this was a busy-thread rejection before publication.
	var delivery *api.ScheduledRunDispatchError
	if errors.As(dispatchErr, &delivery) {
		if !delivery.RetrySafe() {
			slog.Warn("cron: retained fire after uncertain dispatch", "tenant", sched.TenantID, "name", sched.Name, "fire_time", fireTime, "run_id", runID, "error", dispatchErr)
			return true
		}
	} else {
		var conflict *state.ErrConflict
		if errors.As(dispatchErr, &conflict) {
			slog.Warn("cron: overlapping fire rejected", "tenant", sched.TenantID, "name", sched.Name, "fire_time", fireTime, "error", dispatchErr)
			return true
		}
	}
	if relErr := store.ReleaseCronClaim(ctx, sched.Name, fireTime); relErr != nil {
		slog.Error("cron: failed to release claim after dispatch error", "tenant", sched.TenantID, "name", sched.Name, "error", relErr)
	}
	slog.Error("cron: failed to dispatch scheduled run", "tenant", sched.TenantID, "name", sched.Name, "run_id", runID, "error", dispatchErr)
	return false
}
