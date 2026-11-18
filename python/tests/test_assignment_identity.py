"""Original-record proof is mandatory before execution and stays attempt-local."""

import asyncio
import json
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, MagicMock, patch

from runkite_runner import generic_worker, runner_pb2, worker
from runkite_runner.assignment_identity import AssignmentIdentityError, require_run_incarnation
from runkite_runner.cancellation import record_cancel_signal, register_run, unregister_run
from runkite_runner.completion import finish_run
from runkite_runner.heartbeat import heartbeat_loop
from runkite_runner.run_status import should_skip_run
from runkite_runner.tenant_ctx import bind_run, reset_run, tenant_headers


class AssignmentIdentityTests(unittest.IsolatedAsyncioTestCase):
    def test_exact_bounded_identity_validation(self):
        for value in ("A", "mongo-objectid:" + "a" * 24, "x" * 256, "!~"):
            self.assertEqual(require_run_incarnation(value), value)
        for value in (None, "", " ", " x", "x ", "x\ny", "x\x00y", "é", "x" * 257, 1, True):
            with self.subTest(value_type=type(value).__name__), self.assertRaises(AssignmentIdentityError):
                require_run_incarnation(value)

    async def test_same_public_id_keeps_concurrent_http_bindings_distinct(self):
        ready = asyncio.Event()
        seen = []

        async def job(proof):
            token = bind_run("same", 1, proof)
            try:
                ready.set()
                await asyncio.sleep(0)
                seen.append(tenant_headers()["X-Runkite-Run-Incarnation"])
            finally:
                reset_run(token)

        first = asyncio.create_task(job("record-a"))
        await ready.wait()
        await asyncio.gather(first, job("record-b"))
        self.assertCountEqual(seen, ["record-a", "record-b"])
        self.assertNotIn("X-Runkite-Run-Incarnation", tenant_headers())

    async def test_both_pollers_negotiate_identity_before_dequeue(self):
        for module in (worker, generic_worker):
            seen = []

            async def get_job(request, metadata, seen=seen):
                seen.append(request)
                raise asyncio.CancelledError()

            with self.subTest(worker=module.__name__), self.assertRaises(asyncio.CancelledError):
                await module._poll_loop(
                    SimpleNamespace(GetJob=get_job),
                    None,
                    "python-fixture",
                    [],
                    {},
                    set(),
                    asyncio.Lock(),
                )
            self.assertEqual(len(seen), 1)
            self.assertEqual(seen[0].assignment_identity_version, 1)

    async def test_invalid_assignment_never_reaches_status_stream_or_adapter(self):
        for module in (worker, generic_worker):
            for proof in (None, "", "bad proof", "é", "x" * 257):
                adapter = SimpleNamespace(execute=AsyncMock())
                stub = SimpleNamespace(StreamEvents=MagicMock(), ReportStatus=AsyncMock(), Heartbeat=AsyncMock())
                assignment = {"run_id": "same", "thread_id": "thread", "graph_id": "agent", "generation": 1}
                if proof is not None:
                    assignment["run_incarnation_id"] = proof
                with (
                    self.subTest(worker=module.__name__, proof_type=type(proof).__name__),
                    patch.object(module, "should_skip_run", new=AsyncMock(return_value=False)) as status,
                    patch.object(module.logger, "exception"),
                ):
                    await module._handle_job(
                        stub,
                        adapter,
                        SimpleNamespace(assignment_json=json.dumps(assignment)),
                        [],
                        {},
                        set(),
                        asyncio.Lock(),
                    )
                status.assert_not_awaited()
                adapter.execute.assert_not_awaited()
                stub.StreamEvents.assert_not_called()
                stub.ReportStatus.assert_not_awaited()
                stub.Heartbeat.assert_not_awaited()

    async def test_preexecution_proof_and_auth_denials_never_fail_open(self):
        for code, skip in ((200, False), (401, True), (403, True)):
            response = MagicMock(status_code=code)
            response.json.return_value = {"status": "running"}
            client = MagicMock()
            client.__aenter__ = AsyncMock(return_value=client)
            client.__aexit__ = AsyncMock(return_value=None)
            client.get = AsyncMock(return_value=response)
            with patch("runkite_runner.run_status.httpx.AsyncClient", return_value=client):
                actual = await should_skip_run(
                    "http://127.0.0.1:2026", "same", generation=1, tenant_id=" tenant ", run_incarnation_id="record-a"
                )
            self.assertEqual(actual, skip)
            headers = client.get.call_args.kwargs["headers"]
            self.assertEqual(headers["X-Runkite-Run-Incarnation"], "record-a")
            self.assertEqual(headers["X-Runkite-Generation"], "1")
            self.assertIn("X-Runkite-Tenant-Id-B64", headers)

    async def test_workers_echo_captured_proof_on_events_and_completion(self):
        for module in (worker, generic_worker):
            events, reports, proxy_proofs = [], [], []

            def stream(source, metadata, events=events):
                async def drain():
                    async for event in source:
                        events.append(runner_pb2.RunEventProto.FromString(event.SerializeToString()))
                    return runner_pb2.StreamEventsResponse(ok=True)

                return drain()

            async def report(request, metadata, reports=reports):
                reports.append(request)
                return runner_pb2.ReportStatusResponse(ok=True)

            async def execute(_adapter, assignment, send_event, cancel_event=None, proxy_proofs=proxy_proofs):
                proxy_proofs.append(tenant_headers()["X-Runkite-Run-Incarnation"])
                # User code cannot redirect the captured protocol/header proof.
                assignment["run_incarnation_id"] = "mutated-record"
                await send_event({"method": "end", "data": {"status": "success"}})
                return "success"

            class Adapter:
                checkpoint_framework = ""

                async def execute(self, assignment, send_event, cancel_event):
                    return await execute(self, assignment, send_event, cancel_event)

            assignment = {
                "run_id": "same",
                "thread_id": "thread",
                "graph_id": "agent",
                "generation": 2,
                "run_incarnation_id": "record-a",
            }
            with (
                self.subTest(worker=module.__name__),
                patch.object(module, "should_skip_run", new=AsyncMock(return_value=False)),
                patch.object(worker, "execute_run", new=execute),
            ):
                await module._handle_job(
                    SimpleNamespace(StreamEvents=stream, ReportStatus=report),
                    Adapter(),
                    SimpleNamespace(assignment_json=json.dumps(assignment)),
                    [],
                    {},
                    set(),
                    asyncio.Lock(),
                )
            self.assertEqual(proxy_proofs, ["record-a"])
            self.assertTrue(events)
            self.assertEqual({event.run_incarnation_id for event in events}, {"record-a"})
            self.assertEqual({request.run_incarnation_id for request in reports}, {"record-a"})

    async def test_heartbeat_and_completion_echo_original_record(self):
        requests = []

        async def heartbeat(request, metadata):
            requests.append(request)
            return runner_pb2.HeartbeatResponse(superseded=True)

        stopped = asyncio.Event()
        await heartbeat_loop(
            SimpleNamespace(Heartbeat=heartbeat),
            "same",
            [],
            generation=2,
            cancel_event=stopped,
            interval_s=0,
            run_incarnation_id="record-a",
        )
        self.assertTrue(stopped.is_set())
        self.assertEqual(requests[0].run_incarnation_id, "record-a")

        async def report(request, metadata):
            requests.append(request)
            return runner_pb2.ReportStatusResponse(ok=True)

        await finish_run(
            SimpleNamespace(ReportStatus=report),
            "same",
            2,
            "success",
            asyncio.sleep(0, result=runner_pb2.StreamEventsResponse(ok=True)),
            [],
            run_incarnation_id="record-a",
        )
        self.assertEqual(requests[-1].run_incarnation_id, "record-a")

    async def test_cancel_and_old_cleanup_cannot_touch_replacement_record(self):
        pending, early, lock = {}, set(), asyncio.Lock()
        await record_cancel_signal(pending, early, lock, "same", "record-a")
        old = await register_run(pending, early, lock, "same", "record-a")
        new = await register_run(pending, early, lock, "same", "record-b")
        self.assertTrue(old.is_set())
        self.assertFalse(new.is_set())
        await unregister_run(pending, lock, "same", old, "record-a")
        await record_cancel_signal(pending, early, lock, "same", "record-a")
        self.assertFalse(new.is_set())
        self.assertIn(new, pending[("same", "record-b")])
        await unregister_run(pending, lock, "same", old, "record-a")
        self.assertIn(new, pending[("same", "record-b")])
        sibling = await register_run(pending, early, lock, "same", "record-b")
        new.set()  # Supersession belongs only to this heartbeat's attempt.
        self.assertFalse(sibling.is_set())
        await record_cancel_signal(pending, early, lock, "same", "record-b")
        self.assertTrue(sibling.is_set())
        await unregister_run(pending, lock, "same", new, "record-b")
        await unregister_run(pending, lock, "same", sibling, "record-b")

    async def test_unscoped_cancel_signal_is_ignored(self):
        pending, early, lock = {}, set(), asyncio.Lock()
        self.assertFalse(await record_cancel_signal(pending, early, lock, "same", ""))
        self.assertEqual(pending, {})
        self.assertEqual(early, set())


if __name__ == "__main__":
    unittest.main()
