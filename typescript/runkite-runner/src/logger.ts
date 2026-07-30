/**
 * Shared logger for the TypeScript runner. Before this, every module
 * called raw console.log/warn/error directly -- no level filtering, no
 * structured/JSON option, no way to quiet the runner down or feed a log
 * aggregator.
 *
 * LOG_LEVEL and LOG_FORMAT env vars mirror the Go control plane's
 * cmd/logging.go and the Python runner's logging_config.py, so the same
 * two env vars configure logging consistently across all three SDKs.
 */

type Level = "debug" | "info" | "warn" | "error";

const LEVEL_RANK: Record<Level, number> = { debug: 0, info: 1, warn: 2, error: 3 };

function currentLevel(): Level {
  const raw = (process.env.LOG_LEVEL ?? "info").trim().toLowerCase();
  return raw === "debug" || raw === "warn" || raw === "error" ? raw : "info";
}

function isJSON(): boolean {
  return (process.env.LOG_FORMAT ?? "text").trim().toLowerCase() === "json";
}

// Re-read env vars on every call (not cached at module-load time) so
// tests can flip LOG_LEVEL/LOG_FORMAT between assertions without
// re-importing the module -- these are called at most a few times per
// second even under load, so the getenv cost is not worth the
// staleness risk of caching it once at import time.
function log(level: Level, message: string, ...args: unknown[]): void {
  if (LEVEL_RANK[level] < LEVEL_RANK[currentLevel()]) return;

  const sink = level === "error" || level === "warn" ? console.error : console.log;

  if (isJSON()) {
    const payload: Record<string, unknown> = {
      time: new Date().toISOString(),
      level,
      message,
    };
    // Mirrors the Go/Python side's convention of attaching extra values
    // as structured fields rather than string-concatenating them --
    // args here are typically an Error or a handful of primitives, not
    // named key/value pairs, so they're captured under "args" rather
    // than spread into top-level keys (there's no name to key them by).
    if (args.length > 0) payload.args = args.map(serializeArg);
    sink(JSON.stringify(payload));
    return;
  }

  sink(`${new Date().toISOString()} [${level.toUpperCase()}] ${message}`, ...args);
}

function serializeArg(arg: unknown): unknown {
  if (arg instanceof Error) return { name: arg.name, message: arg.message, stack: arg.stack };
  return arg;
}

export const logger = {
  debug: (message: string, ...args: unknown[]): void => log("debug", message, ...args),
  info: (message: string, ...args: unknown[]): void => log("info", message, ...args),
  warn: (message: string, ...args: unknown[]): void => log("warn", message, ...args),
  error: (message: string, ...args: unknown[]): void => log("error", message, ...args),
};
