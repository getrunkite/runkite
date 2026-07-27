import { test } from "node:test";
import assert from "node:assert/strict";
import { recordCancelSignal, registerRun, type CancelState } from "./worker.js";

test("registerRun before recordCancelSignal: cancel observed normally", () => {
  const pending = new Map<string, CancelState>();
  const state = registerRun(pending, "run-1");
  assert.equal(state.cancelled, false);

  recordCancelSignal(pending, "run-1");
  // Same object identity -- executeRun's isCancelled() closure over
  // `state` must see the mutation.
  assert.equal(state.cancelled, true);
});

// The actual race this pair of functions exists to close: WatchCancels is
// a single always-open stream, independent of the per-run GetJob
// response -- a cancel signal can reach the runner before pollLoop
// finishes registering the run it's for.
test("recordCancelSignal before registerRun: cancel is NOT dropped (the race)", () => {
  const pending = new Map<string, CancelState>();

  recordCancelSignal(pending, "run-1"); // cancel arrives first
  const state = registerRun(pending, "run-1"); // run registers afterward

  assert.equal(state.cancelled, true, "expected the pre-arrived cancel to be observed, not overwritten with false");
});

test("registerRun reuses the exact same object a pre-arrived cancel touched", () => {
  const pending = new Map<string, CancelState>();
  recordCancelSignal(pending, "run-1");
  const preRegistered = pending.get("run-1");

  const state = registerRun(pending, "run-1");
  assert.equal(state, preRegistered, "expected registerRun to reuse the pre-existing entry, not replace it");
});

test("cancels for different run_ids never interfere", () => {
  const pending = new Map<string, CancelState>();
  const stateA = registerRun(pending, "run-a");
  recordCancelSignal(pending, "run-b");

  assert.equal(stateA.cancelled, false, "run-a must be unaffected by a cancel for run-b");
  assert.equal(pending.get("run-b")!.cancelled, true);
});

test("recordCancelSignal for a never-registered run leaves a claimable entry immediately after", () => {
  const pending = new Map<string, CancelState>();
  recordCancelSignal(pending, "orphan-run");
  assert.equal(pending.get("orphan-run")!.cancelled, true);
});

// The orphan-entry leak this closes: WatchCancels broadcasts every cancel
// to every runner watching a runner_kind, so a multi-instance deployment
// (or a cancel for an already-completed/nonexistent run) routinely
// produces a pre-registered entry THIS process never claims via
// registerRun -- without the self-cleaning timer, that entry would sit in
// pendingCancels for the runner's entire remaining lifetime.
test("an orphaned pre-registered entry (never claimed by registerRun) is evicted after the cleanup window", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const pending = new Map<string, CancelState>();

  recordCancelSignal(pending, "orphan-run");
  assert.ok(pending.has("orphan-run"), "expected the pre-registered entry to exist right after the signal");

  t.mock.timers.tick(5 * 60 * 1000);
  assert.ok(!pending.has("orphan-run"), "expected the orphaned entry to be evicted after the cleanup window");
});

test("a legitimately claimed entry survives past the cleanup window (not evicted out from under a real run)", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const pending = new Map<string, CancelState>();

  recordCancelSignal(pending, "run-1"); // pre-arrives before registration
  const state = registerRun(pending, "run-1"); // claimed before the cleanup timer fires

  t.mock.timers.tick(5 * 60 * 1000);
  assert.ok(pending.has("run-1"), "expected a claimed entry to survive the cleanup window");
  assert.equal(pending.get("run-1"), state, "expected the same object identity executeRun's isCancelled() closes over");
  assert.equal(state.cancelled, true, "expected the pre-arrived cancel to still be observed after the window");
});
