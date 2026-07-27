export interface WorkerOptions {
    configPath: string;
    grpcAddress: string;
    httpAddress: string;
    runnerKind: string;
}
export interface CancelState {
    cancelled: boolean;
}
/**
 * Records that a cancel signal arrived for runId. Extracted from
 * watchCancelsLoop's stream handler specifically so this decision is
 * unit-testable without a real gRPC stream.
 *
 * Creates a fresh pre-registered entry if the run hasn't been registered
 * yet, rather than silently dropping the signal -- a real race, confirmed
 * live: WatchCancels is a single always-open stream, independent of the
 * per-run GetJob response, so a cancel signal can reach this runner before
 * pollLoop finishes registering the run it's for (client cancels near-
 * instantly after a run is created, while GetJob's response is still being
 * parsed). Pairs with registerRun below, which must NOT overwrite an
 * entry a cancel already touched.
 *
 * A pre-registered entry that registerRun never claims -- WatchCancels
 * broadcasts every cancel to every runner watching a runner_kind, so a
 * multi-instance deployment (or a cancel for an already-completed/
 * nonexistent run) routinely produces entries THIS process never claims
 * -- would otherwise leak for the runner's entire lifetime. Self-cleans
 * via a bounded timer instead.
 */
export declare function recordCancelSignal(pendingCancels: Map<string, CancelState>, runId: string): void;
/**
 * Registers a newly-dispatched run's cancel-tracking entry, reusing a
 * pre-arrived cancel signal (see recordCancelSignal) instead of
 * overwriting it with a fresh { cancelled: false } -- that overwrite is
 * exactly the race: without reusing the existing entry, a cancel that
 * arrived first would be silently lost the instant the run registers.
 * Marks the entry as claimed so recordCancelSignal's cleanup timer (if
 * any) leaves it alone for the run's real lifetime.
 */
export declare function registerRun(pendingCancels: Map<string, CancelState>, runId: string): CancelState;
export declare function runWorker(opts: WorkerOptions): Promise<void>;
/** Reads the "custom_app" section straight from langgraph.json -- kept
 * separate so callers that only need worker options don't need to parse
 * config twice; re-exported here purely for cli.ts's convenience. */
export declare function readConfigFile(configPath: string): unknown;
