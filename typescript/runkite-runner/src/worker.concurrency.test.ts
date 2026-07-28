import { test } from "node:test";
import assert from "node:assert/strict";
import { Metadata, type ClientWritableStream } from "@grpc/grpc-js";
import { Semaphore, pollLoop, handleJob, type CancelState } from "./worker.js";
import type { LangGraphAdapter, RunnableGraph } from "./adapter.js";
import type { RunnerServiceClient, GetJobResponse, StreamEventsResponse, ReportStatusResponse } from "./proto.js";

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// -- Semaphore ----------------------------------------------------------

test("Semaphore: acquire() resolves immediately while slots are available", async () => {
  const sem = new Semaphore(2);
  const start = Date.now();
  await sem.acquire();
  await sem.acquire();
  assert.ok(Date.now() - start < 50, "both acquires within capacity should resolve near-instantly");
});

test("Semaphore: acquire() blocks at capacity until release()", async () => {
  const sem = new Semaphore(1);
  await sem.acquire();

  let acquired = false;
  const p = sem.acquire().then(() => {
    acquired = true;
  });

  await sleep(20);
  assert.equal(acquired, false, "second acquire() must not resolve before release()");

  sem.release();
  await p;
  assert.equal(acquired, true, "second acquire() must resolve once a slot is released");
});

test("Semaphore: releases are handed to waiters in FIFO order", async () => {
  const sem = new Semaphore(1);
  await sem.acquire(); // take the only slot

  const order: number[] = [];
  const p1 = sem.acquire().then(() => order.push(1));
  const p2 = sem.acquire().then(() => order.push(2));
  const p3 = sem.acquire().then(() => order.push(3));

  sem.release();
  await p1;
  sem.release();
  await p2;
  sem.release();
  await p3;

  assert.deepEqual(order, [1, 2, 3], "waiters must be released in the order they called acquire()");
});

test("Semaphore: a release with no waiters simply increments availability for the next acquire()", async () => {
  const sem = new Semaphore(1);
  await sem.acquire();
  sem.release(); // no one waiting
  // Should not throw, and the slot should be available again.
  const start = Date.now();
  await sem.acquire();
  assert.ok(Date.now() - start < 50);
});

test("Semaphore: throws on construction with concurrency < 1", () => {
  assert.throws(() => new Semaphore(0), /concurrency must be >= 1/);
  assert.throws(() => new Semaphore(-1), /concurrency must be >= 1/);
});

// -- pollLoop / handleJob dispatcher --------------------------------------

/** A graph whose stream() waits `delayMs` before yielding one "values"
 * event -- lets tests observe overlap (or lack of it) between
 * concurrently dispatched jobs by their timing. */
function fakeGraph(delayMs: number): RunnableGraph {
  return {
    async stream() {
      await sleep(delayMs);
      return (async function* () {
        yield ["values", { messages: [{ role: "ai", content: "done" }] }];
      })();
    },
  };
}

function fakeAdapter(graph: RunnableGraph): LangGraphAdapter {
  return { isFactory: () => false, getGraph: () => graph } as unknown as LangGraphAdapter;
}

function makeAssignment(runId: string, graphId = "test_graph") {
  return {
    run_id: runId,
    thread_id: `thread-${runId}`,
    graph_id: graphId,
    input: { messages: [{ role: "user", content: "hi" }] },
    stream_modes: ["values"],
  };
}

/** Fake gRPC client: getJob() dequeues from a fixed job list (then
 * blocks forever, never resolving -- matching a real idle long-poll,
 * so a test's dispatcher loop harmlessly parks instead of erroring or
 * spinning once every real job has been dispatched). streamEvents()
 * and reportStatus() just record what was sent, no real streaming. */
function makeFakeClient(jobs: GetJobResponse[]) {
  const queue = [...jobs];
  const reportedStatuses: Array<{ runId: string; status: string }> = [];
  const client = {
    getJob: (_req: unknown, _meta: unknown, cb: (err: Error | null, resp: GetJobResponse) => void) => {
      const job = queue.shift();
      if (job) {
        setImmediate(() => cb(null, job));
      }
      // else: never calls back -- simulates an idle long-poll parked
      // mid-flight, not an error or an instant "no job" retry storm.
    },
    streamEvents: (_meta: unknown, cb: (err: Error | null, resp: StreamEventsResponse) => void) => {
      const stream = {
        write: () => true,
        end: () => setImmediate(() => cb(null, { ok: true })),
      };
      return stream as unknown as ClientWritableStream<unknown>;
    },
    reportStatus: (
      req: { runId: string; status: string },
      _meta: unknown,
      cb: (err: Error | null, resp: ReportStatusResponse) => void,
    ) => {
      reportedStatuses.push({ runId: req.runId, status: req.status });
      setImmediate(() => cb(null, { ok: true }));
    },
  } as unknown as RunnerServiceClient;
  return { client, reportedStatuses };
}

