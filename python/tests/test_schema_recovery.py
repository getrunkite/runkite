"""Bounded, context-independent static schema recovery, without a live provider.

Run: python -m unittest discover -s python/tests -p test_schema_recovery.py -v
The control-plane restart/atomic-stub behavior is qualified separately against
its real HTTP endpoint. MockTransport here exercises the real HTTPX client.
"""

from __future__ import annotations

import asyncio
import contextvars
import json
import os
import sys
import unittest
from types import SimpleNamespace
from typing import TypedDict
from unittest.mock import AsyncMock, Mock, patch

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import httpx  # noqa: E402
import runkite_runner.schema_introspect as si  # noqa: E402
from runkite_runner import worker  # noqa: E402
from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402

_REAL_CLIENT = httpx.AsyncClient


class State(TypedDict):
    value: str


class StaticGraph:
    builder = SimpleNamespace(state_schema=State)

    def __init__(self, title="original"):
        self.schema = {"type": "object", "title": title, "properties": {"value": {"type": "string"}}}
        self.calls = 0

    def get_input_jsonschema(self):
        self.calls += 1
        return self.schema

    get_output_jsonschema = get_input_jsonschema
    get_config_jsonschema = get_input_jsonschema


def client_factory(handler, clients=None, **extra):
    def create(*args, **kwargs):
        client = _REAL_CLIENT(*args, transport=httpx.MockTransport(handler), **kwargs, **extra)
        if clients is not None:
            clients.append(client)
        return client

    return create


class UnreadBody(httpx.AsyncByteStream):
    def __init__(self):
        self.closed = False

    async def __aiter__(self):
        raise AssertionError("Schema publisher must not read the response body")
        yield b""  # pragma: no cover

    async def aclose(self):
        self.closed = True


