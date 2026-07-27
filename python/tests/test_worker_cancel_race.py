"""Self-check for worker.py's (the production LangGraph runner) cancel
registration race -- the same class of bug the TypeScript runner's
recordCancelSignal/registerRun and generic_worker's pre_cancelled/
register_run already fix, found here via an independent audit of this
codebase after those fixes existed elsewhere: a cancel signal arriving
on the separate WatchCancels stream before _poll_loop has registered
run_id in pending_cancels (GetJob -> parse -> register is real elapsed
time) used to be silently dropped -- `ev is None` -> nothing happens --
so a cancel that raced job dispatch was permanently ignored.

Proves:
1. register_run returns an already-set Event when a cancel signal
   pre-arrived for that run_id (claims it from pre_cancelled).
2. register_run returns a normal unset Event for a run_id with no
   pre-arrived signal.
3. Claiming a pre-arrived signal removes it from pre_cancelled (no
   dangling entry left behind for a later, unrelated run reusing the
   same run_id space).

Usage:
    python/.venv/bin/python python/tests/test_worker_cancel_race.py
"""

import asyncio
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner.worker import register_run  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


async def test_pre_arrived_cancel_is_observed():
    pending: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()

    # Simulates watch_cancels() observing a signal for a run_id the
    # poll loop hasn't registered yet.
    pre_cancelled.add("early-run")

    ev = await register_run(pending, pre_cancelled, lock, "early-run")
    check("pre-arrived cancel leaves the Event already set", ev.is_set())
    check("pre_cancelled entry is claimed, not left dangling", "early-run" not in pre_cancelled)
    check("the Event is stored in pending_cancels under its run_id", pending.get("early-run") is ev)


async def test_normal_registration_is_unaffected():
    pending: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()

    ev = await register_run(pending, pre_cancelled, lock, "normal-run")
    check("a run with no pre-arrived signal gets a normal unset Event", not ev.is_set())


async def test_unrelated_pre_cancelled_entries_dont_leak_across_runs():
    pending: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()

    pre_cancelled.add("run-a")
    ev_a = await register_run(pending, pre_cancelled, lock, "run-a")
    check("run-a's own pre-arrived cancel is observed", ev_a.is_set())

    ev_b = await register_run(pending, pre_cancelled, lock, "run-b")
    check("run-b (unrelated run_id) is unaffected by run-a's cancel", not ev_b.is_set())


async def main():
    await test_pre_arrived_cancel_is_observed()
    await test_normal_registration_is_unaffected()
    await test_unrelated_pre_cancelled_entries_dont_leak_across_runs()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
