package main

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// setupLogging configures the global slog default handler from two env
// vars, replacing the hardcoded LevelInfo/NewTextHandler setup every
// entrypoint (serve, dev, db upgrade/reset) used to duplicate:
//
//   - LOG_LEVEL: debug|info|warn|error (case-insensitive), default info.
//   - LOG_FORMAT: text|json, default text -- unset behaves exactly like
//     before this existed, so nothing regresses for anyone not setting it.
//     JSON is the shape a log aggregator (Datadog, Grafana Loki, etc.)
//     expects; every existing slog.Info/Error call already passes
//     structured attributes (run_id, thread_id, error, ...), so switching
//     the handler is the whole fix -- no call sites need touching.
func setupLogging() {
	configureLogging(os.Stdout)
}

// configureLogging is setupLogging's actual logic, split out so a test
// can inject a buffer instead of os.Stdout and assert on real output --
// there's no other way to observe what slog.SetDefault actually wired up.
func configureLogging(w io.Writer) {
	opts := &slog.HandlerOptions{Level: parseLogLevel(envOrDefault("LOG_LEVEL", "info"))}

	var handler slog.Handler
	if strings.EqualFold(envOrDefault("LOG_FORMAT", "text"), "json") {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	slog.SetDefault(slog.New(handler))
}

// parseLogLevel defaults to Info for empty or unrecognized input rather
// than erroring -- a typo'd LOG_LEVEL shouldn't crash startup, just fall
// back to today's existing default.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
