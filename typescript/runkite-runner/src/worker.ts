/**
 * Worker main loop: long-poll for jobs, execute LangGraph.js agents,
 * stream events back, watch for cancels, report final status. TypeScript
 * mirror of the Python runner's run_worker()/_poll_loop()/watch_cancels()
 * in worker.py -- same gRPC call sequence, same client-streaming-per-run
 * pattern, same cancel-registration approach.
 */
import { Metadata, type ClientWritableStream } from "@grpc/grpc-js";
import path from "node:path";
import { readFileSync } from "node:fs";
import { LangGraphAdapter, loadCustomAppConfig } from "./adapter.js";
import { CheckpointerManager } from "./checkpoint.js";
import { RunkiteStore } from "./store.js";
import { executeRun, type RunAssignment, type RunEvent } from "./executeRun.js";
import { loadRequestHandler, serveCustomApp } from "./customApp.js";
import {
  createRunnerClient,
  type CancelSignal,
  type GetJobResponse,
  type RunEventProto,
  type RunnerServiceClient,
  type StreamEventsResponse,
} from "./proto.js";

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
export class Semaphore {
  private available: number;
  private readonly waiters: Array<() => void> = [];

  constructor(concurrency: number) {
    if (concurrency < 1) {
      throw new Error(`concurrency must be >= 1, got ${concurrency}`);
    }
    this.available = concurrency;
  }

  acquire(): Promise<void> {
    if (this.available > 0) {
      this.available--;
      return Promise.resolve();
    }
    return new Promise<void>((resolve) => {
      this.waiters.push(resolve);
    });
  }

  /** Releases one slot. If a waiter is queued, hands the slot straight
   * to it (the slot count never actually increments in that case) --
   * otherwise increments `available` for the next acquire() to claim. */
  release(): void {
    const next = this.waiters.shift();
    if (next) {
      next();
    } else {
      this.available++;
    }
  }
}

export interface CancelState {
  cancelled: boolean;
}

