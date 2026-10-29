import assert from "node:assert/strict";
import { test } from "node:test";
import { reconcileRun } from "./reconcile-run.ts";

test("reconciliation waits for durable status and then stops", (context) => {
  context.mock.timers.enable({ apis: ["setTimeout"] });
  let requests = 0;
  let terminal = false;
  const stop = reconcileRun(
    () => requests++,
    () => terminal,
  );
  assert.equal(requests, 1);
  context.mock.timers.tick(250);
  assert.equal(requests, 2);
  terminal = true;
  context.mock.timers.tick(10000);
  assert.equal(requests, 2);
  stop();
});
test("navigation cleanup prevents further refreshes", (context) => {
  context.mock.timers.enable({ apis: ["setTimeout"] });
  let requests = 0;
  const stop = reconcileRun(
    () => requests++,
    () => false,
  );
  stop();
  context.mock.timers.tick(10000);
  assert.equal(requests, 1);
});
test("reconciliation has a bounded retry budget", (context) => {
  context.mock.timers.enable({ apis: ["setTimeout"] });
  let requests = 0;
  reconcileRun(
    () => requests++,
    () => false,
  );
  for (const delay of [250, 500, 1000, 2000, 4000, 10000])
    context.mock.timers.tick(delay);
  assert.equal(requests, 6);
});
