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
  onHeartbeat: (req: HeartbeatRequest) => Error | HeartbeatResponse | undefined,
): RunnerServiceClient {
  return {
    heartbeat(
      req: HeartbeatRequest,
      _metadata: Metadata,
      callback: (err: Error | null, resp: HeartbeatResponse) => void,
    ): void {
      const result = onHeartbeat(req);
      if (result instanceof Error) {
        callback(result, { ok: false, superseded: false });
      } else {
        callback(null, result ?? { ok: true, superseded: false });
      }
    },
  } as unknown as RunnerServiceClient;
}

test("startHeartbeatLoop calls heartbeat repeatedly with the correct runId", async () => {
  const calls: string[] = [];
  const client = fakeClient((req) => {
    calls.push(req.runId);
    return undefined;
  });

  const handle = startHeartbeatLoop(client, "run-x", new Metadata(), { intervalMs: 50 });
  await sleep(230);
  handle.stop();

  assert.ok(calls.length >= 3, `expected at least 3 heartbeats sent in ~230ms at 50ms interval, got ${calls.length}`);
  assert.ok(
    calls.every((id) => id === "run-x"),
    "every heartbeat should carry the correct runId",
  );
});

test("startHeartbeatLoop stops cleanly on stop() -- no calls land afterward", async () => {
  let count = 0;
  const client = fakeClient(() => {
    count++;
    return undefined;
  });

  const handle = startHeartbeatLoop(client, "run-y", new Metadata(), { intervalMs: 50 });
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

  const handle = startHeartbeatLoop(client, "run-z", new Metadata(), { intervalMs: 50 });
  await sleep(230);
  handle.stop();

  assert.ok(count >= 3, `loop should keep running after the first RPC failed, got ${count} calls`);
});

test("startHeartbeatLoop sends the given generation on every call", async () => {
  const generations: string[] = [];
  const client = fakeClient((req) => {
    generations.push(req.generation);
    return undefined;
  });

  const handle = startHeartbeatLoop(client, "run-gen", new Metadata(), { generation: 7, intervalMs: 50 });
  await sleep(120);
  handle.stop();

  assert.ok(generations.length >= 2, `expected at least 2 heartbeats, got ${generations.length}`);
  assert.ok(
    generations.every((g) => g === "7"),
    `every heartbeat should carry generation=7, got ${JSON.stringify(generations)}`,
  );
});

test("startHeartbeatLoop defaults generation to 0 when not given", async () => {
  const generations: string[] = [];
  const client = fakeClient((req) => {
    generations.push(req.generation);
    return undefined;
  });

  const handle = startHeartbeatLoop(client, "run-default-gen", new Metadata(), { intervalMs: 50 });
  await sleep(120);
  handle.stop();

  assert.ok(
    generations.every((g) => g === "0"),
    `unspecified generation should default to 0 (unfenced), got ${JSON.stringify(generations)}`,
  );
});

test("startHeartbeatLoop stops itself and fires onSuperseded when superseded=true", async () => {
  let count = 0;
  let supersededFired = false;
  const client = fakeClient(() => {
    count++;
    return { ok: true, superseded: count >= 2 };
  });

  const handle = startHeartbeatLoop(client, "run-superseded", new Metadata(), {
    generation: 1,
    intervalMs: 50,
    onSuperseded: () => {
      supersededFired = true;
    },
  });
  // 2nd call (superseded=true) lands at ~100ms; give it real margin.
  await sleep(300);

  assert.ok(supersededFired, "onSuperseded should have fired");
  const countAtSupersede = count;
  await sleep(150);
  assert.equal(count, countAtSupersede, "no further heartbeat calls after superseded");

  handle.stop(); // no-op, but exercises the already-stopped path cleanly
});

test("startHeartbeatLoop does not fire onSuperseded while not superseded", async () => {
  let supersededFired = false;
  const client = fakeClient(() => ({ ok: true, superseded: false }));

  const handle = startHeartbeatLoop(client, "run-not-superseded", new Metadata(), {
    generation: 1,
    intervalMs: 50,
    onSuperseded: () => {
      supersededFired = true;
    },
  });
  await sleep(120);
  assert.equal(supersededFired, false, "onSuperseded should not fire while not superseded");
  handle.stop();
});

test("startHeartbeatLoop superseded without onSuperseded does not throw", async () => {
  const client = fakeClient(() => ({ ok: true, superseded: true }));

  const handle = startHeartbeatLoop(client, "run-no-callback", new Metadata(), { generation: 1, intervalMs: 50 });
  await sleep(150);
  handle.stop(); // must not throw even though onSuperseded was never provided
});