class SchemaRecoveryTests(unittest.IsolatedAsyncioTestCase):
    async def test_cached_bytes_survive_graph_mutation_and_stub_reset(self):
        graph = StaticGraph()
        retained = None
        requests = []
        recovered = asyncio.Event()

        def handler(request):
            nonlocal retained
            requests.append(request)
            if request.method == "PUT":
                retained = request.content
            elif retained is None:  # Mimic a control-plane bootstrap reset.
                retained = request.content
                recovered.set()
            return httpx.Response(204)

        with patch.object(si.httpx, "AsyncClient", client_factory(handler)):
            publisher = si.SchemaPublisher({"agent": graph}, "http://plane", "", "kind", interval=0.01)
            await publisher.report_startup()
            first_body = retained
            graph.schema["title"] = "changed after capture"
            retained = None
            publisher.start()
            try:
                await asyncio.wait_for(recovered.wait(), 1)
            finally:
                await publisher.stop()
        self.assertEqual(graph.calls, 3)
        self.assertEqual(retained, first_body)
        self.assertEqual(json.loads(retained)["input_schema"]["title"], "original")
        self.assertEqual([r.method for r in requests], ["PUT", "POST"])
        self.assertTrue(str(requests[-1].url).endswith("/schema/recover"))

    async def test_initial_http_outage_recovers_without_a_second_put(self):
        requests = []
        restored = asyncio.Event()

        def handler(request):
            requests.append(request)
            if request.method == "PUT":
                return httpx.Response(503, stream=UnreadBody())
            restored.set()
            return httpx.Response(204)

        graph = StaticGraph()
        with patch.object(si.httpx, "AsyncClient", client_factory(handler)):
            publisher = si.SchemaPublisher({"a": graph}, "http://plane", "", "kind")
            await publisher.report_startup()
            publisher.start()
            try:
                await asyncio.wait_for(restored.wait(), 1)
            finally:
                await publisher.stop()
        self.assertEqual([request.method for request in requests], ["PUT", "POST"])
        self.assertEqual(requests[0].content, requests[1].content)
        self.assertEqual(graph.calls, 3)

    async def test_old_plane_404_never_falls_back_to_put_and_bodies_are_unread(self):
        methods = []
        bodies = []
        enough = asyncio.Event()

        def handler(request):
            methods.append(request.method)
            body = UnreadBody()
            bodies.append(body)
            if methods.count("POST") >= 3:
                enough.set()
            return httpx.Response(404, stream=body)

        with patch.object(si.httpx, "AsyncClient", client_factory(handler)):
            publisher = si.SchemaPublisher({"agent": StaticGraph()}, "http://plane", "", "kind", interval=0.005)
            await publisher.report_startup()
            publisher.start()
            try:
                await asyncio.wait_for(enough.wait(), 1)
            finally:
                await publisher.stop()
        self.assertEqual(methods, ["PUT", "POST", "POST", "POST"])
        self.assertTrue(all(body.closed for body in bodies))

    async def test_old_plane_initial_put_backlog_is_drained_once_across_bounded_rounds(self):
        requests = []
        all_initial_attempted = asyncio.Event()
        recovered_after_backlog = asyncio.Event()
        names = ["a", "b", "c", "d"]

        async def handler(request):
            requests.append((request.method, request.url.path))
            if request.method == "PUT":
                if sum(method == "PUT" for method, _ in requests) == len(names):
                    all_initial_attempted.set()
                # Each initial request exhausts this small round's deadline.
                await asyncio.Event().wait()
            elif all_initial_attempted.is_set():
                recovered_after_backlog.set()
            return httpx.Response(404)

        with patch.object(si.httpx, "AsyncClient", client_factory(handler)):
            publisher = si.SchemaPublisher(
                {name: StaticGraph() for name in names},
                "http://old-plane",
                "",
                "kind",
                interval=0.005,
                request_timeout=1,
                round_timeout=0.02,
            )
            await asyncio.wait_for(publisher.report_startup(), 0.5)
            self.assertEqual(requests, [("PUT", "/internal/agents/a/schema")])
            publisher.start()
            try:
                await asyncio.wait_for(recovered_after_backlog.wait(), 1)
            finally:
                await publisher.stop()
        self.assertEqual(
            [path for method, path in requests if method == "PUT"],
            [f"/internal/agents/{name}/schema" for name in names],
        )
        self.assertTrue(any(method == "POST" for method, _ in requests))
        self.assertTrue(all(path.endswith("/schema/recover") for method, path in requests if method == "POST"))

    async def test_exact_agent_path_default_tenant_auth_and_fresh_task_context(self):
        marker = contextvars.ContextVar("schema_test_marker", default="clean")
        captured = []
        recovered = asyncio.Event()
        graph_id = " agent/?#雪 "

        def handler(request):
            captured.append((request, marker.get()))
            if request.method == "POST":
                recovered.set()
            return httpx.Response(204)

        tenant_token = bind_tenant("private tenant")
        run_token = bind_run("private run", 42, run_incarnation_id="record-fixture")
        marker_token = marker.set("private assignment")
        try:
            with patch.object(si.httpx, "AsyncClient", client_factory(handler)):
                publisher = si.SchemaPublisher({graph_id: StaticGraph()}, "http://plane/", "token", "pool:python")
                await publisher.report_startup()
                publisher.start()
                try:
                    await asyncio.wait_for(recovered.wait(), 1)
                finally:
                    await publisher.stop()
        finally:
            marker.reset(marker_token)
            reset_run(run_token)
            reset_tenant(tenant_token)
        self.assertEqual([value for _, value in captured], ["private assignment", "clean"])
        for request, _ in captured:
            self.assertEqual(request.headers["X-Runkite-Tenant-Id"], "default")
            self.assertEqual(request.headers["X-Runner-Kind"], "pool:python")
            self.assertEqual(request.headers["X-Runner-Token"], "token")
            self.assertNotIn("X-Runkite-Run-Id", request.headers)
            self.assertNotIn("X-Runkite-Generation", request.headers)
            self.assertIn(b"%20agent%2F%3F%23%E9%9B%AA%20/schema", request.url.raw_path)
            self.assertEqual(request.url.query, b"")
            self.assertEqual(request.headers["Content-Type"], "application/json")

    async def test_dot_agent_is_encoded_as_an_exact_segment(self):
        captured = []

        def handler(request):
            captured.append(request.url.raw_path)
            return httpx.Response(204)

        with patch.object(si.httpx, "AsyncClient", client_factory(handler)):
            publisher = si.SchemaPublisher({"..": StaticGraph()}, "http://plane", "", "kind")
            await publisher.report_startup()
        self.assertEqual(captured, [b"/internal/agents/%2E%2E/schema"])

    async def test_round_deadline_advances_to_next_graph_and_bounds_serial_work(self):
        attempted = []
        active = 0
        max_active = 0

        async def handler(request):
            nonlocal active, max_active
            attempted.append(request.url.path)
            active += 1
            max_active = max(max_active, active)
            try:
                await asyncio.Event().wait()
            finally:
                active -= 1

        with patch.object(si.httpx, "AsyncClient", client_factory(handler)):
            publisher = si.SchemaPublisher(
                {name: StaticGraph() for name in ("a", "b", "c")},
                "http://plane",
                "",
                "kind",
                request_timeout=1,
                round_timeout=0.02,
            )
            async with publisher._client() as client:
                for _ in range(3):
                    await asyncio.wait_for(publisher._round(client, recovery=True), 0.5)
        self.assertEqual(attempted, [f"/internal/agents/{name}/schema" for name in ("a", "b", "c")])
        self.assertEqual(active, 0)
        self.assertEqual(max_active, 1)

    async def test_each_request_has_total_deadline_and_next_graph_is_attempted(self):
        attempted = []

        async def handler(request):
            attempted.append(request.url.path)
            await asyncio.Event().wait()

        with patch.object(si.httpx, "AsyncClient", client_factory(handler)):
            publisher = si.SchemaPublisher(
                {"a": StaticGraph(), "b": StaticGraph()},
                "http://plane",
                "",
                "kind",
                request_timeout=0.01,
                round_timeout=1,
            )
            await asyncio.wait_for(publisher.report_startup(), 0.5)
        self.assertEqual(attempted, ["/internal/agents/a/schema", "/internal/agents/b/schema"])

    async def test_cancellation_awaits_and_closes_the_owned_client(self):
        entered = asyncio.Event()
        request_cancelled = asyncio.Event()
        clients = []

        async def handler(request):
            entered.set()
            try:
                await asyncio.Event().wait()
            finally:
                request_cancelled.set()

        with patch.object(si.httpx, "AsyncClient", client_factory(handler, clients)):
            publisher = si.SchemaPublisher({"a": StaticGraph()}, "http://plane", "", "kind")
            task = publisher.start()
            self.assertIs(publisher.start(), task)
            await asyncio.wait_for(entered.wait(), 1)
            await asyncio.wait_for(publisher.stop(), 1)
            await publisher.stop()
        self.assertTrue(task.done())
        self.assertTrue(request_cancelled.is_set())
        self.assertEqual(len(clients), 1)
        self.assertTrue(clients[0].is_closed)

    async def test_tls_settings_are_used_for_startup_and_recovery(self):
        kwargs_seen = []
        recovered = asyncio.Event()
        verify = object()

        def handler(request):
            if request.method == "POST":
                recovered.set()
            return httpx.Response(204)

        def create(**kwargs):
            kwargs_seen.append(kwargs.copy())
            kwargs.pop("verify")
            return _REAL_CLIENT(transport=httpx.MockTransport(handler), **kwargs)

        with (
            patch.object(si, "httpx_tls_kwargs", return_value={"verify": verify}),
            patch.object(si.httpx, "AsyncClient", create),
        ):
            publisher = si.SchemaPublisher({"a": StaticGraph()}, "https://plane", "", "kind")
            await publisher.report_startup()
            publisher.start()
            try:
                await asyncio.wait_for(recovered.wait(), 1)
            finally:
                await publisher.stop()
        self.assertEqual(len(kwargs_seen), 2)
        self.assertTrue(all(kwargs["verify"] is verify and kwargs["timeout"] == 5 for kwargs in kwargs_seen))

    async def test_client_initialization_failure_retries_the_unattempted_initial_request(self):
        recovered = asyncio.Event()
        attempts = 0
        methods = []

        def handler(request):
            methods.append(request.method)
            recovered.set()
            return httpx.Response(204)

        def create(**kwargs):
            nonlocal attempts
            attempts += 1
            if attempts == 1:
                raise RuntimeError("private TLS path")
            return _REAL_CLIENT(transport=httpx.MockTransport(handler), **kwargs)

        with patch.object(si.httpx, "AsyncClient", create):
            publisher = si.SchemaPublisher({"a": StaticGraph()}, "http://plane", "", "kind", interval=0.005)
            publisher.start()
            try:
                await asyncio.wait_for(recovered.wait(), 1)
            finally:
                await publisher.stop()
        self.assertEqual(attempts, 2)
        self.assertEqual(methods, ["PUT"])

    async def test_fixed_diagnostics_do_not_expose_application_or_transport_data(self):
        secret = "private-schema-token-and-graph-id"

        class BrokenGraph(StaticGraph):
            def get_input_jsonschema(self):
                raise RuntimeError(secret)

        def handler(request):
            raise httpx.ConnectError(secret, request=request)

        with (
            self.assertLogs("runkite.runner", level="DEBUG") as logs,
            patch.object(si.httpx, "AsyncClient", client_factory(handler)),
        ):
            publisher = si.SchemaPublisher({secret: BrokenGraph(secret)}, "http://plane", secret, "kind")
            await publisher.report_startup()
        self.assertNotIn(secret, "\n".join(logs.output))
        self.assertIn("Schema report failed", "\n".join(logs.output))

    async def test_no_client_or_task_for_empty_graphs_or_unserializable_schema(self):
        with patch.object(si.httpx, "AsyncClient", side_effect=AssertionError("unexpected HTTP client")):
            for graphs in ({}, {"bad": StaticGraph(object())}):
                publisher = si.SchemaPublisher(graphs, "http://plane", "", "kind")
                self.assertFalse(publisher.has_reports)
                await publisher.report_startup()
                self.assertIsNone(publisher.start())
                await publisher.stop()

    async def test_cache_bounds_skip_oversize_and_excess_graphs_without_reintrospection(self):
        graphs = {"large": StaticGraph("x" * 4096), "small": StaticGraph(), "extra": StaticGraph()}
        with patch.object(si, "MAX_SCHEMA_BYTES", 1024), patch.object(si, "MAX_CACHED_GRAPHS", 2):
            publisher = si.SchemaPublisher(graphs, "http://plane", "", "kind")
        self.assertEqual(len(publisher._reports), 1)
        self.assertEqual(graphs["large"].calls, 3)
        self.assertEqual(graphs["small"].calls, 3)
        self.assertEqual(graphs["extra"].calls, 0)
        body_size = len(publisher._reports[0].body)
        with patch.object(si, "MAX_CACHE_BYTES", body_size * 2):
            publisher = si.SchemaPublisher(
                {name: StaticGraph() for name in ("a", "b", "c")}, "http://plane", "", "kind"
            )
        self.assertEqual(len(publisher._reports), 2)
        self.assertLessEqual(sum(len(report.body) for report in publisher._reports), body_size * 2)


