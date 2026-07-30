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
export declare const logger: {
    debug: (message: string, ...args: unknown[]) => void;
    info: (message: string, ...args: unknown[]) => void;
    warn: (message: string, ...args: unknown[]) => void;
    error: (message: string, ...args: unknown[]) => void;
};
