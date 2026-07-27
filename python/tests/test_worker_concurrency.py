"""Self-check for worker.py's (the production LangGraph runner) runner-side
concurrency: the semaphore-bounded dispatcher (_poll_loop) that replaced the
original strictly-sequential "GetJob -> await execute_run -> ReportStatus"
loop.

Proves:
1. With concurrency=2, two dispatched jobs actually overlap inside
   graph.astream() -- not just eventually both complete.
2. concurrency=1 (the default) reproduces the exact original
   one-job-at-a-time behavior -- a pure regression guard.
3. Cancelling one of two concurrently-running jobs doesn't affect the
   other (pending_cancels is keyed by run_id, but concurrency is exactly
   the scenario that would expose a mix-up if isolation were broken).
4. Graceful shutdown: an in-flight job's task is independent of the
   dispatcher's own task and survives the dispatcher being cancelled,
   so run_worker's finally block can drain it before tearing down the
   checkpointer/store connections that job still needs.

Usage:
    python/.venv/bin/python python/tests/test_worker_concurrency.py
"""

import asyncio
import contextlib
import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner import runner_pb2  # noqa: E402
from runkite_runner.worker import _poll_loop  # noqa: E402


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


class _MultiJobFakeStub:
    """Serves N distinct jobs from a queue (one per GetJob call), unlike
    a single-job stub that only ever dispatches once."""

    def __init__(self, assignments: list[dict]):
        self._queue = list(assignments)
        self.sent_events: dict[str, list[dict]] = {}
        self.reported_status: dict[str, str] = {}

    async def GetJob(self, request, metadata=None):
        if not self._queue:
            await asyncio.sleep(3600)
        assignment = self._queue.pop(0)
        return runner_pb2.GetJobResponse(has_job=True, assignment_json=json.dumps(assignment))

    def StreamEvents(self, event_generator, metadata=None):
        async def _drain():
            async for evt in event_generator:
                self.sent_events.setdefault(evt.run_id, []).append(json.loads(evt.event_json))
        return asyncio.ensure_future(_drain())

    def WatchCancels(self, request, metadata=None):
        raise NotImplementedError("not exercised by this test")

    async def ReportStatus(self, request, metadata=None):
        self.reported_status[request.run_id] = request.status
        return runner_pb2.ReportStatusResponse()


class _ConcurrencyTrackingController:
    """Owns the shared state _FakeGraph instances report into, and the
    fake adapter that hands out one _FakeGraph per run_id."""

    def __init__(self):
        self.current = 0
        self.max_concurrent = 0
        self.release_events: dict[str, asyncio.Event] = {}

    def is_factory(self, graph_id: str) -> bool:
        return False

    def get_graph(self, graph_id: str):
        # execute_run calls get_graph(graph_id) once per run, but doesn't
        # pass run_id -- return a graph bound to whichever run_id is
        # currently being dispatched via a small trick: a graph object
        # that reads run_id off its own astream() call's config instead.
        return _SharedFakeGraph(self)


class _SharedFakeGraph:
    """One shared graph object (matches real LangGraph: a single
    compiled graph instance is reused across concurrent runs), keyed by
    the run_id each astream() call carries in its own config."""

    def __init__(self, controller: _ConcurrencyTrackingController):
        self._controller = controller

    async def astream(self, input_data, config=None, stream_mode=None):
        # execute_run only checks cancel_event AFTER receiving a chunk
        # (between graph steps), never while blocked waiting for one --
        # matches how a real LangGraph astream() call behaves (there's
        # no preemption mid-step). So this yields an immediate first
        # chunk (letting execute_run's cancellation check run at all),
        # then blocks on `release` for a second one -- the same point a
        # concurrency test can observe as "this run has started."
        run_id = config["configurable"]["run_id"]
        c = self._controller
        c.current += 1
        c.max_concurrent = max(c.max_concurrent, c.current)
        try:
            yield {"messages": ["step1"]}
            release = c.release_events.setdefault(run_id, asyncio.Event())
            await release.wait()
            yield {"messages": ["done"]}
        finally:
            c.current -= 1


async def _wait_until(predicate, timeout_s=5.0):
    steps = int(timeout_s / 0.01)
    for _ in range(steps):
        if predicate():
            return True
        await asyncio.sleep(0.01)
    return predicate()


async def test_poll_loop_runs_jobs_concurrently_at_concurrency_2():
    assignments = [
        {"run_id": "c1", "thread_id": "t1", "graph_id": "g1"},
        {"run_id": "c2", "thread_id": "t1", "graph_id": "g1"},
    ]
    stub = _MultiJobFakeStub(assignments)
    controller = _ConcurrencyTrackingController()
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()
    in_flight: set = set()

    task = asyncio.ensure_future(_poll_loop(
        stub, controller, "test-kind", [], pending_cancels, pre_cancelled, lock,
        concurrency=2, in_flight=in_flight,
    ))

    ok = await _wait_until(lambda: len(controller.release_events) == 2)
    check("both jobs entered astream() before either was released", ok)
    check("max_concurrent reached 2", controller.max_concurrent == 2)

    for ev in controller.release_events.values():
        ev.set()
    ok = await _wait_until(lambda: len(stub.reported_status) == 2)
    check("both jobs eventually completed", ok and stub.reported_status == {"c1": "success", "c2": "success"})

    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task


