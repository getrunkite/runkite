package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	cronlib "github.com/robfig/cron/v3"

	"github.com/sharanharsoor/runkite/internal/api"
	"github.com/sharanharsoor/runkite/internal/config"
	"github.com/sharanharsoor/runkite/internal/models"
	"github.com/sharanharsoor/runkite/internal/state"
	"github.com/sharanharsoor/runkite/internal/tenant"
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
	if next.After(now) {
		return false, time.Time{}
	}
	fireTime = next
	for {
		n := c.schedule.Next(fireTime)
		if n.After(now) {
			break
		}
		fireTime = n
	}
	return true, fireTime
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
// (state.Store.TryClaimCronFire) before dispatching -- with multiple
// control-plane replicas against the same database, exactly one dispatches
// each fire, not N ("Postgres claim window" in the master plan). Runs
// until ctx is cancelled.
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
				loc, locErr := time.LoadLocation(sched.Timezone)
				if locErr != nil {
					if sched.Timezone != "" {
						slog.Warn("cron: unknown timezone, falling back to UTC", "tenant", sched.TenantID, "name", sched.Name, "timezone", sched.Timezone, "error", locErr)
					}
					loc = time.UTC
				}
				parsed, parseErr := cronlib.ParseStandard(sched.Expression)
				if parseErr != nil {
					slog.Error("cron: invalid expression, skipping schedule", "tenant", sched.TenantID, "name", sched.Name, "expression", sched.Expression, "error", parseErr)
					continue
				}
				c = &compiledCronSchedule{expression: sched.Expression, timezone: sched.Timezone, schedule: parsed, location: loc}
				compiled[key] = c
				lastChecked[key] = initialAnchor(schedCtx, store, sched.Name, time.Now().In(loc))
				continue // compile this tick; first due-check is the next tick
			}

			now := time.Now().In(c.location)
			due, fireTime := checkSchedule(c, lastChecked[key], now)
			if !due {
				lastChecked[key] = now
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
				lastChecked[key] = now
				continue
			}

			run, dispatchErr := apiServer.DispatchScheduledRun(schedCtx, sched.Name, sched.AgentID, sched.Input, sched.Config)
			if dispatchErr != nil {
				var conflict *state.ErrConflict
				if errors.As(dispatchErr, &conflict) {
					// Overlapping fire: thread still busy from a prior run.
					// Keep the claim so this fire_time is not retried; the
					// next cron boundary gets a fresh claim key.
					lastChecked[key] = now
					slog.Warn("cron: overlapping fire rejected", "tenant", sched.TenantID, "name", sched.Name, "fire_time", fireTime, "error", dispatchErr)
					continue
				}
				// Transient (rate limit, store blip, missing agent mid-boot):
				// release the claim and leave lastChecked so we retry.
				if relErr := store.ReleaseCronClaim(schedCtx, sched.Name, fireTime); relErr != nil {
					slog.Error("cron: failed to release claim after dispatch error", "tenant", sched.TenantID, "name", sched.Name, "error", relErr)
				}
				slog.Error("cron: failed to dispatch scheduled run", "tenant", sched.TenantID, "name", sched.Name, "error", dispatchErr)
				continue
			}
			lastChecked[key] = now
			slog.Info("cron: dispatched scheduled run", "tenant", sched.TenantID, "name", sched.Name, "run_id", run.RunID, "fire_time", fireTime)
		}
	}
}