function jobResponse(runId: string, graphId = "test_graph"): GetJobResponse {
  return { hasJob: true, assignmentJson: JSON.stringify(makeAssignment(runId, graphId)) };
}

test("pollLoop with concurrency=1 processes jobs strictly one at a time (backward compatible)", async () => {
  let concurrent = 0;
  let peakConcurrent = 0;
  const graph: RunnableGraph = {
    async stream() {
      concurrent++;
      peakConcurrent = Math.max(peakConcurrent, concurrent);
      await sleep(20);
      concurrent--;
      return (async function* () {
        yield ["values", {}];
      })();
    },
  };

  const { client, reportedStatuses } = makeFakeClient([jobResponse("run-1"), jobResponse("run-2"), jobResponse("run-3")]);
  const adapter = fakeAdapter(graph);
  const pendingCancels = new Map<string, CancelState>();

  // pollLoop never returns -- run it unawaited and just wait long enough
  // for the 3 queued jobs (each ~20ms) to be dispatched and finished.
  void pollLoop(client, adapter, "typescript-langgraphjs", new Metadata(), pendingCancels, 1);
  await sleep(150);

  assert.equal(peakConcurrent, 1, "concurrency=1 must never run more than one job's graph.stream() at once");
  assert.deepEqual(
    reportedStatuses.map((s) => s.runId),
    ["run-1", "run-2", "run-3"],
    "all three jobs must complete, in order, under the default concurrency",
  );
  assert.ok(
    reportedStatuses.every((s) => s.status === "success"),
    "all three jobs must succeed",
  );
});

test("pollLoop with concurrency=3 runs multiple jobs' graphs concurrently, not sequentially", async () => {
  let concurrent = 0;
  let peakConcurrent = 0;
  const graph: RunnableGraph = {
    async stream() {
      concurrent++;
      peakConcurrent = Math.max(peakConcurrent, concurrent);
      await sleep(30);
      concurrent--;
      return (async function* () {
        yield ["values", {}];
      })();
    },
  };

  const { client, reportedStatuses } = makeFakeClient([jobResponse("run-1"), jobResponse("run-2"), jobResponse("run-3")]);
  const adapter = fakeAdapter(graph);
  const pendingCancels = new Map<string, CancelState>();

  void pollLoop(client, adapter, "typescript-langgraphjs", new Metadata(), pendingCancels, 3);
  await sleep(100);

  assert.equal(peakConcurrent, 3, "concurrency=3 must let all three jobs' graph.stream() overlap");
  assert.equal(reportedStatuses.length, 3, "all three jobs must still complete");
  assert.ok(reportedStatuses.every((s) => s.status === "success"));
});

test("pollLoop with concurrency=2 dispatches a 3rd job only once a slot frees up (bounded, not unlimited)", async () => {
  let concurrent = 0;
  let peakConcurrent = 0;
  const graph: RunnableGraph = {
    async stream() {
      concurrent++;
      peakConcurrent = Math.max(peakConcurrent, concurrent);
      await sleep(40);
      concurrent--;
      return (async function* () {
        yield ["values", {}];
      })();
    },
  };

  const { client, reportedStatuses } = makeFakeClient([jobResponse("run-1"), jobResponse("run-2"), jobResponse("run-3")]);
  const adapter = fakeAdapter(graph);
  const pendingCancels = new Map<string, CancelState>();

  void pollLoop(client, adapter, "typescript-langgraphjs", new Metadata(), pendingCancels, 2);
  await sleep(120);

  assert.equal(peakConcurrent, 2, "concurrency=2 must cap concurrent jobs at 2, even with 3 immediately available");
  assert.equal(reportedStatuses.length, 3, "the 3rd job must still eventually run once a slot frees up");
});

test("handleJob reports status=error and does not throw when the graph is missing (adapter.getGraph fails)", async () => {
  const adapter = {
    isFactory: () => false,
    getGraph: () => {
      throw new Error("Graph not found: nonexistent_graph");
    },
  } as unknown as LangGraphAdapter;

  const { client, reportedStatuses } = makeFakeClient([]);
  const pendingCancels = new Map<string, CancelState>();

  await assert.doesNotReject(() =>
    handleJob(client, adapter, jobResponse("run-bad", "nonexistent_graph"), new Metadata(), pendingCancels),
  );

  assert.equal(reportedStatuses.length, 1);
  assert.equal(reportedStatuses[0].runId, "run-bad");
  assert.equal(reportedStatuses[0].status, "error");
});

