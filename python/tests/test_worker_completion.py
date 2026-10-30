"""Distinguish agent execution from event-transport completion failures."""

import asyncio
import json
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from runkite_runner import cancellation, generic_worker, runner_pb2, worker
from runkite_runner.heartbeat import heartbeat_loop


class _CancelClock:
    """Invoke canceled callbacks too, covering an already-queued timer race."""

    def __init__(self):
        self.now = 0
        self.timers = []

    def call_later(self, delay, callback):
        timer = SimpleNamespace(at=self.now + delay, callback=callback, cancelled=False)

        def cancel():
            timer.cancelled = True

        timer.cancel = cancel
        self.timers.append(timer)
        return timer

    def advance(self, seconds):
        self.now += seconds
        for timer in list(self.timers):
            if timer.at <= self.now:
                self.timers.remove(timer)
                timer.callback()


class WorkerCompletionTests(unittest.IsolatedAsyncioTestCase):
    async def exercise(self, module, *, construction_fails):
        pending_cancels = {}
        seen_reports = []
        sent_events = []
        executions = []
        error = RuntimeError("original stream construction failure" if construction_fails else "stream close failure")

        class Stub:
            def StreamEvents(self, events, metadata):
                if construction_fails:
                    raise error

                async def drain():
                    async for event in events:
                        sent_events.append(json.loads(event.event_json))
                    raise error

                return drain()

            async def ReportStatus(self, request, metadata):
                seen_reports.append(request)
                return runner_pb2.ReportStatusResponse(ok=True)

        async def execute(adapter, assignment, send_event, cancel_event=None):
            executions.append(assignment["run_id"])
            await send_event({"method": "values", "data": {"answer": "agent succeeded"}})
            await send_event({"method": "end", "data": {"status": "success"}})
            return "success"

        class Adapter:
            checkpoint_framework = ""

            async def execute(self, assignment, send_event, cancel_event):
                return await execute(self, assignment, send_event, cancel_event)

        async def heartbeat(*args, **kwargs):
            await asyncio.Event().wait()

        response = SimpleNamespace(
            assignment_json=json.dumps({"run_id": "run", "thread_id": "thread", "graph_id": "graph"})
        )
        before = set(asyncio.all_tasks())
        with (
            patch.object(module, "should_skip_run", AsyncMock(return_value=False)),
            patch.object(module, "heartbeat_loop", heartbeat),
        ):
            if module is worker:
                with patch.object(module, "execute_run", execute):
                    await module._handle_job(Stub(), Adapter(), response, [], pending_cancels, set(), asyncio.Lock())
            else:
                await module._handle_job(Stub(), Adapter(), response, [], pending_cancels, set(), asyncio.Lock())
        await asyncio.sleep(0)
        self.assertEqual(pending_cancels, {})
        self.assertEqual(set(asyncio.all_tasks()) - before, set(), "Worker leaked a stream or heartbeat task")
        if construction_fails:
            self.assertEqual(executions, [])
            self.assertEqual(len(seen_reports), 1)
            self.assertEqual(seen_reports[0].status, "error")
            self.assertEqual(seen_reports[0].error_message, str(error))
        else:
            self.assertEqual(executions, ["run"])
            self.assertEqual(sent_events[-1]["data"]["status"], "success")
            self.assertEqual(seen_reports, [], "Transport failure must not overwrite successful agent outcome")

    async def test_langgraph_construction_failure_retains_original_error_and_cleans_up(self):
        await self.exercise(worker, construction_fails=True)

    async def test_adapter_construction_failure_retains_original_error_and_cleans_up(self):
        await self.exercise(generic_worker, construction_fails=True)

    async def test_successful_langgraph_with_failed_stream_close_is_recoverable(self):
        await self.exercise(worker, construction_fails=False)

    async def test_successful_adapter_with_failed_stream_close_is_recoverable(self):
        await self.exercise(generic_worker, construction_fails=False)

    async def exercise_opaque_failure(self, fail_at, outcome):
        reports = []
        events = []
        executions = []
        pending = {}

        class Client:
            closed = False

            def __init__(self, **_kwargs):
                pass

            async def get(self, _thread):
                if fail_at == "load":
                    raise OSError("state load unavailable")
                return None

            async def put(self, _thread, _blob):
                if fail_at == "save":
                    raise OSError("state write unavailable")

            async def aclose(self):
                Client.closed = True

        class Adapter:
            checkpoint_framework = "test"

            def prepare_checkpoint_input(self, _prior, input_data):
                return input_data

            def serialize_checkpoint(self, _assignment, _values, _status):
                if fail_at == "serialize":
                    raise ValueError("cannot encode state")
                if fail_at == "empty":
                    return None
                return b"durable state"

            async def execute(self, _assignment, send, _cancel):
                executions.append(True)
                await send({"method": "values", "data": {"messages": ["executed"]}})
                await send({"method": "end", "data": {"status": outcome}})
                return outcome

        class Stub:
            def StreamEvents(self, source, metadata):
                async def drain():
                    async for event in source:
                        events.append(json.loads(event.event_json))
                    return runner_pb2.StreamEventsResponse(ok=True)

                return drain()

            async def ReportStatus(self, request, metadata):
                reports.append(request)
                return runner_pb2.ReportStatusResponse(ok=True)

        async def heartbeat(*args, **kwargs):
            await asyncio.Event().wait()

        response = SimpleNamespace(
            assignment_json=json.dumps({"run_id": "run", "thread_id": "thread", "graph_id": "graph", "generation": 9})
        )
        before = set(asyncio.all_tasks())
        with (
            patch.object(generic_worker, "should_skip_run", AsyncMock(return_value=False)),
            patch.object(generic_worker, "heartbeat_loop", heartbeat),
            patch("runkite_runner.checkpoint.resolve_checkpoint_http_url", return_value="http://plane"),
            patch("runkite_runner.opaque_checkpoint.OpaqueCheckpointClient", Client),
        ):
            await generic_worker._handle_job(Stub(), Adapter(), response, [], pending, set(), asyncio.Lock())
        await asyncio.sleep(0)
        self.assertEqual(reports, [], "A failed state commit must leave the queue assignment unacknowledged")
        self.assertTrue(all(event["method"] == "values" for event in events))
        self.assertEqual(executions, [] if fail_at == "load" else [True])
        self.assertTrue(Client.closed)
        self.assertEqual(pending, {})
        self.assertEqual(set(asyncio.all_tasks()) - before, set(), "Worker leaked a stream or heartbeat task")

    async def test_opaque_state_failure_never_reports_or_publishes_completion(self):
        for fail_at in ("load", "serialize", "empty", "save"):
            for outcome in ("success", "interrupted"):
                with self.subTest(fail_at=fail_at, outcome=outcome):
                    await self.exercise_opaque_failure(fail_at, outcome)

    async def exercise_attempt_supersession(self, module, *, cancel_current):
        pending = {}
        pre_cancelled = set()
        lock = asyncio.Lock()
        started = {generation: asyncio.Event() for generation in (1, 2)}
        release = {generation: asyncio.Event() for generation in (1, 2)}
        events = {}
        reports = {}
        supersede_old = asyncio.Event()
        keep_current_alive = asyncio.Event()
        clock = _CancelClock()

        class Stub:
            def StreamEvents(self, source, metadata):
                async def drain():
                    async for _event in source:
                        pass
                    return runner_pb2.StreamEventsResponse(ok=True)

                return drain()

            async def Heartbeat(self, request, metadata):
                if request.generation == 1:
                    await supersede_old.wait()
                    return runner_pb2.HeartbeatResponse(superseded=True)
                await keep_current_alive.wait()
                return runner_pb2.HeartbeatResponse(ok=True)

            async def ReportStatus(self, request, metadata):
                reports[request.generation] = request.status
                return runner_pb2.ReportStatusResponse(ok=True)

        async def execute(adapter, assignment, send_event, cancel_event=None):
            generation = assignment["generation"]
            events[generation] = cancel_event
            started[generation].set()
            await release[generation].wait()
            outcome = "interrupted" if cancel_event.is_set() else "success"
            await send_event({"method": "end", "data": {"status": outcome}})
            return outcome

        class Adapter:
            checkpoint_framework = ""

            async def execute(self, assignment, send_event, cancel_event):
                return await execute(self, assignment, send_event, cancel_event)

        async def heartbeat(*args, **kwargs):
            await heartbeat_loop(*args, **kwargs, interval_s=0)

        def response(generation):
            return SimpleNamespace(
                assignment_json=json.dumps(
                    {"run_id": "run", "thread_id": "thread", "graph_id": "graph", "generation": generation}
                )
            )

        tasks = []
        with (
            patch.object(module, "should_skip_run", AsyncMock(return_value=False)),
            patch.object(module, "heartbeat_loop", heartbeat),
            patch.object(worker, "execute_run", execute),
            patch.object(cancellation, "get_running_loop", return_value=clock),
        ):
            try:
                for generation in (1, 2):
                    task = asyncio.create_task(
                        module._handle_job(Stub(), Adapter(), response(generation), [], pending, pre_cancelled, lock)
                    )
                    tasks.append(task)
                    await asyncio.wait_for(started[generation].wait(), 2)
                self.assertIsNot(events[1], events[2], "Overlapping attempts shared their cancellation Event")
                supersede_old.set()
                await asyncio.wait_for(events[1].wait(), 2)
                self.assertFalse(events[2].is_set(), "A stale heartbeat cancelled the replacement attempt")
                release[1].set()
                await asyncio.wait_for(tasks[0], 2)
                self.assertEqual(pending["run"], {events[2]}, "Old cleanup removed the replacement registration")
                if cancel_current:
                    await module.record_cancel_signal(pending, pre_cancelled, lock, "run")
                    self.assertTrue(events[2].is_set(), "The replacement missed a genuine run-wide cancel")
                release[2].set()
                await asyncio.wait_for(tasks[1], 2)
                self.assertEqual(reports, {1: "interrupted", 2: "interrupted" if cancel_current else "success"})
                clock.advance(cancellation.CANCEL_SIGNAL_TTL_S)
                self.assertEqual(pending, {})
            finally:
                for task in tasks:
                    task.cancel()
                await asyncio.gather(*tasks, return_exceptions=True)

    async def test_langgraph_superseded_attempt_does_not_cancel_or_unregister_its_replacement(self):
        for cancel_current in (False, True):
            with self.subTest(cancel_current=cancel_current):
                await self.exercise_attempt_supersession(worker, cancel_current=cancel_current)

    async def test_generic_superseded_attempt_does_not_cancel_or_unregister_its_replacement(self):
        for cancel_current in (False, True):
            with self.subTest(cancel_current=cancel_current):
                await self.exercise_attempt_supersession(generic_worker, cancel_current=cancel_current)

    async def test_real_cancellation_reaches_all_attempts_and_racing_registrations(self):
        for module in (worker, generic_worker):
            clock = _CancelClock()
            with (
                self.subTest(worker=module.__name__),
                patch.object(cancellation, "get_running_loop", return_value=clock),
            ):
                pending = {}
                pre_cancelled = set()
                lock = asyncio.Lock()
                first = await module.register_run(pending, pre_cancelled, lock, "run")
                second = await module.register_run(pending, pre_cancelled, lock, "run")
                self.assertTrue(await module.record_cancel_signal(pending, pre_cancelled, lock, "run"))
                self.assertTrue(first.is_set())
                self.assertTrue(second.is_set())
                await module.unregister_run(pending, lock, "run", first)
                third = await module.register_run(pending, pre_cancelled, lock, "run")
                self.assertTrue(third.is_set(), "A replacement consumed or missed a genuine cancellation")
                self.assertFalse(await module.record_cancel_signal(pending, pre_cancelled, lock, "run"))
                await module.unregister_run(pending, lock, "run", first)
                self.assertEqual(pending["run"], {second, third}, "Duplicate cleanup removed another attempt")
                await module.unregister_run(pending, lock, "run", second)
                await module.unregister_run(pending, lock, "run", third)
                self.assertEqual(len(pending["run"]), 0, "Only the bounded canceled tombstone should remain")
                after_cleanup = await module.register_run(pending, pre_cancelled, lock, "run")
                self.assertTrue(after_cleanup.is_set(), "A cancel was lost between cleanup and registration")
                await module.unregister_run(pending, lock, "run", after_cleanup)
                clock.advance(cancellation.CANCEL_SIGNAL_TTL_S)
                self.assertEqual(pending, {})
                self.assertEqual(pre_cancelled, set())

    async def test_early_cancellation_survives_registration_of_multiple_attempts(self):
        for module in (worker, generic_worker):
            clock = _CancelClock()
            with (
                self.subTest(worker=module.__name__),
                patch.object(cancellation, "get_running_loop", return_value=clock),
            ):
                pending = {}
                pre_cancelled = set()
                lock = asyncio.Lock()
                await module.record_cancel_signal(pending, pre_cancelled, lock, "run")
                first = await module.register_run(pending, pre_cancelled, lock, "run")
                second = await module.register_run(pending, pre_cancelled, lock, "run")
                self.assertIsNot(first, second)
                self.assertTrue(first.is_set())
                self.assertTrue(second.is_set())
                unrelated = await module.register_run(pending, pre_cancelled, lock, "other")
                self.assertFalse(unrelated.is_set())

    async def test_cancellation_stays_latched_past_orphan_expiry_while_attempts_are_active(self):
        for module in (worker, generic_worker):
            clock = _CancelClock()
            with (
                self.subTest(worker=module.__name__),
                patch.object(cancellation, "get_running_loop", return_value=clock),
            ):
                pending = {}
                pre_cancelled = set()
                lock = asyncio.Lock()
                await module.record_cancel_signal(pending, pre_cancelled, lock, "run")
                first = await module.register_run(pending, pre_cancelled, lock, "run")
                clock.advance(cancellation.CANCEL_SIGNAL_TTL_S * 2)
                self.assertTrue(first.is_set())
                self.assertIn("run", pending, "An old expiry callback removed a registered attempt")
                # The active bucket's run-wide latch must not depend on the
                # orphan marker surviving for the whole execution.
                pre_cancelled.discard("run")
                second = await module.register_run(pending, pre_cancelled, lock, "run")
                self.assertTrue(second.is_set(), "The replacement forgot a genuine run-wide cancellation")
                await module.unregister_run(pending, lock, "run", first)
                await module.unregister_run(pending, lock, "run", second)
                clock.advance(cancellation.CANCEL_SIGNAL_TTL_S)
                self.assertEqual(pending, {})

    async def test_repeated_cancel_refreshes_orphan_expiry_without_an_old_timer_clearing_it(self):
        for module in (worker, generic_worker):
            clock = _CancelClock()
            with (
                self.subTest(worker=module.__name__),
                patch.object(cancellation, "get_running_loop", return_value=clock),
            ):
                pending = {}
                pre_cancelled = set()
                lock = asyncio.Lock()
                await module.record_cancel_signal(pending, pre_cancelled, lock, "run")
                clock.advance(cancellation.CANCEL_SIGNAL_TTL_S - 1)
                await module.record_cancel_signal(pending, pre_cancelled, lock, "run")
                clock.advance(1)
                self.assertIn("run", pending, "The older expiry deleted a refreshed canceled bucket")
                self.assertIn("run", pre_cancelled, "The older expiry cleared the newer cancel marker")
                clock.advance(cancellation.CANCEL_SIGNAL_TTL_S - 1)
                self.assertEqual(pending, {})
                self.assertEqual(pre_cancelled, set())

    async def test_old_orphan_timer_does_not_clear_a_new_bucket_for_the_same_run(self):
        clock = _CancelClock()
        with patch.object(cancellation, "get_running_loop", return_value=clock):
            pending = {}
            pre_cancelled = set()
            lock = asyncio.Lock()
            await cancellation.record_cancel_signal(pending, pre_cancelled, lock, "run")
            old_bucket = pending.pop("run")
            pre_cancelled.discard("run")
            clock.advance(1)
            await cancellation.record_cancel_signal(pending, pre_cancelled, lock, "run")
            new_bucket = pending["run"]
            clock.advance(cancellation.CANCEL_SIGNAL_TTL_S - 1)
            self.assertIsNot(old_bucket, new_bucket)
            self.assertIs(pending["run"], new_bucket)
            self.assertIn("run", pre_cancelled)
            clock.advance(1)
            self.assertEqual(pending, {})
            self.assertEqual(pre_cancelled, set())


if __name__ == "__main__":
    unittest.main()
