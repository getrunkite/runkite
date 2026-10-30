import assert from "node:assert/strict";
import { test } from "node:test";
import { Metadata } from "@grpc/grpc-js";
import { CompletionDeliveryError } from "./completion.js";
import type { RunExecutor } from "./execution.js";
import type { RunEvent } from "./executeRun.js";
import type { ReportStatusRequest, RunEventProto, RunnerServiceClient, StreamEventsResponse } from "./proto.js";
import { runnerHeaders } from "./tenantCtx.js";
import { handleJob, recordCancelSignal, type CancelState } from "./worker.js";

function clientFixture(options: { supersededGeneration?: string } = {}) {
  const events: RunEventProto[] = [];
  const reports: ReportStatusRequest[] = [];
  const order: string[] = [];
  let cancelled = false;
  const client = {
    streamEvents(_metadata: unknown, callback: (error: Error | null, result?: StreamEventsResponse) => void) {
      return {
        write(event: RunEventProto) {
          events.push(event);
          order.push("event");
          return true;
        },
        end() {
          order.push("stream acknowledged");
          callback(null, { ok: true });
        },
        cancel() {
          cancelled = true;
          callback(new Error("stream cancelled"));
        },
      };
    },
    reportStatus(request: ReportStatusRequest, _metadata: unknown, callback: (error: null, value: unknown) => void) {
      reports.push(request);
      order.push("completion accepted");
      callback(null, { ok: true, superseded: request.generation === options.supersededGeneration });
    },
    heartbeat(request: { generation: string }, _metadata: unknown, callback: (error: null, value: unknown) => void) {
      if (options.supersededGeneration === undefined)
        throw new Error("This short fixture must finish before its first heartbeat tick");
      callback(null, { ok: true, superseded: request.generation === options.supersededGeneration });
    },
  } as unknown as RunnerServiceClient;
  return { client, events, reports, order, wasCancelled: () => cancelled };
}

const assignment = {
  run_id: "run-managed",
  thread_id: "thread-managed",
  graph_id: "agent-managed",
  tenant_id: "tenant-managed",
  generation: 7,
  input: { messages: [{ role: "user", content: "hello" }] },
};

function event(method: string, data: unknown): RunEvent {
  return { event_id: "run-managed-event", seq: 1, method, namespace: [], data, ts: Date.now() };
}

test("generic executor uses the same run binding, generation and acknowledged completion as LangGraph", async () => {
  const fixture = clientFixture();
  const pending = new Map<string, CancelState>();
  const executor: RunExecutor = {
    async execute(received, emit, isCancelled) {
      assert.deepEqual(received, assignment);
      assert.equal(isCancelled(), false);
      assert.deepEqual(runnerHeaders(), {
        "X-Runkite-Tenant-Id": "tenant-managed",
        "X-Runkite-Run-Id": "run-managed",
        "X-Runkite-Generation": "7",
        "X-Runner-Kind": "typescript-ai-sdk",
        "X-Runner-Token": "runner-token",
      });
      await emit(event("values", { messages: [{ role: "ai", content: "hello" }] }));
      await emit(event("end", { status: "success" }));
      return "success";
    },
  };
  await handleJob(
    fixture.client,
    executor,
    { hasJob: true, assignmentJson: JSON.stringify(assignment) },
    new Metadata(),
    pending,
    { runnerKind: "typescript-ai-sdk", runnerToken: "runner-token" },
  );
  assert.equal(fixture.events.length, 2);
  assert.ok(fixture.events.every((entry) => entry.runId === assignment.run_id && entry.generation === "7"));
  assert.equal(fixture.reports.length, 1);
  assert.equal(fixture.reports[0].status, "success");
  assert.equal(fixture.reports[0].generation, "7");
  assert.deepEqual(fixture.order, ["event", "event", "stream acknowledged", "completion accepted"]);
  assert.equal(pending.size, 0);
});

