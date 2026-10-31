import assert from "node:assert/strict";
import { test } from "node:test";
import { pollRequest } from "./poll-request.ts";

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

async function flush() {
  await Promise.resolve();
  await Promise.resolve();
}

test("slow polling never overlaps requests or overwrites current state with a stale response", async (context) => {
  context.mock.timers.enable({ apis: ["setTimeout"] });
  const requests: ReturnType<typeof deferred<number>>[] = [];
  const values: number[] = [];
  const stop = pollRequest(
    () => {
      const result = deferred<number>();
      requests.push(result);
      return result.promise;
    },
    (value) => values.push(value),
    (error) => { throw error; },
    () => {},
    100,
  );
  context.mock.timers.tick(10_000);
  assert.equal(requests.length, 1);
  requests[0].resolve(1);
  await flush();
  context.mock.timers.tick(100);
  assert.equal(requests.length, 2);
  context.mock.timers.tick(10_000);
  assert.equal(requests.length, 2);
  requests[1].resolve(2);
  await flush();
  assert.deepEqual(values, [1, 2]);
  stop();
  context.mock.timers.tick(10_000);
  assert.equal(requests.length, 2);
});

test("navigation aborts the request and ignores a late response even when fetch ignores abort", async () => {
  const pending = deferred<number>();
  let signal: AbortSignal | undefined;
  let updates = 0;
  const stop = pollRequest(
    (current) => { signal = current; return pending.promise; },
    () => updates++,
    () => updates++,
    () => updates++,
  );
  stop();
  assert.equal(signal?.aborted, true);
  pending.resolve(1);
  await flush();
  assert.equal(updates, 0);
});

test("a failed refresh preserves the prior result and schedules recovery", async (context) => {
  context.mock.timers.enable({ apis: ["setTimeout"] });
  const values: number[] = [];
  const errors: unknown[] = [];
  let calls = 0;
  const stop = pollRequest(
    async () => {
      calls++;
      if (calls === 2) throw new Error("temporary outage");
      return calls;
    },
    (value) => values.push(value),
    (error) => errors.push(error),
    () => {},
    100,
  );
  await flush();
  context.mock.timers.tick(100);
  await flush();
  assert.deepEqual(values, [1]);
  assert.equal(errors.length, 1);
  context.mock.timers.tick(100);
  await flush();
  assert.deepEqual(values, [1, 3]);
  stop();
});
