"""Self-check for heartbeat.py's heartbeat_loop: the runner-side periodic call that keeps a job's
in-flight lease alive for its WHOLE execution, not just the first event.

Proves:
1. heartbeat_loop calls stub.Heartbeat repeatedly at roughly the given
   interval, with the run_id and generation, until cancelled.
2. Cancelling the owning task stops it cleanly (no further calls, no
   unhandled exception escaping).
3. A failing Heartbeat RPC is logged and swallowed, not raised -- one bad
   call must not kill the loop or propagate into the caller's _handle_job.
4. item 16, Problem 3 (fencing): a superseded=True response sets
   cancel_event and stops the loop -- the runner's own actionable
   "you've been reclaimed, stop" signal.
5. A non-superseded response leaves cancel_event untouched and the loop
   running.

Usage:
    python/.venv/bin/python python/tests/test_heartbeat.py
"""

from __future__ import annotations

import asyncio
import contextlib
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import grpc  # noqa: E402
from runkite_runner.heartbeat import heartbeat_loop  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


class _FakeHeartbeatResponse:
    def __init__(self, superseded: bool = False):
        self.ok = True
        self.superseded = superseded


class _CountingStub:
    """Records every call's run_id AND generation -- the generation
    check matters here specifically because a stub that ignored it
    would pass tests that only assert on run_id."""

    def __init__(self):
        self.calls: list[str] = []
        self.generations: list[int] = []

    async def Heartbeat(self, request, metadata=None):
        self.calls.append(request.run_id)
        self.generations.append(request.generation)
        return _FakeHeartbeatResponse()


class _FailingThenCountingStub:
    """Fails the first call, succeeds on the rest -- proves one bad RPC
    doesn't kill the loop."""

    def __init__(self):
        self.calls = 0

    async def Heartbeat(self, request, metadata=None):
        self.calls += 1
        if self.calls == 1:
            raise grpc.aio.AioRpcError(
                grpc.StatusCode.UNAVAILABLE,
                grpc.aio.Metadata(),
                grpc.aio.Metadata(),
                details="simulated transient failure",
            )
        return _FakeHeartbeatResponse()


class _SupersededAfterNStub:
    """Returns a normal response for the first n-1 calls, then
    superseded=True from call n onward -- simulates this runner being
    reclaimed partway through its own execution."""

    def __init__(self, supersede_at_call: int):
        self.supersede_at_call = supersede_at_call
        self.calls = 0

    async def Heartbeat(self, request, metadata=None):
        self.calls += 1
        return _FakeHeartbeatResponse(superseded=self.calls >= self.supersede_at_call)


async def test_heartbeat_loop_calls_repeatedly_with_run_id():
    stub = _CountingStub()
    task = asyncio.create_task(heartbeat_loop(stub, "run-x", [], interval_s=0.05))
    await asyncio.sleep(0.23)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

    check("at least 3 heartbeats sent in ~0.23s at 0.05s interval", len(stub.calls) >= 3)
    check("every heartbeat carried the correct run_id", all(c == "run-x" for c in stub.calls))


async def test_heartbeat_loop_stops_cleanly_on_cancel():
    stub = _CountingStub()
    task = asyncio.create_task(heartbeat_loop(stub, "run-y", [], interval_s=0.05))
    await asyncio.sleep(0.12)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task
    count_at_cancel = len(stub.calls)

    # No further calls should land after cancellation, however long we wait.
    await asyncio.sleep(0.2)
    check("no heartbeats sent after the task was cancelled", len(stub.calls) == count_at_cancel)
    check("task actually finished (not left running)", task.done())


async def test_heartbeat_loop_survives_a_failed_rpc():
    stub = _FailingThenCountingStub()
    task = asyncio.create_task(heartbeat_loop(stub, "run-z", [], interval_s=0.05))
    await asyncio.sleep(0.23)
    task.cancel()
    # If the failed RPC had propagated out of the loop instead of being
    # logged and swallowed, this await would raise something other than
    # CancelledError -- reaching the checks below at all is part of the proof.
    with contextlib.suppress(asyncio.CancelledError):
        await task

    check("loop kept running after the first RPC failed", stub.calls >= 3)


async def test_heartbeat_loop_sends_the_given_generation():
    stub = _CountingStub()
    task = asyncio.create_task(heartbeat_loop(stub, "run-gen", [], generation=7, interval_s=0.05))
    await asyncio.sleep(0.12)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

    check("at least 2 heartbeats sent", len(stub.generations) >= 2)
    check("every heartbeat carried generation=7", all(g == 7 for g in stub.generations))


async def test_heartbeat_loop_defaults_generation_to_zero():
    stub = _CountingStub()
    task = asyncio.create_task(heartbeat_loop(stub, "run-default-gen", [], interval_s=0.05))
    await asyncio.sleep(0.12)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

    check("unspecified generation defaults to 0 (unfenced)", all(g == 0 for g in stub.generations))


async def test_heartbeat_loop_stops_and_sets_cancel_event_on_superseded():
    stub = _SupersededAfterNStub(supersede_at_call=2)
    cancel_event = asyncio.Event()
    task = asyncio.create_task(
        heartbeat_loop(stub, "run-superseded", [], generation=1, cancel_event=cancel_event, interval_s=0.05)
    )
    # 2nd call (superseded=True) lands at ~0.1s; give it real margin.
    await asyncio.sleep(0.3)

    check("loop stopped itself (not left running) once superseded", task.done())
    check("cancel_event was set", cancel_event.is_set())
    calls_at_stop = stub.calls
    await asyncio.sleep(0.15)
    check("no further heartbeat calls after superseded", stub.calls == calls_at_stop)


async def test_heartbeat_loop_does_not_touch_cancel_event_when_not_superseded():
    stub = _CountingStub()
    cancel_event = asyncio.Event()
    task = asyncio.create_task(
        heartbeat_loop(stub, "run-not-superseded", [], generation=1, cancel_event=cancel_event, interval_s=0.05)
    )
    await asyncio.sleep(0.12)
    check("cancel_event untouched while not superseded", not cancel_event.is_set())
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task


async def test_heartbeat_loop_superseded_without_cancel_event_does_not_raise():
    """cancel_event is optional (default None) -- a superseded response
    must still stop the loop cleanly, not raise, when no cancel_event
    was given."""
    stub = _SupersededAfterNStub(supersede_at_call=1)
    task = asyncio.create_task(heartbeat_loop(stub, "run-no-cancel-event", [], generation=1, interval_s=0.05))
    await asyncio.sleep(0.2)
    check("loop stopped cleanly with no cancel_event given", task.done())
    check("task raised nothing", task.exception() is None)


async def main():
    await test_heartbeat_loop_calls_repeatedly_with_run_id()
    await test_heartbeat_loop_stops_cleanly_on_cancel()
    await test_heartbeat_loop_survives_a_failed_rpc()
    await test_heartbeat_loop_sends_the_given_generation()
    await test_heartbeat_loop_defaults_generation_to_zero()
    await test_heartbeat_loop_stops_and_sets_cancel_event_on_superseded()
    await test_heartbeat_loop_does_not_touch_cancel_event_when_not_superseded()
    await test_heartbeat_loop_superseded_without_cancel_event_does_not_raise()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
