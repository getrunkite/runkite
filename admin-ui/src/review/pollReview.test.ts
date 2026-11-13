import assert from "node:assert/strict";
import { test } from "node:test";
import { pollReview } from "./pollReview.ts";

test("Review refresh is serialized and its hard budget aborts a slow final request", async (context) => {
  context.mock.timers.enable({ apis: ["setTimeout", "Date"], now: 0 });
  let calls = 0;
  let complete!: (value: string) => void;
  let lastSignal: AbortSignal | undefined;
  const values: string[] = [];
  const stop = pollReview((signal) => { calls++; lastSignal = signal; return new Promise<string>((resolve) => { complete = resolve; }); }, (value) => values.push(value), () => {});
  context.mock.timers.tick(100_000);
  assert.equal(calls, 1);
  complete("first");
  await Promise.resolve(); await Promise.resolve();
  context.mock.timers.tick(8000);
  assert.equal(calls, 2);
  context.mock.timers.tick(12_000);
  assert.equal(lastSignal?.aborted, true);
  complete("late");
  await Promise.resolve(); await Promise.resolve();
  context.mock.timers.tick(1_000_000);
  assert.equal(calls, 2);
  assert.deepEqual(values, ["first"]);
  stop();
});
