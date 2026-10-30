import { test } from "node:test";
import assert from "node:assert/strict";
import { recordCancelSignal, registerRun, unregisterRun, type CancelState } from "./worker.js";

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

test("superseding one attempt does not cancel another registration of the same run", () => {
  const pending = new Map<string, CancelState>();
  const older = registerRun(pending, "run-1");
  older.cancelled = true;
  const current = registerRun(pending, "run-1");

  assert.equal(current.cancelled, false, "supersession belongs only to the older attempt");
  recordCancelSignal(pending, "run-1");
  assert.equal(current.cancelled, true, "a genuine user cancel still reaches the current attempt");
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
  assert.equal(state.cancelled, true, "expected the pre-arrived cancel to still be observed after the window");
});

test("user cancellation reaches every overlapping attempt and later registrations", () => {
  const pending = new Map<string, CancelState>();
  const first = registerRun(pending, "run-1");
  const second = registerRun(pending, "run-1");
  recordCancelSignal(pending, "run-1");
  const third = registerRun(pending, "run-1");
  assert.ok([first, second, third].every((state) => state.cancelled));
});

test("an earlier attempt's cleanup cannot detach another active attempt", () => {
  const pending = new Map<string, CancelState>();
  const first = registerRun(pending, "run-1");
  const second = registerRun(pending, "run-1");
  unregisterRun(pending, "run-1", first);
  recordCancelSignal(pending, "run-1");
  assert.equal(second.cancelled, true);
});

test("a repeated cleanup cannot remove a newer registration", () => {
  const pending = new Map<string, CancelState>();
  const old = registerRun(pending, "run-1");
  unregisterRun(pending, "run-1", old);
  assert.equal(pending.size, 0);
  const current = registerRun(pending, "run-1");
  unregisterRun(pending, "run-1", old);
  recordCancelSignal(pending, "run-1");
  assert.equal(current.cancelled, true);
});

test("a real cancel survives the last cleanup for a dispatch still awaiting registration, then expires", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const pending = new Map<string, CancelState>();
  const old = registerRun(pending, "run-1");
  recordCancelSignal(pending, "run-1");
  unregisterRun(pending, "run-1", old);
  const current = registerRun(pending, "run-1");
  assert.equal(current.cancelled, true);
  t.mock.timers.tick(5 * 60 * 1000);
  assert.equal(pending.has("run-1"), true, "an expiry timer must not remove an active attempt");
  unregisterRun(pending, "run-1", current);
  t.mock.timers.tick(5 * 60 * 1000);
  assert.equal(pending.size, 0, "the canceled tombstone must not leak after every attempt leaves");
});

test("another real cancel refreshes an orphan tombstone instead of losing it to the older timer", (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  const pending = new Map<string, CancelState>();
  recordCancelSignal(pending, "run-1");
  t.mock.timers.tick(4 * 60 * 1000);
  recordCancelSignal(pending, "run-1");
  t.mock.timers.tick(2 * 60 * 1000);
  assert.equal(pending.has("run-1"), true);
  t.mock.timers.tick(3 * 60 * 1000);
  assert.equal(pending.size, 0);
});
