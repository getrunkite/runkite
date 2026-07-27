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

  const postgresDsn = process.env.POSTGRES_DSN;
  const checkpointerManager = new CheckpointerManager();
  await checkpointerManager.start(postgresDsn);
  adapter.attachCheckpointer(checkpointerManager);

  const store = new RunkiteStore({ postgresDsn, httpBaseUrl: opts.httpAddress, runnerToken: runnerToken || undefined });
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

  console.log(`Worker ready. Polling for jobs as runner_kind=${opts.runnerKind}`);

  try {
    await pollLoop(client, adapter, opts.runnerKind, metadata, pendingCancels);
  } finally {
    watcherStopped = true;
    await cancelWatcherPromise.catch(() => {});
    if (customAppHandle) await customAppHandle.stop();
    await checkpointerManager.stop();
    await store.close();
  }
}

async function pollLoop(
  client: RunnerServiceClient,
  adapter: LangGraphAdapter,
  runnerKind: string,
  metadata: Metadata,
  pendingCancels: Map<string, CancelState>,
): Promise<void> {
  while (true) {
    try {
      const response = await new Promise<GetJobResponse>((resolve, reject) => {
        client.getJob({ runnerKind, timeoutSeconds: 30 }, metadata, (err, resp) => {
          if (err) reject(err);
          else resolve(resp);
        });
      });

      if (!response.hasJob) continue; // no job, poll again

      const assignment: RunAssignment = JSON.parse(response.assignmentJson);
      const runId = assignment.run_id;
      console.log(`Got job: run_id=${runId} graph_id=${assignment.graph_id}`);

      const cancelState = registerRun(pendingCancels, runId);

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
        call.write({ runId, eventJson: JSON.stringify(event) });
      };

      const status = await executeRun(adapter, assignment, sendEvent, () => cancelState.cancelled);

      pendingCancels.delete(runId);

      call.end();
      try {
        await streamDone;
      } catch (err) {
        console.error("Stream finalization error:", err);
      }

      await new Promise<void>((resolve, reject) => {
        client.reportStatus(
          { runId, status, errorMessage: status === "error" ? "see error event" : "" },
          metadata,
          (err) => {
            if (err) reject(err);
            else resolve();
          },
        );
      });

      console.log(`Run completed: run_id=${runId} status=${status}`);
    } catch (err) {
      console.error("Worker error:", err);
      await sleep(1000);
    }
  }
}

/** Reads the "custom_app" section straight from langgraph.json -- kept
 * separate so callers that only need worker options don't need to parse
 * config twice; re-exported here purely for cli.ts's convenience. */
export function readConfigFile(configPath: string): unknown {
  return JSON.parse(readFileSync(configPath, "utf-8"));
}
