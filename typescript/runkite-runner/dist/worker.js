/**
 * Worker main loop: long-poll for jobs, execute LangGraph.js agents,
 * stream events back, watch for cancels, report final status. TypeScript
 * mirror of the Python runner's run_worker()/_poll_loop()/watch_cancels()
 * in worker.py -- same gRPC call sequence, same client-streaming-per-run
 * pattern, same cancel-registration approach.
 */
import { Metadata } from "@grpc/grpc-js";
import path from "node:path";
import { readFileSync } from "node:fs";
import { LangGraphAdapter, loadCustomAppConfig } from "./adapter.js";
import { CheckpointerManager } from "./checkpoint.js";
import { startHeartbeatLoop } from "./heartbeat.js";
import { RunkiteStore } from "./store.js";
import { executeRun } from "./executeRun.js";
import { runWithBinding } from "./tenantCtx.js";
import { initTracing, setRunStatus, withRunSpan } from "./tracing.js";
import { loadRequestHandler, serveCustomApp } from "./customApp.js";
import { logger } from "./logger.js";
import { shouldSkipRun } from "./runStatus.js";
import { createRunnerClient, } from "./proto.js";
import { grpcChannelCredentials } from "./tls.js";
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
export class Semaphore {
    available;
    waiters = [];
    constructor(concurrency) {
        if (concurrency < 1) {
            throw new Error(`concurrency must be >= 1, got ${concurrency}`);
        }
        this.available = concurrency;
    }
    acquire() {
        if (this.available > 0) {
            this.available--;
            return Promise.resolve();
        }
        return new Promise((resolve) => {
            this.waiters.push(resolve);
        });
    }
    /** Releases one slot. If a waiter is queued, hands the slot straight
     * to it (the slot count never actually increments in that case) --
     * otherwise increments `available` for the next acquire() to claim. */
    release() {
        const next = this.waiters.shift();
        if (next) {
            next();
        }
        else {
            this.available++;
        }
    }
}
// Generous vs. the millisecond-scale registration race this exists to
// close (see recordCancelSignal) -- long enough that no legitimate
// in-flight registration is still pending, short enough to bound the
// worst-case leak lifetime for an orphaned entry to a few minutes rather
// than the runner's entire uptime.
const ORPHAN_CANCEL_CLEANUP_MS = 5 * 60 * 1000;
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
export function recordCancelSignal(pendingCancels, runId) {
    const entry = pendingCancels.get(runId);
    if (entry) {
        entry.cancelled = true;
        return;
    }
    const fresh = { cancelled: true };
    pendingCancels.set(runId, fresh);
    const timer = setTimeout(() => {
        if (pendingCancels.get(runId) === fresh && !fresh._registered) {
            pendingCancels.delete(runId);
        }
    }, ORPHAN_CANCEL_CLEANUP_MS);
    timer.unref(); // don't keep the process alive just for this cleanup timer
}
/**
 * Registers a newly-dispatched run's cancel-tracking entry, reusing a
 * pre-arrived cancel signal (see recordCancelSignal) instead of
 * overwriting it with a fresh { cancelled: false } -- that overwrite is
 * exactly the race: without reusing the existing entry, a cancel that
 * arrived first would be silently lost the instant the run registers.
 * Marks the entry as claimed so recordCancelSignal's cleanup timer (if
 * any) leaves it alone for the run's real lifetime.
 */
export function registerRun(pendingCancels, runId) {
    const existing = pendingCancels.get(runId);
    if (existing) {
        existing._registered = true;
        return existing;
    }
    const fresh = { cancelled: false };
    pendingCancels.set(runId, fresh);
    return fresh;
}
function sleep(ms) {
    return new Promise((resolve) => setTimeout(resolve, ms));
}
/** Log W3C / correlation fields from RunAssignment.trace_context.
 * Span activation (when OTEL_* is configured) is handled by withRunSpan. */
