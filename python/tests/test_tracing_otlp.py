"""Provider-free OTLP/HTTP wire qualification of the real Python exporter.

Requires the runner's ``otel`` extra and LangChain dependencies. Run directly:
    PYTHONPATH=python python python/tests/test_tracing_otlp.py

The receiver is a bounded loopback capture, not a Langfuse implementation.
No models, project credentials, or customer data are used.
"""

from __future__ import annotations

import asyncio
import base64
import os
import runpy
import socket
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from unittest.mock import patch
from urllib.parse import quote
from uuid import UUID

PYTHON_ROOT = Path(__file__).resolve().parents[1]
TRACE_PATH = "/api/public/otel/v1/traces"
AUTHORIZATION = "Basic " + base64.b64encode(b"pk-synthetic:sk-synthetic").decode("ascii")
PRIVATE_VALUES = ("synthetic-prompt-body", "synthetic-completion-body", "synthetic-tool-input", "synthetic-tool-output")
ERROR_VALUE = "synthetic-exported-exception"
MAX_BODY = 128 * 1024


async def emit_spans() -> None:
    # Import the checkout, not an installed runner. Each invocation has fresh
    # OTel globals and exercises init(), callbacks, batching, and shutdown().
    sys.path.insert(0, str(PYTHON_ROOT))
    from runkite_runner import tracing

    shutdown = tracing.init()

    async def emit_one(index: int) -> None:
        run_id = f"wire-run-{index}"
        try:
            with tracing.run_span(
                run_id,
                graph_id="synthetic-agent",
                thread_id=f"wire-thread-{index}",
                tenant_id="synthetic-tenant",
                trace_context={
                    "traceparent": "00-"
                    + ("11" if index == 0 else "33") * 16
                    + "-"
                    + ("22" if index == 0 else "44") * 8
                    + "-01",
                    "tracestate": f"wire=case{index}",
                    "correlation_id": f"wire-correlation-{index}",
                },
            ) as span:
                if span is None:
                    raise AssertionError("real tracing initialization was disabled")
                callbacks = tracing.make_run_callbacks(run_id)
                if len(callbacks) != 1:
                    raise AssertionError("real LangChain callback was unavailable")
                handler = callbacks[0]
                llm_id, tool_id = UUID(int=index * 2 + 1), UUID(int=index * 2 + 2)
                if index == 0:
                    handler.on_chat_model_start(
                        {"name": "ignored-model"},
                        [[{"content": PRIVATE_VALUES[0]}]],
                        run_id=llm_id,
                        metadata={"ls_model_name": "synthetic-model"},
                    )
                else:
                    handler.on_llm_start(
                        {"name": "ignored-model"},
                        [PRIVATE_VALUES[0]],
                        run_id=llm_id,
                        metadata={"ls_model_name": "synthetic-model"},
                    )
                await asyncio.sleep(0)
                handler.on_tool_start(
                    {"name": "synthetic-tool"}, PRIVATE_VALUES[2], run_id=tool_id, parent_run_id=llm_id
                )
                await asyncio.sleep(0)
                if index == 1:
                    error = RuntimeError(ERROR_VALUE)
                    handler.on_tool_error(error, run_id=tool_id)
                    handler.on_llm_error(error, run_id=llm_id)
                    tracing.set_run_status(span, "error")
                    raise error
                handler.on_tool_end(PRIVATE_VALUES[3], run_id=tool_id)
                handler.on_llm_end({"generations": [[{"text": PRIVATE_VALUES[1]}]]}, run_id=llm_id)
                tracing.set_run_status(span, "success")
        except RuntimeError as error:
            if index != 1 or str(error) != ERROR_VALUE:
                raise

    try:
        await asyncio.gather(emit_one(0), emit_one(1))
    finally:
        # The 60-second scheduled batch delay exceeds the subprocess deadline.
        # A successful capture therefore depends on graceful shutdown flushing.
        shutdown()


