"""Self-check for generic_worker.py -- the framework-agnostic Runner
Protocol loop that CrewAI/LlamaIndex/plain-LangChain adapters all sit on
top of.

Proves:
1. make_event_factory produces the same RunEvent shape (event_id/seq/
   method/namespace/data/ts) worker.py's LangGraph runner does, with
   correct per-run sequential numbering and pydantic-model serialization.
2. _poll_loop correctly drives an adapter through one job: dispatches
   the assignment, forwards events to the gRPC stream, reports the
   adapter's returned status.
3. An adapter that RAISES instead of returning a status still gets a
   terminal "error" status reported -- the loop's whole reason for
   catching adapter.execute's own exceptions (see FrameworkAdapter's doc
   comment: a buggy/incomplete adapter must never leave a run stuck
   "running" forever).

Usage:
    python/.venv/bin/python python/tests/test_generic_worker.py
"""

import asyncio
import contextlib
import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from runkite_runner import runner_pb2  # noqa: E402
from runkite_runner.generic_worker import (  # noqa: E402
    RunCancelled,
    _poll_loop,
    make_event_factory,
    register_run,
    run_cancellable,
)


def check(name, cond):
    status = "PASS" if cond else "FAIL"
    print(f"[{status}] {name}")
    if not cond:
        raise SystemExit(1)


def test_make_event_factory_shape_and_sequencing():
    make_event = make_event_factory("run-123")
    e1 = make_event("lifecycle", {"event": "running"})
    e2 = make_event("values", {"messages": ["hi"]})

    check("event_id includes run_id", e1["event_id"] == "run-123_evt_1")
    check("seq increments per event", e1["seq"] == 1 and e2["seq"] == 2)
    check("method preserved", e1["method"] == "lifecycle" and e2["method"] == "values")
    check("namespace defaults to []", e1["namespace"] == [])
    check("data preserved", e2["data"] == {"messages": ["hi"]})
    check("ts is a plausible unix-ms timestamp", e1["ts"] > 1700000000000)


def test_make_event_factory_serializes_pydantic_like_objects():
    class FakeMessage:
        def model_dump(self):
            return {"role": "ai", "content": "hello"}

    make_event = make_event_factory("run-456")
    e = make_event("values", {"messages": [FakeMessage()]})
    check(
        "pydantic-style object serialized via model_dump",
        e["data"]["messages"][0] == {"role": "ai", "content": "hello"},
    )


class _FakeCall:
    """Stands in for the object returned by stub.StreamEvents(...) --
    _poll_loop awaits it once after signaling end-of-stream."""

    def __await__(self):
        async def _noop():
            return None

        return _noop().__await__()


class _FakeStub:
    """Duck-typed replacement for runner_pb2_grpc.RunnerServiceStub --
    _poll_loop only ever calls GetJob/StreamEvents/ReportStatus on it,
    so a plain class covering those three is enough to drive one
    iteration of the loop without a real gRPC server."""

    def __init__(self, assignment: dict):
        self._assignment = assignment
        self._served = False
        self.sent_events: list[dict] = []
        self.reported_status = None
        self.reported_error = None

    async def GetJob(self, request, metadata=None):
        if self._served:
            # Second iteration: block forever (simulates a real
            # long-poll with nothing new) so the test's timeout cancels
            # the loop task cleanly instead of spinning.
            await asyncio.sleep(3600)
        self._served = True
        return runner_pb2.GetJobResponse(has_job=True, assignment_json=json.dumps(self._assignment))

    def StreamEvents(self, event_generator, metadata=None):
        async def _drain():
            async for evt in event_generator:
                self.sent_events.append(json.loads(evt.event_json))

        return asyncio.ensure_future(_drain())

    def WatchCancels(self, request, metadata=None):
        raise NotImplementedError("not exercised by this test")

    async def ReportStatus(self, request, metadata=None):
        self.reported_status = request.status
        self.reported_error = request.error_message
        return runner_pb2.ReportStatusResponse()


class _SucceedingAdapter:
    async def load_config(self, config_path):
        pass

    async def execute(self, assignment, event_callback, cancel_event):
        make_event = make_event_factory(assignment["run_id"])
        await event_callback(make_event("values", {"messages": ["done"]}))
        await event_callback(make_event("end", {"status": "success"}))
        return "success"


class _RaisingAdapter:
    async def load_config(self, config_path):
        pass

    async def execute(self, assignment, event_callback, cancel_event):
        raise RuntimeError("adapter blew up")


