/**
 * Self-check for heartbeat.ts's startHeartbeatLoop: the runner-side periodic call that keeps a job's
 * in-flight lease alive for its WHOLE execution, not just the first event.
 * TypeScript mirror of the Python runner's test_heartbeat.py.
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { Metadata } from "@grpc/grpc-js";
import { startHeartbeatLoop } from "./heartbeat.js";
import type { RunnerServiceClient, HeartbeatRequest, HeartbeatResponse } from "./proto.js";

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function fakeClient(
  onHeartbeat: (req: HeartbeatRequest) => Error | undefined,
): RunnerServiceClient {
  return {
    heartbeat(req: HeartbeatRequest, _metadata: Metadata, callback: (err: Error | null, resp: HeartbeatResponse) => void): void {
      const err = onHeartbeat(req);
      callback(err ?? null, { ok: !err });
    },
  } as unknown as RunnerServiceClient;
}

test("startHeartbeatLoop calls heartbeat repeatedly with the correct runId", async () => {
  const calls: string[] = [];
  const client = fakeClient((req) => {
    calls.push(req.runId);
    return undefined;
  });

  const handle = startHeartbeatLoop(client, "run-x", new Metadata(), 50);
  await sleep(230);
  handle.stop();

  assert.ok(calls.length >= 3, `expected at least 3 heartbeats sent in ~230ms at 50ms interval, got ${calls.length}`);
  assert.ok(calls.every((id) => id === "run-x"), "every heartbeat should carry the correct runId");
});

test("startHeartbeatLoop stops cleanly on stop() -- no calls land afterward", async () => {
  let count = 0;
  const client = fakeClient(() => {
    count++;
    return undefined;
  });

  const handle = startHeartbeatLoop(client, "run-y", new Metadata(), 50);
  await sleep(120);
  handle.stop();
  const countAtStop = count;

  await sleep(200);
  assert.equal(count, countAtStop, "no heartbeats should be sent after stop() was called");
});

test("startHeartbeatLoop survives a failed RPC and keeps ticking", async () => {
  let count = 0;
  const client = fakeClient(() => {
    count++;
    if (count === 1) return new Error("simulated transient failure");
    return undefined;
  });

  const handle = startHeartbeatLoop(client, "run-z", new Metadata(), 50);
  await sleep(230);
  handle.stop();

  assert.ok(count >= 3, `loop should keep running after the first RPC failed, got ${count} calls`);
});