async def test_poll_loop_concurrency_1_is_sequential_by_default():
    assignments = [
        {"run_id": "s1", "thread_id": "t1", "graph_id": "g1"},
        {"run_id": "s2", "thread_id": "t1", "graph_id": "g1"},
    ]
    stub = _MultiJobFakeStub(assignments)
    controller = _ConcurrencyTrackingController()
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()

    task = asyncio.ensure_future(_poll_loop(
        stub, controller, "test-kind", [], pending_cancels, pre_cancelled, lock,
        concurrency=1,
    ))

    ok = await _wait_until(lambda: "s1" in controller.release_events)
    check("first job entered astream()", ok)
    await asyncio.sleep(0.05)
    check("second job has NOT started yet (concurrency=1)", "s2" not in controller.release_events)
    check("max_concurrent never exceeded 1", controller.max_concurrent == 1)

    controller.release_events["s1"].set()
    ok = await _wait_until(lambda: "s2" in controller.release_events)
    check("second job started only after the first finished", ok)
    controller.release_events["s2"].set()
    ok = await _wait_until(lambda: len(stub.reported_status) == 2)
    check("both jobs completed sequentially", ok and stub.reported_status == {"s1": "success", "s2": "success"})

    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task


async def test_cancel_isolation_under_concurrency():
    assignments = [
        {"run_id": "iso1", "thread_id": "t1", "graph_id": "g1"},
        {"run_id": "iso2", "thread_id": "t1", "graph_id": "g1"},
    ]
    stub = _MultiJobFakeStub(assignments)
    controller = _ConcurrencyTrackingController()
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()

    task = asyncio.ensure_future(_poll_loop(
        stub, controller, "test-kind", [], pending_cancels, pre_cancelled, lock,
        concurrency=2,
    ))

    ok = await _wait_until(lambda: len(controller.release_events) == 2)
    check("both jobs entered astream()", ok)

    # Cancel iso1 only -- same as watch_cancels would do on a real
    # WatchCancels signal (set the run_id's Event directly). Cancellation
    # is only checked BETWEEN graph steps (see _SharedFakeGraph.astream's
    # comment), so also release iso1 to let its graph yield its next
    # (and, since cancelled, last) chunk -- simulates the graph
    # naturally progressing to its next step after the cancel signal
    # arrived, same as a real LangGraph run would.
    async with lock:
        pending_cancels["iso1"].set()
    controller.release_events["iso1"].set()

    ok2 = await _wait_until(lambda: "iso2" in controller.release_events and stub.reported_status.get("iso2") is None, timeout_s=3.0)
    if ok2:
        controller.release_events["iso2"].set()
    await _wait_until(lambda: len(stub.reported_status) == 2, timeout_s=4.0)

    check("cancelled run reports interrupted", stub.reported_status.get("iso1") == "interrupted")
    check("uncancelled sibling run still succeeds", stub.reported_status.get("iso2") == "success")

    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task


async def test_shutdown_drains_in_flight_jobs_independent_of_dispatcher_cancellation():
    assignments = [{"run_id": "d1", "thread_id": "t1", "graph_id": "g1"}]
    stub = _MultiJobFakeStub(assignments)
    controller = _ConcurrencyTrackingController()
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()
    in_flight: set = set()

    dispatcher_task = asyncio.ensure_future(_poll_loop(
        stub, controller, "test-kind", [], pending_cancels, pre_cancelled, lock,
        concurrency=2, in_flight=in_flight,
    ))

    ok = await _wait_until(lambda: "d1" in controller.release_events)
    check("job entered astream() before shutdown", ok)
    check("job task is tracked in in_flight", len(in_flight) == 1)

    dispatcher_task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await dispatcher_task
    job_task = next(iter(in_flight))
    check("job task was NOT cancelled by the dispatcher's own cancellation", not job_task.cancelled() and not job_task.done())

    controller.release_events["d1"].set()
    await asyncio.gather(*in_flight, return_exceptions=True)
    check("drained job completed and reported status", stub.reported_status.get("d1") == "success")


async def main():
    await test_poll_loop_runs_jobs_concurrently_at_concurrency_2()
    await test_poll_loop_concurrency_1_is_sequential_by_default()
    await test_cancel_isolation_under_concurrency()
    await test_shutdown_drains_in_flight_jobs_independent_of_dispatcher_cancellation()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