class _MultiJobFakeStub:
    """Like _FakeStub but serves N distinct jobs from a queue (one per
    GetJob call) instead of a single assignment repeated once then
    blocked forever -- needed to prove the dispatcher can have multiple
    jobs in flight at once, not just handle one job correctly."""

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


class _ConcurrencyTrackingAdapter:
    """Records the maximum number of overlapping execute() calls seen,
    and lets a test control exactly when each call finishes -- the
    direct evidence that the dispatcher actually runs jobs concurrently,
    not just that it eventually gets to all of them sequentially."""

    def __init__(self):
        self.current = 0
        self.max_concurrent = 0
        self.release_events: dict[str, asyncio.Event] = {}

    async def load_config(self, config_path):
        pass

    async def execute(self, assignment, event_callback, cancel_event):
        run_id = assignment["run_id"]
        self.current += 1
        self.max_concurrent = max(self.max_concurrent, self.current)
        try:
            release = self.release_events.setdefault(run_id, asyncio.Event())
            release_wait = asyncio.ensure_future(release.wait())
            cancel_wait = asyncio.ensure_future(cancel_event.wait())
            done, pending = await asyncio.wait({release_wait, cancel_wait}, return_when=asyncio.FIRST_COMPLETED)
            for t in pending:
                t.cancel()
            return "interrupted" if cancel_wait in done else "success"
        finally:
            self.current -= 1


async def _run_one_iteration(stub, adapter):
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()
    task = asyncio.ensure_future(_poll_loop(stub, adapter, "test-kind", [], pending_cancels, pre_cancelled, lock))
    # One GetJob call is enough to dispatch and complete the single
    # assignment; the fake stub then blocks forever on the next
    # GetJob, so cancel the loop task once we've seen a status report.
    for _ in range(200):
        if stub.reported_status is not None:
            break
        await asyncio.sleep(0.01)
    task.cancel()
    try:
        await task
    except asyncio.CancelledError:
        pass


async def test_poll_loop_dispatches_and_reports_success():
    assignment = {"run_id": "r1", "thread_id": "t1", "graph_id": "g1"}
    stub = _FakeStub(assignment)
    await _run_one_iteration(stub, _SucceedingAdapter())

    check("status reported as success", stub.reported_status == "success")
    check("events forwarded to the gRPC stream", any(e["method"] == "values" for e in stub.sent_events))
    check("end event forwarded", any(e["method"] == "end" for e in stub.sent_events))


async def test_poll_loop_reports_error_when_adapter_raises():
    assignment = {"run_id": "r2", "thread_id": "t1", "graph_id": "g1"}
    stub = _FakeStub(assignment)
    await _run_one_iteration(stub, _RaisingAdapter())

    check("status reported as error when adapter.execute raises", stub.reported_status == "error")
    check("a synthetic error event was still forwarded", any(e["method"] == "error" for e in stub.sent_events))


async def test_poll_loop_runs_jobs_concurrently_at_concurrency_2():
    """The direct evidence the whole runner-side concurrency initiative
    is about: with concurrency=2, two dispatched jobs must overlap in
    the adapter, not run one-after-the-other."""
    assignments = [
        {"run_id": "c1", "thread_id": "t1", "graph_id": "g1"},
        {"run_id": "c2", "thread_id": "t1", "graph_id": "g1"},
    ]
    stub = _MultiJobFakeStub(assignments)
    adapter = _ConcurrencyTrackingAdapter()
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()
    in_flight: set = set()

    task = asyncio.ensure_future(
        _poll_loop(
            stub,
            adapter,
            "test-kind",
            [],
            pending_cancels,
            pre_cancelled,
            lock,
            concurrency=2,
            in_flight=in_flight,
        )
    )

    # Wait until both jobs have actually entered execute() and are
    # blocked on their release Event -- proves they were dispatched
    # concurrently, not that the dispatcher merely queued them.
    for _ in range(500):
        if len(adapter.release_events) == 2:
            break
        await asyncio.sleep(0.01)
    check("both jobs entered execute() before either was released", len(adapter.release_events) == 2)
    check("max_concurrent reached 2", adapter.max_concurrent == 2)

    for ev in adapter.release_events.values():
        ev.set()
    for _ in range(500):
        if len(stub.reported_status) == 2:
            break
        await asyncio.sleep(0.01)
    check("both jobs eventually completed", stub.reported_status == {"c1": "success", "c2": "success"})

    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task