function logTraceContext(runId, tc) {
    if (!tc)
        return;
    const parts = [`run_id=${runId}`];
    for (const key of ["correlation_id", "traceparent", "tracestate"]) {
        const val = tc[key];
        if (val)
            parts.push(`${key}=${val}`);
    }
    if (parts.length > 1)
        logger.info(`trace ${parts.join(" ")}`);
}
export async function runWorker(opts) {
    const tracingShutdown = await initTracing();
    const adapter = new LangGraphAdapter(opts.configPath);
    await adapter.load();
    // Runner auth, two-tier model: if the control plane has
    // RUNNER_TOKEN_<KIND> configured (production mode), this runner must
    // send a matching runner-kind/runner-token pair as gRPC metadata on
    // every call. In local mode this is a no-op -- runners are trusted
    // implicitly.
    const runnerToken = process.env.RUNNER_TOKEN ?? "";
    const metadata = new Metadata();
    if (runnerToken) {
        metadata.set("runner-kind", opts.runnerKind);
        metadata.set("runner-token", runnerToken);
    }
    const concurrency = opts.concurrency ?? 1;
    // node-postgres's own default pool size (10) is already fine for the
    // concurrency=1 common case -- a single in-flight job can still fan
    // out to several concurrent store/checkpoint calls internally (e.g.
    // a graph with parallel nodes each touching the store). Only grow the
    // pool for a HIGHER --concurrency, never shrink it below that
    // default; passing poolSize=1 here would be a real regression from
    // today's unconfigured behavior, not "mirroring Python's
    // pool_size=concurrency" -- Python's psycopg_pool has no equivalent
    // implicit floor to preserve.
    const poolSize = Math.max(concurrency, 10);
    const postgresDsn = process.env.POSTGRES_DSN;
    const checkpointerManager = new CheckpointerManager();
    await checkpointerManager.start(postgresDsn, poolSize);
    adapter.attachCheckpointer(checkpointerManager);
    const store = new RunkiteStore({
        postgresDsn,
        httpBaseUrl: opts.httpAddress,
        runnerToken: runnerToken || undefined,
        poolSize,
    });
    adapter.attachStore(store);
    logger.info(`Store mode: ${store.mode}`);
    // grpcChannelCredentials() (not just "did TLS env vars get read") is
    // the actual signal createRunnerClient itself uses to decide
    // insecure vs TLS -- checking the same function here (rather than
    // re-reading RUNKITE_TLS_CA_FILE directly) can't drift from what the
    // client connection actually does. Matches the Python runner's
    // identical "(TLS)" suffix in worker.py's run_worker.
    const tlsSuffix = grpcChannelCredentials() ? " (TLS)" : "";
    logger.info(`Connecting to control plane at ${opts.grpcAddress}${tlsSuffix}`);
    const client = createRunnerClient(opts.grpcAddress);
    // Track cancel state by run_id. watchCancelsLoop flips these; execute_run
    // polls isCancelled() for the currently-running job.
    const pendingCancels = new Map();
    let watcherStopped = false;
    async function watchCancelsLoop() {
        while (!watcherStopped) {
            try {
                await new Promise((resolve, reject) => {
                    const call = client.watchCancels({ runnerKind: opts.runnerKind }, metadata);
                    call.on("data", (signal) => {
                        logger.info(`Cancel signal received via gRPC for run ${signal.runId}`);
                        recordCancelSignal(pendingCancels, signal.runId);
                    });
                    call.on("error", reject);
                    call.on("end", resolve);
                });
            }
            catch (err) {
                logger.error("WatchCancels error:", err);
            }
            if (!watcherStopped)
                await sleep(1000);
        }
    }
    const cancelWatcherPromise = watchCancelsLoop();
    // Custom routes, in-runner mode. Same process, same event loop as the
    // poll loop below -- see customApp.ts's
    // doc comment for the trade-off that implies. Sidecar mode needs
    // nothing here: it's a separate process the control plane proxies to
    // directly, configured entirely on the Go side.
    let customAppHandle = null;
    const customAppConfig = loadCustomAppConfig(opts.configPath);
    if (customAppConfig) {
        const handler = await loadRequestHandler(path.dirname(path.resolve(opts.configPath)), customAppConfig.module);
        customAppHandle = serveCustomApp(handler, customAppConfig.host ?? "127.0.0.1", customAppConfig.port ?? 8100);
    }
    logger.info(`Worker ready. Polling for jobs as runner_kind=${opts.runnerKind} concurrency=${concurrency}`);
    // Populated/drained by pollLoop's dispatcher, but declared here so it
    // survives pollLoop's own exit -- see pollLoop's doc comment for why
    // draining still-running jobs before store/checkpointer teardown
    // matters, same as Python's run_worker/in_flight_tasks.
    const inFlight = new Set();
    try {
        await pollLoop(client, adapter, opts.runnerKind, metadata, pendingCancels, concurrency, inFlight, {
            httpAddress: opts.httpAddress,
            runnerToken,
        });
    }
    finally {
        watcherStopped = true;
        await cancelWatcherPromise.catch(() => { });
        if (customAppHandle)
            await customAppHandle.stop();
        if (inFlight.size > 0) {
            logger.info(`Draining ${inFlight.size} in-flight job(s) before shutdown...`);
            await Promise.allSettled([...inFlight]);
        }
        await checkpointerManager.stop();
        await store.close();
        await tracingShutdown();
    }
}
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
export async function handleJob(client, adapter, response, metadata, pendingCancels, opts) {
    let runId;
    // Fencing token -- see heartbeat.ts's doc comment. Hoisted here (not
    // just below), like runId, so the outer catch's own reportStatus call
    // always has a defined value even if JSON.parse/assignment.run_id
    // itself threw first.
    let generation = 0;
    try {
        const assignment = JSON.parse(response.assignmentJson);
        runId = assignment.run_id;
        // Defaults to 0 (unfenced) for a control plane that predates this
        // field, same convention as the Go/Python side.
        generation = assignment.generation ?? 0;
        logger.info(`Got job: run_id=${runId} graph_id=${assignment.graph_id}`);
        const tc = assignment.trace_context;
        logTraceContext(runId, tc);
        await withRunSpan({
            runId: runId,
            graphId: assignment.graph_id,
            threadId: assignment.thread_id,
            tenantId: assignment.tenant_id,
            traceContext: tc,
        }, async (span) => {
            await runWithBinding({ tenantId: assignment.tenant_id, runId: runId, generation }, async () => {
                const status = await handleJobUnderTenant(client, adapter, assignment, runId, generation, metadata, pendingCancels, opts);
                if (status)
                    setRunStatus(span, status);
            });
        });
    }
    catch (err) {
        logger.error(`Worker error handling run_id=${runId}:`, err);
        if (runId !== undefined) {
            const failedRunId = runId;
            await new Promise((resolve) => {
                client.reportStatus({
                    runId: failedRunId,
                    status: "error",
                    errorMessage: err instanceof Error ? err.message : String(err),
                    generation: String(generation),
                }, metadata, () => resolve());
            });
        }
    }
}
async function handleJobUnderTenant(client, adapter, assignment, runId, generation, metadata, pendingCancels, opts) {
    // PROTOCOL §10.3: cancel-after-dequeue guard before any agent work.
    if (opts?.httpAddress &&
        (await shouldSkipRun(opts.httpAddress, runId, {
            runnerKind: opts.runnerKind,
            runnerToken: opts.runnerToken,
        }))) {
        return undefined;
    }
    const cancelState = registerRun(pendingCancels, runId);
    // Started as soon as runId is known, BEFORE streamEvents' first
    // message -- see heartbeat.ts / the Python runner's mirrored change
    // in worker.py's _handle_job. onSuperseded shares cancelState with
    // executeRun below -- a superseded heartbeat sets the SAME flag a
    // real cancel signal would, so this runner stops cooperatively
    // instead of racing a second runner that's already taken over.
    const heartbeat = startHeartbeatLoop(client, runId, metadata, {
        generation,
        onSuperseded: () => {
            cancelState.cancelled = true;
        },
    });
    try {
        // Open one persistent client-streaming call per run, same pattern
        // as the Python runner's asyncio.Queue-backed generator -- the
        // ClientWritableStream's own internal buffering plays the same
        // role the queue does there.
        let resolveStream;
        let rejectStream;
        const streamDone = new Promise((resolve, reject) => {
            resolveStream = resolve;
            rejectStream = reject;
        });
        const call = client.streamEvents(metadata, (err, resp) => {
            if (err)
                rejectStream(err);
            else
                resolveStream(resp);
        });
        const sendEvent = async (event) => {
            call.write({ runId: runId, eventJson: JSON.stringify(event), generation: String(generation) });
        };
        const status = await executeRun(adapter, assignment, sendEvent, () => cancelState.cancelled);
        call.end();
        try {
            await streamDone;
        }
        catch (err) {
            logger.error("Stream finalization error:", err);
        }
        await new Promise((resolve, reject) => {
            client.reportStatus({
                runId: runId,
                status,
                errorMessage: status === "error" ? "see error event" : "",
                generation: String(generation),
            }, metadata, (err) => {
                if (err)
                    reject(err);
                else
                    resolve();
            });
        });
        logger.info(`Run completed: run_id=${runId} status=${status}`);
        return status;
    }
    finally {
        heartbeat.stop();
        // Always clear the cancel registration, even if executeRun itself
        // threw -- otherwise a failure here would leak an entry in
        // pendingCancels for every job that hits it.
        pendingCancels.delete(runId);
    }
}
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
export async function pollLoop(client, adapter, runnerKind, metadata, pendingCancels, concurrency = 1, inFlight, jobOpts) {
    const sem = new Semaphore(concurrency);
    while (true) {
        await sem.acquire();
        let response;
        try {
            response = await new Promise((resolve, reject) => {
                client.getJob({ runnerKind, timeoutSeconds: 30 }, metadata, (err, resp) => {
                    if (err)
                        reject(err);
                    else
                        resolve(resp);
                });
            });
        }
        catch (err) {
            logger.error("Worker error:", err);
            sem.release();
            await sleep(1000);
            continue;
        }
        if (!response.hasJob) {
            sem.release();
            continue; // no job, poll again
        }
        // Deliberately not awaited: this job runs concurrently with the
        // dispatcher's next iteration. handleJob catches all its own
        // errors (see its own doc comment), so this .finally() is only
        // ever responsible for releasing the semaphore slot and untracking
        // the job -- never for surfacing a rejection.
        const jobPromise = handleJob(client, adapter, response, metadata, pendingCancels, {
            httpAddress: jobOpts?.httpAddress,
            runnerKind,
            runnerToken: jobOpts?.runnerToken,
        });
        inFlight?.add(jobPromise);
        jobPromise.finally(() => {
            sem.release();
            inFlight?.delete(jobPromise);
        });
    }
}
/** Reads the "custom_app" section straight from langgraph.json -- kept
 * separate so callers that only need worker options don't need to parse
 * config twice; re-exported here purely for cli.ts's convenience. */
export function readConfigFile(configPath) {
    return JSON.parse(readFileSync(configPath, "utf-8"));
}
