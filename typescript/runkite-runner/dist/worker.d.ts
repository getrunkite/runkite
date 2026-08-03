/**
 * Worker main loop: long-poll for jobs, execute LangGraph.js agents,
 * stream events back, watch for cancels, report final status. TypeScript
 * mirror of the Python runner's run_worker()/_poll_loop()/watch_cancels()
 * in worker.py -- same gRPC call sequence, same client-streaming-per-run
 * pattern, same cancel-registration approach.
 */
import { Metadata } from "@grpc/grpc-js";
import { LangGraphAdapter } from "./adapter.js";
import { type GetJobResponse, type RunnerServiceClient } from "./proto.js";
export interface WorkerOptions {
    configPath: string;
    grpcAddress: string;
    httpAddress: string;
    runnerKind: string;
    /** Max concurrent in-flight jobs per runner process -- mirrors the
     * Python runner's --concurrency/RUNKITE_CONCURRENCY. Default 1
     * preserves the original one-job-at-a-time behavior exactly. */
    concurrency?: number;
}
/**
 * Minimal counting semaphore for bounding concurrent job dispatch.
 * Node has no asyncio.Semaphore equivalent built in, and this is the
 * TypeScript mirror of the Python runner's `_poll_loop` dispatcher
 * (worker.py): acquire BEFORE GetJob, not after a job is received --
 * at capacity, the dispatcher simply doesn't poll for more work, which
 * is the backpressure this needs and nothing extra to build.
 * FIFO-ordered: waiters are released in the order they called
 * acquire(), same fairness asyncio.Semaphore provides.
 */
export declare class Semaphore {
    private available;
    private readonly waiters;
    constructor(concurrency: number);
    acquire(): Promise<void>;
    /** Releases one slot. If a waiter is queued, hands the slot straight
     * to it (the slot count never actually increments in that case) --
     * otherwise increments `available` for the next acquire() to claim. */
    release(): void;
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
/**
 * Executes one dispatched job end to end: register cancel, stream
 * events, execute_run, report status. Split out of pollLoop so the
 * dispatcher can run one of these per concurrency slot as an
 * independent, uncoupled promise -- TypeScript mirror of the Python
 * runner's `_handle_job` (worker.py). Everything here is fresh
 * per-call local state (runId, the streaming call, etc.), so there's
 * no shared-closure risk running many of these concurrently.
 *
 * Every error is caught HERE, not left to propagate to the dispatcher
 * -- with concurrency > 1, an unhandled rejection escaping one job's
 * promise must never take down the dispatcher (or, since Node has no
 * per-task isolation the way asyncio.Task does, risk becoming an
 * unhandled-rejection process crash) and every OTHER in-flight job
 * with it. The old single-job loop caught these at the outer
 * `while (true)` level; that's no longer where "one job" lives.
 */
export declare function handleJob(client: RunnerServiceClient, adapter: LangGraphAdapter, response: GetJobResponse, metadata: Metadata, pendingCancels: Map<string, CancelState>, opts?: {
    httpAddress?: string;
    runnerKind?: string;
    runnerToken?: string;
}): Promise<void>;
/**
 * Semaphore-bounded dispatcher: long-polls for jobs and hands each one
 * to its own handleJob call, up to `concurrency` running at once.
 * TypeScript mirror of the Python runner's `_poll_loop` (worker.py).
 * concurrency=1 (the default) preserves the exact original one-job-at-
 * a-time behavior -- GetJob is only called again once the single
 * in-flight job has finished.
 *
 * `inFlight`, if provided, is populated/drained by this function but
 * declared in the CALLER's scope (runWorker) specifically so it
 * survives this loop's own exit (normal return or a thrown error) --
 * same reason Python's `run_worker` declares `in_flight_tasks` outside
 * `_poll_loop` and drains it in a `finally` block afterward: shutting
 * down the checkpointer/store connections (which still-running jobs
 * depend on) out from under them, the instant this while(true) loop
 * happens to exit, would be a real bug, not just an edge case.
 */
export declare function pollLoop(client: RunnerServiceClient, adapter: LangGraphAdapter, runnerKind: string, metadata: Metadata, pendingCancels: Map<string, CancelState>, concurrency?: number, inFlight?: Set<Promise<void>>, jobOpts?: {
    httpAddress?: string;
    runnerToken?: string;
}): Promise<void>;
/** Reads the "custom_app" section straight from langgraph.json -- kept
 * separate so callers that only need worker options don't need to parse
 * config twice; re-exported here purely for cli.ts's convenience. */
export declare function readConfigFile(configPath: string): unknown;