class Capture:
    def __init__(self) -> None:
        self.requests: list[tuple[str, dict[str, str], bytes]] = []
        self.errors: list[str] = []
        self.headers_started = threading.Event()
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def setup(self) -> None:
                super().setup()
                # Apply before BaseHTTPRequestHandler reads the request line
                # or headers. HTTPServer.timeout does not bound serve_forever.
                self.connection.settimeout(3)
                owner.headers_started.set()

            def do_POST(self) -> None:
                try:
                    length = int(self.headers.get("Content-Length", "0"))
                except ValueError:
                    length = 0
                if not 0 < length <= MAX_BODY or len(owner.requests) >= 4:
                    owner.errors.append("capture request exceeded its bounds")
                    self.send_error(413)
                    return
                self.connection.settimeout(3)
                body = self.rfile.read(length)
                if len(body) != length:
                    owner.errors.append("capture received an incomplete body")
                owner.requests.append((self.path, {key.lower(): value for key, value in self.headers.items()}, body))
                # An empty protobuf ExportTraceServiceResponse is valid OTLP.
                self.send_response(200)
                self.send_header("Content-Type", "application/x-protobuf")
                self.send_header("Content-Length", "0")
                self.send_header("Connection", "close")
                self.end_headers()

            def log_message(self, _format: str, *args: object) -> None:
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)

    def __enter__(self) -> Capture:
        self.thread.start()
        return self

    def __exit__(self, *_args: object) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=4)
        if self.thread.is_alive():
            raise AssertionError("capture did not stop within its deadline")


def child_environment(port: int, signal_specific: bool, home: str) -> dict[str, str]:
    # An allowlist excludes ambient provider keys, OTel credentials, proxy
    # settings, SDK instrumentation and resource metadata from the subprocess.
    env = {key: os.environ[key] for key in ("PATH", "SYSTEMROOT") if key in os.environ}
    env.update(
        PYTHONNOUSERSITE="1",
        PYTHONDONTWRITEBYTECODE="1",
        HOME=home,
        NETRC=os.devnull,
        NO_PROXY="127.0.0.1,localhost",
        OTEL_SERVICE_NAME="runkite-python-wire",
        OTEL_BSP_SCHEDULE_DELAY="60000",
        OTEL_BSP_MAX_EXPORT_BATCH_SIZE="512",
        OTEL_BSP_MAX_QUEUE_SIZE="2048",
        OTEL_EXPORTER_OTLP_TIMEOUT="2",
    )
    headers = "Authorization=" + quote(AUTHORIZATION, safe="") + ",x-langfuse-ingestion-version=4"
    base_endpoint = f"http://127.0.0.1:{port}/api/public/otel"
    if signal_specific:
        env.update(
            OTEL_EXPORTER_OTLP_ENDPOINT=f"http://127.0.0.1:{port}/wrong-base",
            OTEL_EXPORTER_OTLP_PROTOCOL="grpc",
            OTEL_EXPORTER_OTLP_HEADERS="Authorization=wrong,x-langfuse-ingestion-version=0",
            OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=base_endpoint + "/v1/traces",
            OTEL_EXPORTER_OTLP_TRACES_PROTOCOL="http/protobuf",
            OTEL_EXPORTER_OTLP_TRACES_HEADERS=headers,
        )
    else:
        env.update(
            OTEL_EXPORTER_OTLP_ENDPOINT=base_endpoint,
            OTEL_EXPORTER_OTLP_PROTOCOL="http/protobuf",
            OTEL_EXPORTER_OTLP_HEADERS=headers,
        )
    return env


