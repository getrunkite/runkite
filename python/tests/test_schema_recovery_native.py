"""Opt-in real SQLite plane and checkout Python/LangGraph schema recovery.

Set RUNKITE_TEST_SCHEMA_RECOVERY_BINARY to an absolute executable built from
this checkout, then run this file with the runner's Python dependencies. No
provider, Docker, database service, user configuration or installed runner is
used. The only runtime changes are fixture-local schema publisher timings and
observational wrappers around real gRPC calls. Heartbeat/fencing stays intact.
All HTTP clients target literal 127.0.0.1. The current Go CLI binds its ephemeral
listeners on all interfaces, so use an isolated local/CI test environment. Both
client API and runner interfaces require generated, temporary credentials.
"""

from __future__ import annotations

import contextlib
import http.client
import json
import os
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
AGENT = "schema-recovery-native"
SCHEMA_PATH = f"/internal/agents/{AGENT}/schema"
SCHEMA_FIELDS = ("input_schema", "output_schema", "state_schema", "config_schema")
MAX_BODY = 1 << 20
MAX_RESPONSE = 2 << 20


class FixtureFailure(RuntimeError):
    """A bounded, payload-free native qualification failure."""


def _private_json(path: Path, value: dict) -> None:
    temporary = path.with_suffix(".pending")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as output:
        json.dump(value, output, separators=(",", ":"))
    os.replace(temporary, path)


def _worker_fixture() -> None:
    """Run the actual CLI with shorter test-only schema timing and RPC counters."""
    sys.path.insert(0, str(ROOT / "python"))
    from runkite_runner import worker
    from runkite_runner.schema_introspect import SchemaPublisher

    metrics_path = Path(os.environ["RUNKITE_SCHEMA_NATIVE_DIR"]) / "rpc-metrics.json"
    metrics = {"pid": os.getpid(), "get_job_calls": 0, "assignments": 0, "heartbeats": 0, "generation": 0}
    _private_json(metrics_path, metrics)

    class FastSchemaPublisher(SchemaPublisher):
        def __init__(self, *args, **kwargs):
            super().__init__(*args, **kwargs, interval=0.25, request_timeout=1, round_timeout=2)

    original_stub = worker.runner_pb2_grpc.RunnerServiceStub

    def observed_stub(channel):
        stub = original_stub(channel)
        original_get_job = stub.GetJob
        original_heartbeat = stub.Heartbeat

        async def get_job(*args, **kwargs):
            metrics["get_job_calls"] += 1
            _private_json(metrics_path, metrics)
            response = await original_get_job(*args, **kwargs)
            if response.has_job:
                assignment = json.loads(response.assignment_json)
                metrics["assignments"] += 1
                metrics["generation"] = assignment.get("generation", 0)
                _private_json(metrics_path, metrics)
            return response

        async def heartbeat(*args, **kwargs):
            response = await original_heartbeat(*args, **kwargs)
            if response.ok and not response.superseded:
                metrics["heartbeats"] += 1
                _private_json(metrics_path, metrics)
            return response

        stub.GetJob = get_job
        stub.Heartbeat = heartbeat
        return stub

    worker.SchemaPublisher = FastSchemaPublisher
    worker.runner_pb2_grpc.RunnerServiceStub = observed_stub
    sys.argv = [sys.argv[0], *sys.argv[2:]]
    worker.main()


def _http(port: int, method: str, path: str, headers: dict | None = None, value: dict | None = None):
    """No proxy/netrc discovery, redirects, cookies or response-body diagnostics."""
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=2)
    body = None if value is None else json.dumps(value, separators=(",", ":")).encode()
    supplied = dict(headers or {})
    if body is not None:
        supplied["Content-Type"] = "application/json"
    try:
        connection.request(method, path, body=body, headers=supplied)
        response = connection.getresponse()
        content = response.read(MAX_RESPONSE + 1)
        if len(content) > MAX_RESPONSE:
            raise FixtureFailure("native HTTP response exceeded its bound")
        result = json.loads(content) if content else None
        return response.status, result
    except FixtureFailure:
        raise
    except Exception:
        raise FixtureFailure("native HTTP request failed") from None
    finally:
        connection.close()


