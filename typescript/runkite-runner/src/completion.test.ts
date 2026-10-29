import assert from "node:assert/strict";
import { test } from "node:test";
import { Metadata } from "@grpc/grpc-js";
import { CompletionDeliveryError, finishRun } from "./completion.js";
import type { ReportStatusRequest, RunnerServiceClient } from "./proto.js";

const request = { runId: "run", generation: "2", status: "success", errorMessage: "" };
test("completion retries the same status without replaying execution", async () => {
  const seen: ReportStatusRequest[] = [];
  const client = {
    reportStatus: (r: ReportStatusRequest, _m: unknown, cb: Function) => {
      seen.push({ ...r });
      if (seen.length === 1) cb(new Error("storage unavailable"));
      else cb(null, { ok: true });
    },
  } as unknown as RunnerServiceClient;
  assert.equal(
    await finishRun(client, request, Promise.resolve({ ok: true }), new Metadata(), { retryDelayMs: 0 }),
    "success",
  );
  assert.deepEqual(seen, [request, request]);
});
test("unconfirmed stream preserves every agent outcome without reporting replacement status", async () => {
  const seen: ReportStatusRequest[] = [];
  let cancellations = 0;
  const client = {
    reportStatus: (r: ReportStatusRequest, _m: unknown, cb: Function) => {
      seen.push(r);
      cb(null, { ok: true });
    },
  } as unknown as RunnerServiceClient;
  for (const status of ["success", "error", "interrupted", "timeout"]) {
    const cause = new Error("stream close failed");
    const original = { ...request, status };
    await assert.rejects(
      finishRun(client, original, Promise.reject(cause), new Metadata(), { cancelStream: () => cancellations++ }),
      (error: unknown) => error instanceof CompletionDeliveryError && error.cause === cause,
    );
    assert.equal(original.status, status);
  }
  assert.deepEqual(seen, []);
  assert.equal(cancellations, 4);
});

test("a timed-out completion RPC is cancelled before retrying the same request", async () => {
  let cancellations = 0;
  const seen: ReportStatusRequest[] = [];
  const client = {
    reportStatus: (r: ReportStatusRequest, _m: unknown, cb: Function) => {
      seen.push({ ...r });
      if (seen.length === 2) cb(null, { ok: true });
      return { cancel: () => cancellations++ };
    },
  } as unknown as RunnerServiceClient;
  assert.equal(
    await finishRun(client, request, Promise.resolve({ ok: true }), new Metadata(), {
      rpcTimeoutMs: 10,
      retryDelayMs: 0,
    }),
    "success",
  );
  assert.equal(cancellations, 1);
  assert.deepEqual(seen, [request, request]);
});

test("a stalled event stream is cancelled without reporting an invented outcome", async () => {
  let cancellations = 0;
  const client = {
    reportStatus: () => assert.fail("An unacknowledged stream must not report a terminal status"),
  } as unknown as RunnerServiceClient;
  await assert.rejects(
    finishRun(client, request, new Promise(() => {}), new Metadata(), {
      streamTimeoutMs: 10,
      cancelStream: () => cancellations++,
    }),
    CompletionDeliveryError,
  );
  assert.equal(cancellations, 1);
});

test("negative stream acknowledgment keeps the assignment recoverable", async () => {
  const client = {
    reportStatus: () => assert.fail("A rejected stream must not report a terminal status"),
  } as unknown as RunnerServiceClient;
  await assert.rejects(
    finishRun(client, request, Promise.resolve({ ok: false }), new Metadata()),
    CompletionDeliveryError,
  );
});

test("superseded completion is not retried", async () => {
  let calls = 0;
  const client = {
    reportStatus: (_r: unknown, _m: unknown, cb: Function) => {
      calls++;
      cb(null, { superseded: true });
    },
  } as unknown as RunnerServiceClient;
  assert.equal(await finishRun(client, request, Promise.resolve({ ok: true }), new Metadata()), undefined);
  assert.equal(calls, 1);
});
test("retry exhaustion is distinguished from execution failure", async () => {
  const client = {
    reportStatus: (_r: unknown, _m: unknown, cb: Function) => cb(new Error("offline")),
  } as unknown as RunnerServiceClient;
  await assert.rejects(
    finishRun(client, request, Promise.resolve({ ok: true }), new Metadata(), { attempts: 2, retryDelayMs: 0 }),
    CompletionDeliveryError,
  );
});
