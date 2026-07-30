package main

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"bogus":   slog.LevelInfo, // unrecognized input falls back to Info, doesn't crash startup
	}
	for input, want := range cases {
		if got := parseLogLevel(input); got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

// TestConfigureLogging_JSONFormat proves LOG_FORMAT=json actually
// switches the default logger's handler (not just accepted as a no-op
// env var) and LOG_LEVEL actually filters what gets emitted -- by
// injecting a buffer and logging through the real slog.Default() that
// configureLogging wired up, not a second hand-built logger.
func TestConfigureLogging_JSONFormat(t *testing.T) {
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("LOG_LEVEL", "warn")
	var buf bytes.Buffer
	configureLogging(&buf)
	t.Cleanup(func() {
		// Restore a plain default so later tests in this package (which
		// assume nothing about the global slog handler) aren't affected.
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	})

	slog.Info("should be filtered out")
	slog.Warn("should appear", "key", "value")

	out := buf.String()
	if strings.Contains(out, "should be filtered out") {
		t.Error("Info-level log line appeared despite LOG_LEVEL=warn")
	}
	if !strings.Contains(out, `"msg":"should appear"`) {
		t.Errorf("expected JSON-formatted warn line, got: %s", out)
	}
	if !strings.Contains(out, `"key":"value"`) {
		t.Errorf("expected structured attribute in JSON output, got: %s", out)
	}
}

// TestConfigureLogging_TextFormatIsDefault proves an unset LOG_FORMAT
// keeps the original plain-text shape -- nothing regresses for anyone
// not setting the new env vars.
func TestConfigureLogging_TextFormatIsDefault(t *testing.T) {
	os.Unsetenv("LOG_FORMAT")
	os.Unsetenv("LOG_LEVEL")
	var buf bytes.Buffer
	configureLogging(&buf)
	t.Cleanup(func() {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	})

	slog.Info("plain text line", "key", "value")

	out := buf.String()
	if strings.Contains(out, "{") {
		t.Errorf("expected plain text output (no JSON braces), got: %s", out)
	}
	if !strings.Contains(out, "msg=\"plain text line\"") {
		t.Errorf("expected slog's default text shape, got: %s", out)
	}
}