class _SchemaProxy:
    """Owned loopback HTTP forwarding with a fault switch for schema paths only."""

    def __init__(self, upstream_port: int, runner_token: str, run_id: str):
        self.upstream_port = upstream_port
        self.runner_token = runner_token
        self.status_path = f"/internal/runs/{run_id}/status"
        self.lock = threading.Lock()
        self.blocked = False
        self.counts = {"put": 0, "post": 0, "post_success": 0, "faults": 0, "headers_valid": True}
        self.server = None
        self.thread = None

    def set_blocked(self, blocked: bool) -> None:
        # Schema forwarding holds this lock through the upstream response, so
        # setting the switch fences any earlier successful in-flight report.
        with self.lock:
            self.blocked = blocked

    def snapshot(self) -> dict:
        with self.lock:
            return self.counts.copy()

    def __enter__(self):
        owner = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def setup(self):
                super().setup()
                self.connection.settimeout(2)

            def log_message(self, *args):
                pass

            def _response(self, status, body=b"", headers=()):
                self.send_response(status)
                for name, value in headers:
                    if name.lower() not in {
                        "connection",
                        "content-length",
                        "transfer-encoding",
                        "keep-alive",
                        "upgrade",
                        "proxy-authenticate",
                        "proxy-authorization",
                        "te",
                        "trailer",
                    }:
                        self.send_header(name, value)
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Connection", "close")
                self.end_headers()
                self.close_connection = True
                self.wfile.write(body)

            def _handle(self):
                schema = (self.command == "PUT" and self.path == SCHEMA_PATH) or (
                    self.command == "POST" and self.path == SCHEMA_PATH + "/recover"
                )
                if not schema and not (self.command == "GET" and self.path == owner.status_path):
                    self._response(404)
                    return
                lengths = self.headers.get_all("Content-Length", [])
                if len(lengths) > 1 or self.headers.get("Transfer-Encoding"):
                    self._response(400)
                    return
                raw_length = lengths[0] if lengths else "0"
                if not raw_length.isascii() or not raw_length.isdecimal() or len(raw_length) > 8:
                    self._response(400)
                    return
                length = int(raw_length)
                if length > MAX_BODY:
                    self._response(413)
                    return
                body = self.rfile.read(length)
                if len(body) != length:
                    self._response(400)
                    return
                if schema:
                    with owner.lock:
                        owner.counts["put" if self.command == "PUT" else "post"] += 1
                        valid = (
                            self.headers.get("X-Runner-Kind") == "python-langgraph"
                            and self.headers.get("X-Runner-Token") == owner.runner_token
                            and self.headers.get("X-Runkite-Tenant-Id") == "default"
                            and self.headers.get("X-Runkite-Run-Id") is None
                            and self.headers.get("X-Runkite-Generation") is None
                        )
                        owner.counts["headers_valid"] &= valid
                        if owner.blocked:
                            owner.counts["faults"] += 1
                            self._response(503)
                        else:
                            status = self._forward(body)
                            if self.command == "POST" and 200 <= status < 300:
                                owner.counts["post_success"] += 1
                else:
                    self._forward(body)

            def _forward(self, body):
                connection = http.client.HTTPConnection("127.0.0.1", owner.upstream_port, timeout=2)
                try:
                    headers = {
                        name: value
                        for name, value in self.headers.items()
                        if name.lower() not in {"host", "connection", "content-length", "transfer-encoding"}
                    }
                    connection.request(self.command, self.path, body=body, headers=headers)
                    response = connection.getresponse()
                    content = response.read(MAX_RESPONSE + 1)
                    if len(content) > MAX_RESPONSE:
                        self._response(502)
                        return 502
                    status = response.status
                    self._response(status, content, response.getheaders())
                    return status
                except Exception:
                    with contextlib.suppress(Exception):
                        self._response(502)
                    return 502
                finally:
                    connection.close()

            def do_GET(self):
                self._safe_handle()

            do_POST = do_GET
            do_PUT = do_GET

            def _safe_handle(self):
                try:
                    self._handle()
                except Exception:
                    with contextlib.suppress(Exception):
                        self._response(502)

        class Server(ThreadingHTTPServer):
            daemon_threads = True
            block_on_close = False

            def handle_error(self, request, client_address):
                pass

        self.server = Server(("127.0.0.1", 0), Handler)
        self.server.timeout = 0.2
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.1}, daemon=True)
        self.thread.start()
        self.port = self.server.server_address[1]
        return self

    def __exit__(self, *args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(2)
        if self.thread.is_alive():
            raise FixtureFailure("native schema proxy did not stop")


def _port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return listener.getsockname()[1]


def _stop(process: subprocess.Popen, grace: float = 3) -> None:
    if process.poll() is not None:
        process.wait(timeout=1)
        return
    with contextlib.suppress(ProcessLookupError):
        os.killpg(process.pid, signal.SIGTERM)
    try:
        process.wait(timeout=grace)
    except subprocess.TimeoutExpired:
        with contextlib.suppress(ProcessLookupError):
            os.killpg(process.pid, signal.SIGKILL)
        try:
            process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            raise FixtureFailure("native owned process did not stop") from None


def _real_schema(value) -> bool:
    if not isinstance(value, dict):
        return False
    for field in ("input_schema", "output_schema", "state_schema"):
        props = value.get(field, {}).get("properties", {})
        if props.get("message", {}).get("type") != "string" or props.get("busy", {}).get("type") != "boolean":
            return False
    return isinstance(value.get("config_schema"), dict)


def _stub_schema(value) -> bool:
    return isinstance(value, dict) and all(value.get(field) == {"type": "object"} for field in SCHEMA_FIELDS)


@unittest.skipUnless(os.environ.get("RUNKITE_TEST_SCHEMA_RECOVERY_BINARY"), "native schema binary not selected")
class NativeSchemaRecoveryTests(unittest.TestCase):
    def test_long_lived_worker_recovers_after_idle_restart_and_while_busy(self):
        selected = os.environ["RUNKITE_TEST_SCHEMA_RECOVERY_BINARY"]
        binary = Path(selected)
        if not binary.is_absolute() or not binary.is_file() or not os.access(binary, os.X_OK):
            raise FixtureFailure("native schema binary must be an absolute executable")
        if os.name != "posix":
            self.skipTest("owned process groups require POSIX")
        processes = []
        log_files = []
        deadline = time.monotonic() + 240
        old_umask = os.umask(0o077)

        def timed_out(signum, frame):
            raise FixtureFailure("native qualification exceeded its overall deadline")

        previous_handler = signal.signal(signal.SIGALRM, timed_out)
        previous_timer = signal.setitimer(signal.ITIMER_REAL, 240)
        try:
            with tempfile.TemporaryDirectory(prefix="runkite-schema-native-") as directory:
                owned = Path(directory)
                for child in ("home", "tmp", "config", "cache"):
                    (owned / child).mkdir(mode=0o700)
                env = {
                    "PATH": os.defpath,
                    "HOME": str(owned / "home"),
                    "TMPDIR": str(owned / "tmp"),
                    "XDG_CONFIG_HOME": str(owned / "config"),
                    "XDG_CACHE_HOME": str(owned / "cache"),
                    "NETRC": os.devnull,
                    "LANG": "C",
                    "PYTHONUTF8": "1",
                    "PYTHONNOUSERSITE": "1",
                    "PYTHONDONTWRITEBYTECODE": "1",
                    "PYTHONUNBUFFERED": "1",
                    "PYTHONPATH": str(ROOT / "python"),
                }
                runner_token = secrets.token_hex(32)
                api_key = secrets.token_hex(32)
                config = owned / "langgraph.json"
                shutil.copyfile(Path(__file__).parent / "fixtures/schema_recovery_graph.py", owned / "graph.py")
                config.write_text(
                    json.dumps(
                        {
                            "graphs": {AGENT: "./graph.py:graph"},
                            "runner_kind": "python-langgraph",
                            "dependencies": ["."],
                            "auth": {
                                "type": "api_key",
                                "strict_permissions": True,
                                "keys": {
                                    api_key: {
                                        "name": "native-schema-fixture",
                                        "permissions": ["admin"],
                                        "tenant_id": "default",
                                    }
                                },
                            },
                        }
                    )
                )
                http_port, grpc_port = _port(), _port()
                while grpc_port == http_port:
                    grpc_port = _port()
                cp_env = {
                    **env,
                    "DATABASE_PATH": str(owned / "state.sqlite"),
                    "RUNKITE_ALLOW_INSECURE_SERVE": "1",
                    "RUNNER_TOKEN_PYTHON_LANGGRAPH": runner_token,
                    "RUNNER_TENANTS_PYTHON_LANGGRAPH": "default",
                }
                api_headers = {"Authorization": "Bearer " + api_key}
                runner_headers = {
                    "X-Runner-Kind": "python-langgraph",
                    "X-Runner-Token": runner_token,
                    "X-Runkite-Tenant-Id": "default",
                }
                run_id = str(uuid.uuid4())

                def spawn(command, child_env, label):
                    log = (owned / (label + ".log")).open("xb")
                    log_files.append(log)
                    try:
                        process = subprocess.Popen(
                            command,
                            cwd=owned,
                            env=child_env,
                            stdin=subprocess.DEVNULL,
                            stdout=log,
                            stderr=log,
                            start_new_session=True,
                        )
                    except Exception:
                        raise FixtureFailure("native owned process could not start") from None
                    processes.append(process)
                    return process

                def wait_for(predicate, label, timeout=30):
                    until = min(deadline, time.monotonic() + timeout)
                    while time.monotonic() < until:
                        if any(process.poll() is not None for process in processes if process is not retired_cp):
                            raise FixtureFailure("native owned process exited before qualification completed")
                        try:
                            if predicate():
                                return
                        except FixtureFailure:
                            pass
                        time.sleep(0.05)
                    raise FixtureFailure("native qualification deadline: " + label)

                def schema():
                    status, value = _http(http_port, "GET", f"/agents/{AGENT}/schemas", api_headers)
                    return value if status == 200 else None

                def metrics():
                    try:
                        value = json.loads((owned / "rpc-metrics.json").read_text())
                    except (OSError, ValueError):
                        raise FixtureFailure("native worker metrics unavailable") from None
                    if not isinstance(value, dict) or set(value) != {
                        "pid",
                        "get_job_calls",
                        "assignments",
                        "heartbeats",
                        "generation",
                    }:
                        raise FixtureFailure("native worker metrics invalid")
                    if any(type(item) is not int or item < 0 for item in value.values()):
                        raise FixtureFailure("native worker metrics invalid")
                    return value

                def start_plane(label):
                    return spawn(
                        [
                            str(binary),
                            "serve",
                            "--config",
                            str(config),
                            "--port",
                            str(http_port),
                            "--grpc-port",
                            str(grpc_port),
                        ],
                        cp_env,
                        label,
                    )

                retired_cp = None
                try:
                    cp = start_plane("plane-first")
                    wait_for(lambda: _stub_schema(schema()), "initial bootstrap schema")
                    self.assertTrue((owned / "state.sqlite").is_file(), "native SQLite fixture was not persisted")
                    with _SchemaProxy(http_port, runner_token, run_id) as proxy:
                        worker_env = {
                            **env,
                            "RUNNER_TOKEN": runner_token,
                            "RUNKITE_HTTP_URL": "",
                            "RUNKITE_SCHEMA_NATIVE_DIR": str(owned),
                        }
                        runner = spawn(
                            [
                                sys.executable,
                                str(Path(__file__).resolve()),
                                "--worker-fixture",
                                "--config",
                                str(config),
                                "--grpc-address",
                                f"127.0.0.1:{grpc_port}",
                                "--http-address",
                                f"http://127.0.0.1:{proxy.port}",
                                "--runner-kind",
                                "python-langgraph",
                                "--concurrency",
                                "1",
                            ],
                            worker_env,
                            "worker",
                        )
                        wait_for(lambda: _real_schema(schema()), "initial real schema")
                        wait_for(lambda: metrics()["get_job_calls"] >= 1, "initial job poll")
                        self.assertEqual(proxy.snapshot()["put"], 1, "native worker repeated its initial PUT")
                        worker_pid = runner.pid

                        # Block only schema HTTP while the idle plane is stopped
                        # and restarted on exactly the same DB/config/addresses.
                        proxy.set_blocked(True)
                        retired_cp = cp
                        _stop(cp, grace=18)
                        self.assertEqual(cp.returncode, 0, "idle plane did not shut down cleanly")
                        cp = start_plane("plane-restarted")
                        wait_for(lambda: _stub_schema(schema()), "restart bootstrap reset")
                        count_before = proxy.snapshot()["post_success"]
                        proxy.set_blocked(False)
                        wait_for(lambda: _real_schema(schema()), "idle recovery POST")
                        wait_for(lambda: proxy.snapshot()["post_success"] > count_before, "idle POST receipt")
                        self.assertEqual(runner.pid, worker_pid, "native worker process changed")
                        self.assertEqual(metrics()["pid"], worker_pid, "native worker identity changed")
                        self.assertEqual(proxy.snapshot()["put"], 1, "idle recovery used another normal PUT")

                        thread_id = str(uuid.uuid4())
                        status, _ = _http(
                            http_port,
                            "POST",
                            f"/threads/{thread_id}/runs",
                            api_headers,
                            {
                                "run_id": run_id,
                                "agent_id": AGENT,
                                "if_not_exists": "create",
                                "input": {"message": "public native schema fixture", "busy": True},
                            },
                        )
                        self.assertEqual(status, 200, "native run was not accepted")
                        wait_for(lambda: (owned / "started").is_file(), "busy static graph")
                        before = metrics()
                        self.assertEqual(before["assignments"], 1, "native run was dispatched more than once")
                        self.assertGreater(before["generation"], 0, "native run did not receive a fenced attempt")

                        proxy.set_blocked(True)
                        faults_before = proxy.snapshot()["faults"]
                        status, _ = _http(
                            http_port,
                            "PUT",
                            SCHEMA_PATH,
                            runner_headers,
                            {field: {"type": "object"} for field in SCHEMA_FIELDS},
                        )
                        self.assertEqual(status, 204, "authenticated schema stub reset failed")
                        self.assertTrue(_stub_schema(schema()), "busy schema reset was not visible")
                        wait_for(lambda: proxy.snapshot()["faults"] > faults_before, "schema-only HTTP fault")

                        # Retain the ordinary 2s heartbeat and 6s reclaim lease.
                        # More than one lease elapses while schema HTTP fails.
                        held_since = time.monotonic()
                        wait_for(
                            lambda: (
                                time.monotonic() - held_since >= 8
                                and metrics()["heartbeats"] >= before["heartbeats"] + 3
                            ),
                            "live heartbeat during schema outage",
                            timeout=20,
                        )
                        during = metrics()
                        self.assertEqual(
                            during["get_job_calls"],
                            before["get_job_calls"],
                            "worker polled another job while its only slot was occupied",
                        )
                        self.assertEqual(during["assignments"], 1, "busy run was redispatched")
                        self.assertEqual(during["generation"], before["generation"], "busy attempt was replaced")
                        status, active_run = _http(http_port, "GET", f"/runs/{run_id}", api_headers)
                        self.assertTrue(
                            status == 200 and active_run.get("status") == "running",
                            "busy run did not remain live during schema outage",
                        )
                        self.assertTrue(_stub_schema(schema()), "faulted schema path unexpectedly recovered")

                        count_before = proxy.snapshot()["post_success"]
                        proxy.set_blocked(False)
                        wait_for(lambda: _real_schema(schema()), "busy recovery POST")
                        wait_for(lambda: proxy.snapshot()["post_success"] > count_before, "busy POST receipt")
                        self.assertFalse((owned / "release").exists(), "busy graph was released before recovery")
                        self.assertEqual(
                            metrics()["get_job_calls"],
                            before["get_job_calls"],
                            "schema recovery depended on another job poll",
                        )
                        self.assertEqual(proxy.snapshot()["put"], 1, "busy recovery used another normal PUT")
                        self.assertTrue(proxy.snapshot()["headers_valid"], "schema request identity headers changed")
                        (owned / "release").write_text("release")

                        def completed():
                            code, value = _http(http_port, "GET", f"/runs/{run_id}", api_headers)
                            if code != 200 or not isinstance(value, dict):
                                return False
                            if value.get("status") in {"error", "interrupted", "timeout"}:
                                raise AssertionError("native held run ended unsuccessfully")
                            return value.get("status") == "success"

                        wait_for(completed, "native held run completion")
                        self.assertEqual(metrics()["assignments"], 1, "native run executed more than once")
                        _stop(runner)
                        _stop(cp, grace=18)
                finally:
                    # The deadline interrupts qualification, never cleanup. Each
                    # owned process then has a separate bounded kill/wait path.
                    signal.setitimer(signal.ITIMER_REAL, 0)
                    cleanup_failed = False
                    for process in reversed(processes):
                        try:
                            _stop(process, grace=3)
                        except Exception:
                            cleanup_failed = True
                    for log in log_files:
                        log.close()
                    if cleanup_failed or any(process.poll() is None for process in processes):
                        raise FixtureFailure("native owned-process cleanup failed")
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            signal.signal(signal.SIGALRM, previous_handler)
            if previous_timer[0]:
                signal.setitimer(signal.ITIMER_REAL, *previous_timer)
            os.umask(old_umask)


if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] == "--worker-fixture":
        _worker_fixture()
    else:
        unittest.main()