async def test_poll_loop_concurrency_1_is_sequential_by_default():
    """Regression guard: concurrency=1 (the default) must reproduce the
    exact original one-job-at-a-time behavior -- max_concurrent must
    never exceed 1 even though _handle_job now runs as a spawned Task."""
    assignments = [
        {"run_id": "s1", "thread_id": "t1", "graph_id": "g1"},
        {"run_id": "s2", "thread_id": "t1", "graph_id": "g1"},
    ]
    stub = _MultiJobFakeStub(assignments)
    adapter = _ConcurrencyTrackingAdapter()
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()

    task = asyncio.ensure_future(
        _poll_loop(
            stub,
            adapter,
            "test-kind",
            [],
            pending_cancels,
            pre_cancelled,
            lock,
            concurrency=1,
        )
    )

    for _ in range(500):
        if "s1" in adapter.release_events:
            break
        await asyncio.sleep(0.01)
    check("first job entered execute()", "s1" in adapter.release_events)
    await asyncio.sleep(0.05)
    check("second job has NOT started yet (concurrency=1)", "s2" not in adapter.release_events)
    check("max_concurrent never exceeded 1", adapter.max_concurrent == 1)

    adapter.release_events["s1"].set()
    for _ in range(500):
        if "s2" in adapter.release_events:
            break
        await asyncio.sleep(0.01)
    adapter.release_events["s2"].set()
    for _ in range(500):
        if len(stub.reported_status) == 2:
            break
        await asyncio.sleep(0.01)
    check("both jobs completed sequentially", stub.reported_status == {"s1": "success", "s2": "success"})

    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task


async def test_cancel_isolation_under_concurrency():
    """Cancelling one of two concurrently-running jobs must not affect
    the other -- pending_cancels is keyed by run_id, so this should
    already hold, but concurrency is exactly the scenario that would
    expose a mix-up if it didn't."""
    assignments = [
        {"run_id": "iso1", "thread_id": "t1", "graph_id": "g1"},
        {"run_id": "iso2", "thread_id": "t1", "graph_id": "g1"},
    ]
    stub = _MultiJobFakeStub(assignments)
    adapter = _ConcurrencyTrackingAdapter()
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()

    task = asyncio.ensure_future(
        _poll_loop(
            stub,
            adapter,
            "test-kind",
            [],
            pending_cancels,
            pre_cancelled,
            lock,
            concurrency=2,
        )
    )

    for _ in range(500):
        if len(adapter.release_events) == 2:
            break
        await asyncio.sleep(0.01)

    # Cancel iso1 only -- via pending_cancels directly, same as
    # watch_cancels would do on a real WatchCancels signal.
    async with lock:
        pending_cancels["iso1"].set()

    for _ in range(500):
        if len(stub.reported_status) == 2:
            break
        await asyncio.sleep(0.01)
        if "iso2" in adapter.release_events and stub.reported_status.get("iso2") is None:
            adapter.release_events["iso2"].set()

    check("cancelled run reports interrupted", stub.reported_status.get("iso1") == "interrupted")
    check("uncancelled sibling run still succeeds", stub.reported_status.get("iso2") == "success")

    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task


async def test_shutdown_drains_in_flight_jobs_independent_of_dispatcher_cancellation():
    """Regression for the graceful-shutdown design: _handle_job tasks
    are independent asyncio.Tasks, not sub-coroutines of the dispatcher
    -- cancelling the DISPATCHER (what run_worker's finally block does
    first) must NOT cancel an already-spawned job task. Draining
    `in_flight` afterward must let it finish naturally."""
    assignments = [{"run_id": "d1", "thread_id": "t1", "graph_id": "g1"}]
    stub = _MultiJobFakeStub(assignments)
    adapter = _ConcurrencyTrackingAdapter()
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()
    in_flight: set = set()

    dispatcher_task = asyncio.ensure_future(
        _poll_loop(
            stub,
            adapter,
            "test-kind",
            [],
            pending_cancels,
            pre_cancelled,
            lock,
            concurrency=2,
            in_flight=in_flight,
        )
    )

    for _ in range(500):
        if "d1" in adapter.release_events:
            break
        await asyncio.sleep(0.01)
    check("job entered execute() before shutdown", "d1" in adapter.release_events)
    check("job task is tracked in in_flight", len(in_flight) == 1)

    # Simulate run_worker's shutdown: cancel the dispatcher, THEN drain
    # in_flight -- the job's own task must survive the dispatcher's
    # cancellation and still be drainable.
    dispatcher_task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await dispatcher_task
    check(
        "job task was NOT cancelled by the dispatcher's own cancellation",
        not next(iter(in_flight)).cancelled() and not next(iter(in_flight)).done(),
    )

    adapter.release_events["d1"].set()
    await asyncio.gather(*in_flight, return_exceptions=True)
    check("drained job completed and reported status", stub.reported_status.get("d1") == "success")


