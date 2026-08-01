package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/config"
	"github.com/getrunkite/runkite/internal/state"
	"github.com/getrunkite/runkite/internal/tenant"
)

// retentionConfig is the resolved, ready-to-use form of config.RetentionEntry
// -- durations already parsed, all prune dimensions resolved to
// "on or off" so runRetentionLoop never re-parses config.
type retentionConfig struct {
	runsMaxAge               time.Duration // zero means run pruning is off
	checkpointsKeepLast      int           // <= 0 means checkpoint pruning is off
	cronClaimsMaxAge         time.Duration // zero means cron-claim pruning is off
	terminalHookClaimsMaxAge time.Duration // zero means terminal-hook-claim pruning is off
	webhookDeadLettersMaxAge time.Duration // zero means webhook DLQ pruning is off
	interval                 time.Duration
}

// initRetentionConfig reads the "retention" section from the first
// discovered langgraph.json (same control-plane-wide/first-file convention
// as initAuthProvider/initRateLimiter/initHooks/initCorsConfig). Returns
// nil when unconfigured, or configured but all prune dimensions (runs,
// checkpoints, cron claims, terminal-hook claims, webhook dead letters)
// are off -- the caller then skips starting the background loop entirely,
// matching this project's disabled-by-default-until-configured convention
// for every platform extension.
func initRetentionConfig(configPath string) *retentionConfig {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.Retention == nil {
		return nil
	}

	rc := &retentionConfig{checkpointsKeepLast: cfg.Retention.CheckpointsKeepLast}
	if cfg.Retention.RunsMaxAge != "" {
		d, parseErr := time.ParseDuration(cfg.Retention.RunsMaxAge)
		if parseErr != nil {
			slog.Error("retention: invalid runs_max_age, run pruning disabled", "value", cfg.Retention.RunsMaxAge, "error", parseErr)
		} else {
			rc.runsMaxAge = d
		}
	}
	if cfg.Retention.CronClaimsMaxAge != "" {
		d, parseErr := time.ParseDuration(cfg.Retention.CronClaimsMaxAge)
		if parseErr != nil {
			slog.Error("retention: invalid cron_claims_max_age, cron-claim pruning disabled", "value", cfg.Retention.CronClaimsMaxAge, "error", parseErr)
		} else {
			rc.cronClaimsMaxAge = d
		}
	}
	if cfg.Retention.TerminalHookClaimsMaxAge != "" {
		d, parseErr := time.ParseDuration(cfg.Retention.TerminalHookClaimsMaxAge)
		if parseErr != nil {
			slog.Error("retention: invalid terminal_hook_claims_max_age, terminal-hook-claim pruning disabled", "value", cfg.Retention.TerminalHookClaimsMaxAge, "error", parseErr)
		} else {
			rc.terminalHookClaimsMaxAge = d
		}
	}
	if cfg.Retention.WebhookDeadLettersMaxAge != "" {
		d, parseErr := time.ParseDuration(cfg.Retention.WebhookDeadLettersMaxAge)
		if parseErr != nil {
			slog.Error("retention: invalid webhook_dead_letters_max_age, DLQ pruning disabled", "value", cfg.Retention.WebhookDeadLettersMaxAge, "error", parseErr)
		} else {
			rc.webhookDeadLettersMaxAge = d
		}
	}
	if rc.runsMaxAge <= 0 && rc.checkpointsKeepLast <= 0 && rc.cronClaimsMaxAge <= 0 && rc.terminalHookClaimsMaxAge <= 0 && rc.webhookDeadLettersMaxAge <= 0 {
		return nil
	}

	rc.interval = time.Duration(cfg.Retention.IntervalMinutes) * time.Minute
	if rc.interval <= 0 {
		rc.interval = 60 * time.Minute
	}
	return rc
}

// runRetentionLoop periodically prunes old terminal runs and/or excess
// checkpoint history, per the resolved retentionConfig. Always operates
// across every tenant (tenant.SystemContext) -- retention is a deployment-
// wide policy set once in config, not something scoped to whoever
// happens to trigger a tick, mirroring runCronScheduler's same system-
// context rationale in cmd/cron.go.
func runRetentionLoop(ctx context.Context, store state.Store, rc *retentionConfig) {
	// Run once immediately rather than waiting for the first ticker
	// fire: time.NewTicker's first tick doesn't land until rc.interval
	// has elapsed (up to the 60-minute default), which would otherwise
	// leave a freshly-enabled retention policy doing nothing for up to
	// an hour even if there's a large backlog of old data to prune the
	// moment the control plane starts.
	runRetentionTick(ctx, store, rc)

	ticker := time.NewTicker(rc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runRetentionTick(ctx, store, rc)
	}
}

// runRetentionTick is the single-tick body, split out from runRetentionLoop
// so it's directly testable without dealing with tickers/goroutines.
func runRetentionTick(ctx context.Context, store state.Store, rc *retentionConfig) {
	sysCtx := tenant.SystemContext(ctx)
	if rc.runsMaxAge > 0 {
		cutoff := time.Now().UTC().Add(-rc.runsMaxAge)
		n, err := store.PruneRuns(sysCtx, cutoff)
		if err != nil {
			slog.Error("retention: PruneRuns failed", "error", err)
		} else if n > 0 {
			slog.Info("retention: pruned old runs", "count", n, "older_than", cutoff)
		}
	}
	if rc.checkpointsKeepLast > 0 {
		n, err := store.PruneCheckpoints(sysCtx, rc.checkpointsKeepLast)
		if err != nil {
			slog.Error("retention: PruneCheckpoints failed", "error", err)
		} else if n > 0 {
			slog.Info("retention: pruned excess checkpoints", "count", n, "keep_last", rc.checkpointsKeepLast)
		}
	}
	if rc.cronClaimsMaxAge > 0 {
		cutoff := time.Now().UTC().Add(-rc.cronClaimsMaxAge)
		n, err := store.PruneCronClaims(sysCtx, cutoff)
		if err != nil {
			slog.Error("retention: PruneCronClaims failed", "error", err)
		} else if n > 0 {
			slog.Info("retention: pruned old cron claims", "count", n, "older_than", cutoff)
		}
	}
	if rc.terminalHookClaimsMaxAge > 0 {
		cutoff := time.Now().UTC().Add(-rc.terminalHookClaimsMaxAge)
		n, err := store.PruneTerminalHookClaims(sysCtx, cutoff)
		if err != nil {
			slog.Error("retention: PruneTerminalHookClaims failed", "error", err)
		} else if n > 0 {
			slog.Info("retention: pruned old terminal hook claims", "count", n, "older_than", cutoff)
		}
	}
	if rc.webhookDeadLettersMaxAge > 0 {
		cutoff := time.Now().UTC().Add(-rc.webhookDeadLettersMaxAge)
		n, err := store.PruneWebhookDeadLetters(sysCtx, cutoff)
		if err != nil {
			slog.Error("retention: PruneWebhookDeadLetters failed", "error", err)
		} else if n > 0 {
			slog.Info("retention: pruned old webhook dead letters", "count", n, "older_than", cutoff)
		}
	}
}