class OTLPWireTests(unittest.TestCase):
    def test_synthetic_authorization_cannot_be_overridden_by_personal_netrc(self) -> None:
        with tempfile.TemporaryDirectory(prefix="runkite-otel-credentials-") as temporary:
            ambient_home, isolated_home = (Path(temporary) / name for name in ("ambient", "isolated"))
            ambient_home.mkdir(mode=0o700)
            isolated_home.mkdir(mode=0o700)
            for home in (ambient_home, isolated_home):
                # Both are owned synthetic files. Never inspect a user's netrc.
                netrc = home / ".netrc"
                netrc.write_text("machine 127.0.0.1 login synthetic-netrc-user password synthetic-netrc-password\n")
                netrc.chmod(0o600)
            with patch.dict(
                os.environ,
                {
                    "HOME": str(ambient_home),
                    "NETRC": str(ambient_home / ".netrc"),
                    "HTTP_PROXY": "http://synthetic-proxy.invalid",
                    "OTEL_RESOURCE_ATTRIBUTES": "synthetic.ambient=must-not-export",
                },
            ):
                environment = child_environment(1, signal_specific=False, home=str(isolated_home))
            self.assertEqual(environment["HOME"], str(isolated_home))
            self.assertEqual(environment["NETRC"], os.devnull)
            self.assertNotIn("HTTP_PROXY", environment)
            self.assertNotIn("OTEL_RESOURCE_ATTRIBUTES", environment)
            for native_guard in (False, True):
                with self.subTest(native_guard=native_guard):
                    child_env = dict(environment)
                    mode = "--check-auth-isolation"
                    if native_guard:
                        # Exercise the standalone emitter's own guard even if
                        # its caller passes a credential-bearing environment.
                        child_env["HOME"] = str(ambient_home)
                        child_env["NETRC"] = str(ambient_home / ".netrc")
                        mode = "--check-native-auth-isolation"
                    result = subprocess.run(
                        [sys.executable, str(Path(__file__).resolve()), mode],
                        env=child_env,
                        capture_output=True,
                        timeout=10,
                        check=False,
                    )
                    self.assertEqual(result.returncode, 0, "synthetic HTTP authorization isolation failed")

    def test_idle_request_headers_do_not_block_capture_shutdown(self) -> None:
        # Keep the idle peer open while __exit__ stops the capture. A separate
        # process ensures a future broken shutdown cannot hang the test suite.
        with tempfile.TemporaryDirectory(prefix="runkite-otel-idle-") as home:
            result = subprocess.run(
                [sys.executable, str(Path(__file__).resolve()), "--idle-header"],
                env=child_environment(1, signal_specific=False, home=home),
                capture_output=True,
                timeout=10,
                check=False,
            )
        self.assertEqual(result.returncode, 0, "capture could not stop with incomplete request headers")

    def assert_wire(self, signal_specific: bool) -> None:
        from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest

        with tempfile.TemporaryDirectory(prefix="runkite-otel-wire-") as home, Capture() as capture:
            result = subprocess.run(
                [sys.executable, str(Path(__file__).resolve()), "--emit"],
                env=child_environment(capture.server.server_port, signal_specific, home=home),
                capture_output=True,
                timeout=20,
                check=False,
            )
        # Do not echo exporter diagnostics or captured payloads into CI logs.
        self.assertEqual(result.returncode, 0, "exporter subprocess failed")
        self.assertFalse(capture.errors, "capture rejected a request")
        self.assertTrue(capture.requests, "graceful shutdown did not deliver an OTLP request")
        spans = []
        for path, headers, body in capture.requests:
            self.assertEqual(path, TRACE_PATH)
            self.assertTrue(
                headers.get("authorization") == AUTHORIZATION, "exporter did not use synthetic authorization"
            )
            self.assertEqual(headers.get("x-langfuse-ingestion-version"), "4")
            self.assertEqual(headers.get("content-type"), "application/x-protobuf")
            self.assertNotIn("content-encoding", headers)
            for private in PRIVATE_VALUES:
                self.assertNotIn(private.encode(), body, "normal callback body was copied to telemetry")
            request = ExportTraceServiceRequest.FromString(body)
            for resource_spans in request.resource_spans:
                resource = {
                    attribute.key: attribute.value.string_value for attribute in resource_spans.resource.attributes
                }
                self.assertEqual(resource.get("service.name"), "runkite-python-wire")
                for scope in resource_spans.scope_spans:
                    self.assertEqual(scope.scope.name, "runkite.runner")
                    spans.extend(scope.spans)
        self.assertEqual(len(spans), 6, "missing or duplicate exported spans")
        for index in range(2):
            run_id = f"wire-run-{index}"
            selected = {
                span.name: span
                for span in spans
                if any(a.key == "run.id" and a.value.string_value == run_id for a in span.attributes)
            }
            self.assertEqual(set(selected), {"runkite.run", "runkite.llm", "runkite.tool"})
            run, llm, tool = (selected[name] for name in ("runkite.run", "runkite.llm", "runkite.tool"))
            trace_id = bytes.fromhex(("11" if index == 0 else "33") * 16)
            for span in selected.values():
                self.assertEqual(span.trace_id, trace_id)
                self.assertEqual(span.status.code, 1 if index == 0 else 2)
                self.assertGreater(span.end_time_unix_nano, span.start_time_unix_nano)
            self.assertEqual(run.parent_span_id.hex(), ("22" if index == 0 else "44") * 8)
            self.assertEqual(run.trace_state, f"wire=case{index}")
            self.assertEqual(llm.parent_span_id, run.span_id)
            self.assertEqual(tool.parent_span_id, llm.span_id)
            attrs = {a.key: a.value.string_value for a in run.attributes}
            self.assertEqual(attrs["graph.id"], "synthetic-agent")
            self.assertEqual(attrs["thread.id"], f"wire-thread-{index}")
            self.assertEqual(attrs["tenant.id"], "synthetic-tenant")
            self.assertEqual(attrs["correlation.id"], f"wire-correlation-{index}")
            self.assertEqual(attrs["run.status"], "success" if index == 0 else "error")
            self.assertIn(("llm.name", "synthetic-model"), [(a.key, a.value.string_value) for a in llm.attributes])
            self.assertIn(("tool.name", "synthetic-tool"), [(a.key, a.value.string_value) for a in tool.attributes])
            if index == 1:
                # Privacy boundary: normal callback bodies are excluded, but
                # raw exception messages and stack traces are intentionally not redacted.
                for span in selected.values():
                    if span.name == "runkite.run":
                        # SDK versions may add the exception type when the
                        # error escapes start_as_current_span's context.
                        self.assertIn(span.status.message, (ERROR_VALUE, "RuntimeError: " + ERROR_VALUE))
                    else:
                        self.assertEqual(span.status.message, ERROR_VALUE)
                    self.assertTrue(any(event.name == "exception" for event in span.events))
                    self.assertTrue(
                        any(
                            a.key == "exception.message" and a.value.string_value == ERROR_VALUE
                            for event in span.events
                            for a in event.attributes
                        )
                    )

    def test_signal_specific_endpoint_protocol_and_headers_take_precedence(self) -> None:
        self.assert_wire(signal_specific=True)

    def test_general_endpoint_appends_trace_path(self) -> None:
        self.assert_wire(signal_specific=False)