async def test_stream_events_setup_failure_still_reports_status():
    """Regression: if StreamEvents raises before the stream task is
    created, _handle_job used to hit an unbound stream_task/status and
    skip ReportStatus entirely -- leaving the run stuck "running" on
    the control plane. Must still report error once run_id is known."""

    class _StreamFailsStub(_FakeStub):
        def StreamEvents(self, event_generator, metadata=None):
            raise RuntimeError("stream setup failed")

    stub = _StreamFailsStub({"run_id": "r-stream-fail", "thread_id": "t1", "graph_id": "g1"})
    pending_cancels: dict = {}
    pre_cancelled: set = set()
    lock = asyncio.Lock()
    task = asyncio.ensure_future(
        _poll_loop(stub, _SucceedingAdapter(), "test-kind", [], pending_cancels, pre_cancelled, lock)
    )
    for _ in range(200):
        if stub.reported_status is not None:
            break
        await asyncio.sleep(0.01)
    task.cancel()
    with contextlib.suppress(asyncio.CancelledError):
        await task

    check("StreamEvents setup failure still reports a status", stub.reported_status == "error")
    check("pending_cancels did not leak the failed run", "r-stream-fail" not in pending_cancels)


async def test_run_cancellable_returns_result_when_no_cancel_event():
    result = await run_cancellable(_slow_coro("done", 0.01), None)
    check("run_cancellable returns the coroutine's result when cancel_event is None", result == "done")


async def test_run_cancellable_returns_result_when_faster_than_cancel():
    ev = asyncio.Event()
    result = await run_cancellable(_slow_coro("done", 0.01), ev)
    check("run_cancellable returns the result when it finishes before cancellation", result == "done")


async def test_run_cancellable_raises_when_cancel_fires_first():
    ev = asyncio.Event()

    async def _never_finishes():
        await asyncio.sleep(3600)
        return "should never get here"

    async def _fire_cancel_soon():
        await asyncio.sleep(0.01)
        ev.set()

    fire_task = asyncio.ensure_future(_fire_cancel_soon())
    raised = False
    try:
        await run_cancellable(_never_finishes(), ev)
    except RunCancelled:
        raised = True
    await fire_task
    check("run_cancellable raises RunCancelled when cancel_event fires first", raised)


async def test_register_run_observes_pre_arrived_cancel():
    """Regression for the WatchCancels-before-register race the TS
    runner already fixed: a cancel signal that lands before GetJob
    finishes registering the run must still be observed."""
    pending: dict = {}
    pre: set = set()
    lock = asyncio.Lock()
    pre.add("early-run")
    ev = await register_run(pending, pre, lock, "early-run")
    check("pre-arrived cancel leaves Event already set", ev.is_set())
    check("pre_cancelled entry is claimed (not left dangling)", "early-run" not in pre)

    pending2: dict = {}
    pre2: set = set()
    ev2 = await register_run(pending2, pre2, lock, "normal-run")
    check("normal register returns unset Event", not ev2.is_set())


async def _slow_coro(value, delay):
    await asyncio.sleep(delay)
    return value


async def main():
    test_make_event_factory_shape_and_sequencing()
    test_make_event_factory_serializes_pydantic_like_objects()
    await test_poll_loop_dispatches_and_reports_success()
    await test_poll_loop_reports_error_when_adapter_raises()
    await test_poll_loop_runs_jobs_concurrently_at_concurrency_2()
    await test_poll_loop_concurrency_1_is_sequential_by_default()
    await test_cancel_isolation_under_concurrency()
    await test_shutdown_drains_in_flight_jobs_independent_of_dispatcher_cancellation()
    await test_stream_events_setup_failure_still_reports_status()
    await test_run_cancellable_returns_result_when_no_cancel_event()
    await test_run_cancellable_returns_result_when_faster_than_cancel()
    await test_run_cancellable_raises_when_cancel_fires_first()
    await test_register_run_observes_pre_arrived_cancel()
    print("\nAll checks passed.")


if __name__ == "__main__":
    asyncio.run(main())
