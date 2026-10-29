import assert from "node:assert/strict";
import { test } from "node:test";
import { Metadata } from "@grpc/grpc-js";
import { CompletionDeliveryError } from "./completion.js";
import type { RunExecutor } from "./execution.js";
import type { RunEvent } from "./executeRun.js";
import type { ReportStatusRequest, RunEventProto, RunnerServiceClient, StreamEventsResponse } from "./proto.js";
import { runnerHeaders } from "./tenantCtx.js";
import { handleJob, recordCancelSignal, type CancelState } from "./worker.js";

function clientFixture() {
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
      callback(null, { ok: true });
    },
    heartbeat() {
      throw new Error("This short fixture must finish before its first heartbeat tick");
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

test("generic executor observes a cancel received before dispatch registration", async () => {
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
