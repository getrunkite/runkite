/**
 * Self-check for logger.ts: LOG_LEVEL and LOG_FORMAT env vars,
 * mirroring the Go control plane's logging_test.go and the Python
 * runner's test_logging_config.py.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { logger } from "./logger.js";

/** Captures console.log/console.error output for the duration of fn,
 * then restores the originals -- logger.ts routes info/debug through
 * console.log and warn/error through console.error. */
async function captureConsole(fn: () => void): Promise<string[]> {
  const lines: string[] = [];
  const origLog = console.log;
  const origError = console.error;
  console.log = (...args: unknown[]) => lines.push(args.map(String).join(" "));
  console.error = (...args: unknown[]) => lines.push(args.map(String).join(" "));
  try {
    fn();
  } finally {
    console.log = origLog;
    console.error = origError;
  }
  return lines;
}

function withEnv(vars: Record<string, string | undefined>, fn: () => void): void {
  const prev: Record<string, string | undefined> = {};
  for (const key of Object.keys(vars)) prev[key] = process.env[key];
  try {
    for (const [key, value] of Object.entries(vars)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
    fn();
  } finally {
    for (const [key, value] of Object.entries(prev)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  }
}

test("default level (info) shows info/warn/error, filters debug", async () => {
  const lines = await captureConsole(() => {
    withEnv({ LOG_LEVEL: undefined, LOG_FORMAT: undefined }, () => {
      logger.debug("debug msg");
      logger.info("info msg");
      logger.warn("warn msg");
      logger.error("error msg");
    });
  });
  assert.equal(lines.filter((l) => l.includes("debug msg")).length, 0, "debug should be filtered at default level");
  assert.equal(lines.filter((l) => l.includes("info msg")).length, 1);
  assert.equal(lines.filter((l) => l.includes("warn msg")).length, 1);
  assert.equal(lines.filter((l) => l.includes("error msg")).length, 1);
});

test("LOG_LEVEL=warn filters info and debug, keeps warn/error", async () => {
  const lines = await captureConsole(() => {
    withEnv({ LOG_LEVEL: "warn", LOG_FORMAT: undefined }, () => {
      logger.debug("debug msg");
      logger.info("info msg");
      logger.warn("warn msg");
      logger.error("error msg");
    });
  });
  assert.equal(lines.filter((l) => l.includes("debug msg") || l.includes("info msg")).length, 0);
  assert.equal(lines.filter((l) => l.includes("warn msg")).length, 1);
  assert.equal(lines.filter((l) => l.includes("error msg")).length, 1);
});

test("LOG_FORMAT=json produces parseable JSON with level and message fields", async () => {
  const lines = await captureConsole(() => {
    withEnv({ LOG_LEVEL: "debug", LOG_FORMAT: "json" }, () => {
      logger.info("hello json");
    });
  });
  assert.equal(lines.length, 1);
  const parsed = JSON.parse(lines[0]);
  assert.equal(parsed.level, "info");
  assert.equal(parsed.message, "hello json");
  assert.ok(typeof parsed.time === "string" && parsed.time.length > 0);
});

test("LOG_FORMAT=json serializes an Error arg into name/message/stack instead of losing it", async () => {
  const lines = await captureConsole(() => {
    withEnv({ LOG_LEVEL: "debug", LOG_FORMAT: "json" }, () => {
      logger.error("run failed", new Error("boom"));
    });
  });
  const parsed = JSON.parse(lines[0]);
  assert.equal(parsed.args[0].name, "Error");
  assert.equal(parsed.args[0].message, "boom");
  assert.ok(typeof parsed.args[0].stack === "string");
});

test("unset LOG_FORMAT keeps the original plain-text shape", async () => {
  const lines = await captureConsole(() => {
    withEnv({ LOG_LEVEL: undefined, LOG_FORMAT: undefined }, () => {
      logger.info("plain text line");
    });
  });
  assert.equal(lines.length, 1);
  assert.ok(!lines[0].startsWith("{"), "should not be JSON-formatted by default");
  assert.ok(lines[0].includes("[INFO]"));
  assert.ok(lines[0].includes("plain text line"));
});
