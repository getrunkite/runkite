"""Distinguish agent execution from event-transport completion failures."""

import asyncio
import json
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

from runkite_runner import generic_worker, runner_pb2, worker


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


if __name__ == "__main__":
    unittest.main()
