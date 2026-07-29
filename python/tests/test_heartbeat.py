"""Self-check for heartbeat.py's heartbeat_loop (plans/pending_items.md
item 16, Problem 2): the runner-side periodic call that keeps a job's
in-flight lease alive for its WHOLE execution, not just the first event.

Proves:
1. heartbeat_loop calls stub.Heartbeat repeatedly at roughly the given
   interval, with the run_id, until cancelled.
2. Cancelling the owning task stops it cleanly (no further calls, no
   unhandled exception escaping).
3. A failing Heartbeat RPC is logged and swallowed, not raised -- one bad
   call must not kill the loop or propagate into the caller's _handle_job.

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


class _CountingStub:
    def __init__(self):
        self.calls: list[str] = []

    async def Heartbeat(self, request, metadata=None):
        self.calls.append(request.run_id)


class _FailingThenCountingStub:
    """Fails the first call, succeeds on the rest -- proves one bad RPC
    doesn't kill the loop."""

    def __init__(self):
        self.calls = 0

    async def Heartbeat(self, request, metadata=None):
        self.calls += 1
        if self.calls == 1:
            raise grpc.aio.AioRpcError(
                grpc.StatusCode.UNAVAILABLE, grpc.aio.Metadata(), grpc.aio.Metadata(),
                details="simulated transient failure",
            )


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


async def main():
    await test_heartbeat_loop_calls_repeatedly_with_run_id()
    await test_heartbeat_loop_stops_cleanly_on_cancel()
    await test_heartbeat_loop_survives_a_failed_rpc()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
