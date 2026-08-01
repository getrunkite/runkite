package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/getrunkite/runkite/internal/api"
	"github.com/getrunkite/runkite/internal/config"
)

// runTimeoutConfig is the resolved form of config.RunTimeoutEntry --
// durations already parsed so the loop never re-parses config.
type runTimeoutConfig struct {
	maxDuration time.Duration
	interval    time.Duration
}

// initRunTimeoutConfig reads the "run_timeout" section from the first
// discovered langgraph.json (same control-plane-wide/first-file
// convention as initRetentionConfig). Returns nil when unconfigured or
// when max_duration is absent/invalid -- the caller then skips starting
// the background loop entirely.
func initRunTimeoutConfig(configPath string) *runTimeoutConfig {
	paths := config.FindLangGraphJSON(configPath)
	if len(paths) == 0 {
		return nil
	}
	cfg, err := config.LoadLangGraphJSON(paths[0])
	if err != nil || cfg.RunTimeout == nil || cfg.RunTimeout.MaxDuration == "" {
		return nil
	}

	d, parseErr := time.ParseDuration(cfg.RunTimeout.MaxDuration)
	if parseErr != nil {
		slog.Error("run_timeout: invalid max_duration, sweep disabled", "value", cfg.RunTimeout.MaxDuration, "error", parseErr)
		return nil
	}
	if d <= 0 {
		slog.Error("run_timeout: max_duration must be > 0, sweep disabled", "value", cfg.RunTimeout.MaxDuration)
		return nil
	}

	interval := time.Duration(cfg.RunTimeout.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 15 * time.Second
	}
	return &runTimeoutConfig{maxDuration: d, interval: interval}
}

// runTimeoutLoop periodically forces overdue pending/running runs to
// status "timeout". Runs once immediately on startup (same reasoning as
// retention: a freshly-enabled policy shouldn't wait a full interval
// before acting on an existing backlog), then every rc.interval.
func runTimeoutLoop(ctx context.Context, apiServer *api.Server, rc *runTimeoutConfig) {
	runTimeoutTick(ctx, apiServer, rc)

	ticker := time.NewTicker(rc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runTimeoutTick(ctx, apiServer, rc)
	}
}

// runTimeoutTick is the single-tick body, split out so it's directly
// testable without dealing with tickers/goroutines.
func runTimeoutTick(ctx context.Context, apiServer *api.Server, rc *runTimeoutConfig) {
	n, err := apiServer.TimeoutOverdueRuns(ctx, rc.maxDuration, 100)
	if err != nil {
		slog.Error("run_timeout: TimeoutOverdueRuns failed", "error", err)
		return
	}
	if n > 0 {
		slog.Info("run_timeout: timed out overdue runs", "count", n, "max_duration", rc.maxDuration)
	}
}