// Internal-only bookkeeping, not part of the public CancelState contract
// (existing callers/tests only ever read/assert `.cancelled`).
interface TrackedCancelState extends CancelState {
  _registered?: boolean;
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
export function recordCancelSignal(pendingCancels: Map<string, CancelState>, runId: string): void {
  const entry = pendingCancels.get(runId) as TrackedCancelState | undefined;
  if (entry) {
    entry.cancelled = true;
    return;
  }
  const fresh: TrackedCancelState = { cancelled: true };
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
export function registerRun(pendingCancels: Map<string, CancelState>, runId: string): CancelState {
  const existing = pendingCancels.get(runId) as TrackedCancelState | undefined;
  if (existing) {
    existing._registered = true;
    return existing;
  }
  const fresh: TrackedCancelState = { cancelled: false };
  pendingCancels.set(runId, fresh);
  return fresh;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export async function runWorker(opts: WorkerOptions): Promise<void> {
  const adapter = new LangGraphAdapter(opts.configPath);
  await adapter.load();

  // Runner auth (master plan's "two-tier" model): if the control plane has
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
  console.log(`Store mode: ${store.mode}`);

  console.log(`Connecting to control plane at ${opts.grpcAddress}`);
  const client = createRunnerClient(opts.grpcAddress);

  // Track cancel state by run_id. watchCancelsLoop flips these; execute_run
  // polls isCancelled() for the currently-running job.
  const pendingCancels = new Map<string, CancelState>();
  let watcherStopped = false;

  async function watchCancelsLoop(): Promise<void> {
    while (!watcherStopped) {
      try {
        await new Promise<void>((resolve, reject) => {
          const call = client.watchCancels({ runnerKind: opts.runnerKind }, metadata);
          call.on("data", (signal: CancelSignal) => {
            console.log(`Cancel signal received via gRPC for run ${signal.runId}`);
            recordCancelSignal(pendingCancels, signal.runId);
          });
          call.on("error", reject);
          call.on("end", resolve);
        });
      } catch (err) {
        console.error("WatchCancels error:", err);
      }
      if (!watcherStopped) await sleep(1000);
    }
  }
  const cancelWatcherPromise = watchCancelsLoop();

  // Custom routes, in-runner mode (master plan: "Custom routes"). Same
  // process, same event loop as the poll loop below -- see customApp.ts's
  // doc comment for the trade-off that implies. Sidecar mode needs
  // nothing here: it's a separate process the control plane proxies to
  // directly, configured entirely on the Go side.
  let customAppHandle: ReturnType<typeof serveCustomApp> | null = null;
  const customAppConfig = loadCustomAppConfig(opts.configPath);
  if (customAppConfig) {
    const handler = await loadRequestHandler(path.dirname(path.resolve(opts.configPath)), customAppConfig.module);
    customAppHandle = serveCustomApp(handler, customAppConfig.host ?? "127.0.0.1", customAppConfig.port ?? 8100);
  }

  console.log(`Worker ready. Polling for jobs as runner_kind=${opts.runnerKind} concurrency=${concurrency}`);

  // Populated/drained by pollLoop's dispatcher, but declared here so it
  // survives pollLoop's own exit -- see pollLoop's doc comment for why
  // draining still-running jobs before store/checkpointer teardown
  // matters, same as Python's run_worker/in_flight_tasks.
  const inFlight = new Set<Promise<void>>();

  try {
    await pollLoop(client, adapter, opts.runnerKind, metadata, pendingCancels, concurrency, inFlight);
  } finally {
    watcherStopped = true;
    await cancelWatcherPromise.catch(() => {});
    if (customAppHandle) await customAppHandle.stop();
    if (inFlight.size > 0) {
      console.log(`Draining ${inFlight.size} in-flight job(s) before shutdown...`);
      await Promise.allSettled([...inFlight]);
    }
    await checkpointerManager.stop();
    await store.close();
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
export async function handleJob(
  client: RunnerServiceClient,
  adapter: LangGraphAdapter,
  response: GetJobResponse,
  metadata: Metadata,
  pendingCancels: Map<string, CancelState>,
): Promise<void> {
  let runId: string | undefined;
  try {
    const assignment: RunAssignment = JSON.parse(response.assignmentJson);
    runId = assignment.run_id;
    console.log(`Got job: run_id=${runId} graph_id=${assignment.graph_id}`);

    const cancelState = registerRun(pendingCancels, runId);

    try {
      // Open one persistent client-streaming call per run, same pattern
      // as the Python runner's asyncio.Queue-backed generator -- the
      // ClientWritableStream's own internal buffering plays the same
      // role the queue does there.
      let resolveStream!: (resp: StreamEventsResponse) => void;
      let rejectStream!: (err: Error) => void;
      const streamDone = new Promise<StreamEventsResponse>((resolve, reject) => {
        resolveStream = resolve;
        rejectStream = reject;
      });
      const call: ClientWritableStream<RunEventProto> = client.streamEvents(metadata, (err, resp) => {
        if (err) rejectStream(err);
        else resolveStream(resp);
      });

      const sendEvent = async (event: RunEvent): Promise<void> => {
        call.write({ runId: runId!, eventJson: JSON.stringify(event) });
      };

      const status = await executeRun(adapter, assignment, sendEvent, () => cancelState.cancelled);

      call.end();
      try {
        await streamDone;
      } catch (err) {
        console.error("Stream finalization error:", err);
      }

      await new Promise<void>((resolve, reject) => {
        client.reportStatus(
          { runId: runId!, status, errorMessage: status === "error" ? "see error event" : "" },
          metadata,
          (err) => {
            if (err) reject(err);
            else resolve();
          },
        );
      });

      console.log(`Run completed: run_id=${runId} status=${status}`);
    } finally {
      // Always clear the cancel registration, even if executeRun itself
      // threw -- otherwise a failure here would leak an entry in
      // pendingCancels for every job that hits it.
      pendingCancels.delete(runId);
    }
  } catch (err) {
    console.error(`Worker error handling run_id=${runId}:`, err);
    if (runId) {
      const failedRunId = runId;
      await new Promise<void>((resolve) => {
        client.reportStatus({ runId: failedRunId, status: "error", errorMessage: String(err) }, metadata, () => resolve());
      }).catch(() => {});
    }
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
export async function pollLoop(
  client: RunnerServiceClient,
  adapter: LangGraphAdapter,
  runnerKind: string,
  metadata: Metadata,
  pendingCancels: Map<string, CancelState>,
  concurrency = 1,
  inFlight?: Set<Promise<void>>,
): Promise<void> {
  const sem = new Semaphore(concurrency);

  while (true) {
    await sem.acquire();

    let response: GetJobResponse;
    try {
      response = await new Promise<GetJobResponse>((resolve, reject) => {
        client.getJob({ runnerKind, timeoutSeconds: 30 }, metadata, (err, resp) => {
          if (err) reject(err);
          else resolve(resp);
        });
      });
    } catch (err) {
      console.error("Worker error:", err);
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
    const jobPromise = handleJob(client, adapter, response, metadata, pendingCancels);
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
export function readConfigFile(configPath: string): unknown {
  return JSON.parse(readFileSync(configPath, "utf-8"));
}