test("pollLoop: one job failing (bad graph_id) does not stop the dispatcher from processing the next job", async () => {
  const goodGraph = fakeGraph(5);
  const adapter = {
    isFactory: () => false,
    getGraph: (graphId: string) => {
      if (graphId === "missing_graph") throw new Error(`Graph not found: ${graphId}`);
      return goodGraph;
    },
  } as unknown as LangGraphAdapter;

  const { client, reportedStatuses } = makeFakeClient([jobResponse("run-fail", "missing_graph"), jobResponse("run-ok", "test_graph")]);
  const pendingCancels = new Map<string, CancelState>();

  void pollLoop(client, adapter, "typescript-langgraphjs", new Metadata(), pendingCancels, 1);
  await sleep(80);

  assert.deepEqual(
    reportedStatuses.map((s) => ({ runId: s.runId, status: s.status })),
    [
      { runId: "run-fail", status: "error" },
      { runId: "run-ok", status: "success" },
    ],
    "the dispatcher must survive one job's failure and still process the next job normally",
  );
});

test("pollLoop tracks dispatched jobs in the optional inFlight set, and untracks them on completion", async () => {
  const graph = fakeGraph(30);
  const adapter = fakeAdapter(graph);
  const { client } = makeFakeClient([jobResponse("run-1"), jobResponse("run-2")]);
  const pendingCancels = new Map<string, CancelState>();
  const inFlight = new Set<Promise<void>>();

  void pollLoop(client, adapter, "typescript-langgraphjs", new Metadata(), pendingCancels, 2, inFlight);

  // Give the dispatcher one tick to acquire both semaphore slots and
  // dispatch both jobs, but not long enough for either job's 30ms
  // stream() delay to have resolved yet.
  await sleep(10);
  assert.equal(inFlight.size, 2, "both dispatched jobs must be tracked while still running");

  await sleep(60);
  assert.equal(inFlight.size, 0, "completed jobs must be untracked -- draining later must not wait on stale entries");
});

test("runWorker-style shutdown: draining inFlight before teardown lets a still-running job finish instead of being abandoned", async () => {
  // Mirrors what runWorker's finally block does with the inFlight set
  // pollLoop populates -- verifies the DRAINING pattern itself (not
  // runWorker, which needs a real gRPC client/adapter/config file to
  // construct end to end), same scope every other dispatcher test here
  // covers pollLoop/handleJob directly rather than through runWorker.
  let jobFullyCompleted = false;
  const graph: RunnableGraph = {
    async stream() {
      await sleep(30);
      jobFullyCompleted = true;
      return (async function* () {
        yield ["values", {}];
      })();
    },
  };
  const adapter = fakeAdapter(graph);
  const { client, reportedStatuses } = makeFakeClient([jobResponse("run-1")]);
  const pendingCancels = new Map<string, CancelState>();
  const inFlight = new Set<Promise<void>>();

  const loopPromise = pollLoop(client, adapter, "typescript-langgraphjs", new Metadata(), pendingCancels, 1, inFlight);
  await sleep(10); // let the dispatcher pick up the one job

  // Simulate the shutdown drain runWorker's finally block does --
  // deliberately NOT awaiting loopPromise itself (pollLoop never
  // returns on its own), just draining whatever's in inFlight.
  await Promise.allSettled([...inFlight]);

  assert.equal(jobFullyCompleted, true, "draining must wait for the in-flight job's own stream() to actually finish");
  assert.equal(reportedStatuses.length, 1);
  assert.equal(reportedStatuses[0].status, "success");
  void loopPromise; // still running, parked on the fake client's never-resolving next GetJob -- harmless
});

test("handleJob clears its pendingCancels entry even when the job fails", async () => {
  const adapter = {
    isFactory: () => false,
    getGraph: () => {
      throw new Error("boom");
    },
  } as unknown as LangGraphAdapter;

  const { client } = makeFakeClient([]);
  const pendingCancels = new Map<string, CancelState>();

  await handleJob(client, adapter, jobResponse("run-1"), new Metadata(), pendingCancels);

  assert.equal(pendingCancels.has("run-1"), false, "a failed job must not leak its cancel-tracking entry");
});
