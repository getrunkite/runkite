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


if __name__ == "__main__":
    unittest.main()