if __name__ == "__main__":
    if sys.argv[1:] == ["--emit"]:
        asyncio.run(emit_spans())
    elif sys.argv[1:] == ["--idle-header"]:
        idle_peer = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        idle_peer.settimeout(3)
        try:
            with Capture() as idle_capture:
                idle_peer.connect(("127.0.0.1", idle_capture.server.server_port))
                idle_peer.sendall(b"POST /api/public/otel/v1/traces HTTP/1.1\r\nHost: unfinished")
                if not idle_capture.headers_started.wait(timeout=3):
                    raise AssertionError("capture never accepted the idle peer")
            if idle_capture.requests:
                raise AssertionError("incomplete headers reached the exporter capture")
            if idle_capture.thread.is_alive():
                raise AssertionError("capture thread outlived shutdown")
        finally:
            idle_peer.close()
    elif sys.argv[1:] in (["--check-auth-isolation"], ["--check-native-auth-isolation"]):
        if sys.argv[1:] == ["--check-native-auth-isolation"]:
            fixture = runpy.run_path(str(PYTHON_ROOT / "tests/fixtures/tracing_otlp_emit.py"))
            fixture["_isolate_http_credentials"]()
        import requests

        session = requests.Session()
        try:
            # prepare_request exercises real Requests authentication selection,
            # including netrc, without DNS, sockets, or a provider call.
            prepared = session.prepare_request(
                requests.Request(
                    "POST", "http://127.0.0.1:1/api/public/otel/v1/traces", headers={"Authorization": AUTHORIZATION}
                )
            )
            if prepared.headers.get("Authorization") != AUTHORIZATION:
                raise AssertionError("synthetic HTTP authorization was overridden")
        finally:
            session.close()
    else:
        unittest.main()