test("generic executor observes a cancel received before dispatch registration", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const fixture = clientFixture();
  const pending = new Map<string, CancelState>();
  recordCancelSignal(pending, assignment.run_id);
  const executor: RunExecutor = {
    async execute(_received, emit, isCancelled) {
      assert.equal(isCancelled(), true);
      await emit(event("end", { status: "interrupted" }));
      return "interrupted";
    },
  };
  await handleJob(
    fixture.client,
    executor,
    { hasJob: true, assignmentJson: JSON.stringify(assignment) },
    new Metadata(),
    pending,
  );
  assert.equal(fixture.reports[0].status, "interrupted");
  t.mock.timers.tick(5 * 60 * 1000);
  assert.equal(pending.size, 0);
});

test("unconfirmed executor persistence closes the stream and leaves completion recoverable", async () => {
  const fixture = clientFixture();
  const pending = new Map<string, CancelState>();
  const executor: RunExecutor = {
    async execute(_received, emit) {
      await emit(event("messages", { text: "progress" }));
      throw new CompletionDeliveryError("native state save was not confirmed");
    },
  };
  await handleJob(
    fixture.client,
    executor,
    { hasJob: true, assignmentJson: JSON.stringify(assignment) },
    new Metadata(),
    pending,
  );
  assert.equal(fixture.wasCancelled(), true);
  assert.deepEqual(fixture.reports, []);
  assert.equal(fixture.events.length, 1);
  assert.equal(JSON.parse(fixture.events[0].eventJson).method, "messages");
  assert.equal(pending.size, 0);
});

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

async function pausedAttempt(
  fixture: ReturnType<typeof clientFixture>,
  pending: Map<string, CancelState>,
  generation: number,
) {
  const started = deferred();
  const finish = deferred();
  let cancelled!: () => boolean;
  const executor: RunExecutor = {
    async execute(_received, emit, isCancelled) {
      cancelled = isCancelled;
      started.resolve();
      await finish.promise;
      const status = isCancelled() ? "interrupted" : "success";
      await emit(event("end", { status }));
      return status;
    },
  };
  const done = handleJob(
    fixture.client,
    executor,
    { hasJob: true, assignmentJson: JSON.stringify({ ...assignment, generation }) },
    new Metadata(),
    pending,
  );
  await started.promise;
  return { done, finish: finish.resolve, isCancelled: () => cancelled() };
}

test("a superseded heartbeat cannot interrupt a replacement executing in the same worker", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const fixture = clientFixture({ supersededGeneration: "7" });
  const pending = new Map<string, CancelState>();
  const older = await pausedAttempt(fixture, pending, 7);
  const current = await pausedAttempt(fixture, pending, 8);
  try {
    t.mock.timers.tick(2000);
    await Promise.resolve();
    assert.equal(older.isCancelled(), true);
    assert.equal(current.isCancelled(), false, "no user canceled the current generation");
  } finally {
    older.finish();
    current.finish();
    await Promise.all([older.done, current.done]);
  }
  assert.equal(fixture.reports.find((report) => report.generation === "8")?.status, "success");
  assert.equal(pending.size, 0);
});

test("finishing an older attempt does not hide a later user cancel from its replacement", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const fixture = clientFixture({ supersededGeneration: "7" });
  const pending = new Map<string, CancelState>();
  const older = await pausedAttempt(fixture, pending, 7);
  const current = await pausedAttempt(fixture, pending, 8);
  try {
    older.finish();
    await older.done;
    recordCancelSignal(pending, assignment.run_id);
    assert.equal(current.isCancelled(), true);
  } finally {
    current.finish();
    await current.done;
  }
  assert.equal(fixture.reports.find((report) => report.generation === "8")?.status, "interrupted");
  t.mock.timers.tick(5 * 60 * 1000);
  assert.equal(pending.size, 0);
});

test("WatchCancels fans a user cancel out to every active generation in handleJob", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const fixture = clientFixture({ supersededGeneration: "7" });
  const pending = new Map<string, CancelState>();
  const older = await pausedAttempt(fixture, pending, 7);
  const current = await pausedAttempt(fixture, pending, 8);
  try {
    recordCancelSignal(pending, assignment.run_id);
    assert.equal(older.isCancelled(), true);
    assert.equal(current.isCancelled(), true);
  } finally {
    older.finish();
    current.finish();
    await Promise.all([older.done, current.done]);
  }
  assert.ok(fixture.reports.every((report) => report.status === "interrupted"));
  t.mock.timers.tick(5 * 60 * 1000);
  assert.equal(pending.size, 0);
});