class WorkerSchemaLifecycleTests(unittest.IsolatedAsyncioTestCase):
    async def _worker(self, *, setup_fails=False, empty=False):
        recovered = asyncio.Event()
        clients = []
        requests = []
        constructed = []

        def handler(request):
            requests.append(request.method)
            if request.method == "POST":
                recovered.set()
            return httpx.Response(204)

        def publisher(*args):
            obj = si.SchemaPublisher(*args, interval=0.005)
            constructed.append(obj)
            return obj

        async def watch_cancels(*args, **kwargs):
            await asyncio.Event().wait()
            yield  # pragma: no cover

        stub = Mock(WatchCancels=watch_cancels)
        adapter = Mock(graphs={} if empty else {"a": StaticGraph()})
        adapter.load_config = AsyncMock()
        checkpointer = Mock(start=AsyncMock(), stop=AsyncMock())
        store = Mock(warm=AsyncMock(), aclose=AsyncMock(), mode="memory")

        async def poll(*args, **kwargs):
            if not empty:
                # Poll/job work stays busy while the independent publisher runs.
                await asyncio.wait_for(recovered.wait(), 1)
            raise RuntimeError("end worker fixture")

        with (
            patch.object(worker, "validate_pool_environment"),
            patch.object(worker, "init_tracing", return_value=Mock()),
            patch.object(worker, "LangGraphAdapter", return_value=adapter),
            patch.object(worker, "SchemaPublisher", side_effect=publisher),
            patch.object(worker, "CheckpointerManager", return_value=checkpointer),
            patch.object(worker, "RunkiteStore", return_value=store),
            patch.object(worker, "grpc_channel_credentials", return_value=None),
            patch.object(worker.grpc.aio, "insecure_channel", return_value=Mock()),
            patch.object(worker.runner_pb2_grpc, "RunnerServiceStub", return_value=stub),
            patch.object(worker, "_load_custom_app_config", return_value=None),
            patch.object(worker, "_poll_loop", side_effect=poll),
            patch.object(si.httpx, "AsyncClient", client_factory(handler, clients)),
        ):
            if setup_fails:
                store.warm.side_effect = RuntimeError("setup failed")
            with self.assertRaisesRegex(RuntimeError, "setup failed" if setup_fails else "end worker fixture"):
                await worker.run_worker("unused.json")
        self.assertIsNone(constructed[0]._task)
        self.assertTrue(all(client.is_closed for client in clients))
        return requests, recovered.is_set(), checkpointer, store

    async def test_worker_owns_recovery_while_poll_is_busy_and_awaits_shutdown(self):
        requests, recovered, checkpointer, store = await self._worker()
        self.assertEqual(requests, ["PUT", "POST"])
        self.assertTrue(recovered)
        checkpointer.stop.assert_awaited_once()
        store.aclose.assert_awaited_once()

    async def test_resource_setup_failure_does_not_orphan_recovery(self):
        requests, recovered, _, _ = await self._worker(setup_fails=True)
        self.assertEqual(requests, ["PUT"])
        self.assertFalse(recovered)

    async def test_factory_only_worker_has_no_schema_http_or_background_task(self):
        requests, recovered, _, _ = await self._worker(empty=True)
        self.assertEqual(requests, [])
        self.assertFalse(recovered)


if __name__ == "__main__":
    unittest.main()
