"""Opt-in real plane, wheel-installed runner core and native Agent Framework tests.

Set RUNKITE_TEST_BINARY, RUNKITE_TEST_SEED_BINARY and RUNKITE_TEST_RUNNER_WHEEL.
Optional RUNKITE_TEST_POSTGRES_DSN + RUNKITE_TEST_REDIS_URL select dedicated
external test services; otherwise this uses SQLite and the in-process queue.
"""

from __future__ import annotations

import contextlib
import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
import uuid
import zipfile
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import httpx

ROOT = Path(__file__).resolve().parents[3]
ADAPTER = Path(__file__).resolve().parent
TERMINAL = {"success", "error", "timeout", "interrupted"}


def wait_for(description: str, operation: Callable[[], Any], timeout: float = 25.0) -> Any:
    deadline = time.monotonic() + timeout
    last: Exception | None = None
    while time.monotonic() < deadline:
        try:
            value = operation()
            if value:
                return value
        except (httpx.HTTPError, AssertionError, OSError) as exc:
            last = exc
        time.sleep(0.04)
    raise AssertionError(f"Timed out waiting for {description}; last error: {last}")


def free_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


class Process:
    def __init__(self, command: list[str], directory: Path, environment: dict[str, str], name: str) -> None:
        self.path = directory / f"{name}.log"
        self.output = self.path.open("wb")
        self.child = subprocess.Popen(command, cwd=directory, env=environment, stdout=self.output, stderr=self.output)

    def stop(self, *, kill: bool = False) -> None:
        if self.child.poll() is None:
            self.child.kill() if kill else self.child.terminate()
            try:
                self.child.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.child.kill()
                self.child.wait(timeout=5)
        self.output.close()

    def logs(self) -> str:
        return self.path.read_text(errors="replace")[-64_000:]


@unittest.skipUnless(os.environ.get("RUNKITE_TEST_BINARY"), "Set RUNKITE_TEST_BINARY for real managed-runner E2E")
class ManagedAgentFrameworkE2E(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="runkite-ms-e2e-")
        self.directory = Path(self.temporary.name)
        self.processes: list[Process] = []
        self.relays: list[ThreadingHTTPServer] = []
        self.addCleanup(self.cleanup)
        postgres = os.environ.get("RUNKITE_TEST_POSTGRES_DSN")
        redis = os.environ.get("RUNKITE_TEST_REDIS_URL")
        self.assertEqual(bool(postgres), bool(redis), "Set both dedicated Postgres and Redis test URLs")
        self.profile = "Postgres + Redis" if postgres else "SQLite + in-process"
        self.http = httpx.Client(timeout=20, trust_env=False)
        prefix = uuid.uuid4().hex[:12]
        self.tenant_a = f"ms-e2e-a-{prefix}"
        self.tenant_b = f"ms-e2e-b-{prefix}"
        self.agent = f"ms_e2e_{prefix}"
        self.slow = f"ms_e2e_slow_{prefix}"
        self.counters = self.directory / "counters.jsonl"
        self.config = self.directory / "langgraph.json"
        example = json.loads((ROOT / "examples" / "managed_agent_framework" / "langgraph.json").read_text())
        self.config.write_text(
            json.dumps(
                {
                    "runner_kind": example["runner_kind"],
                    "graphs": {self.agent: "./agent.py:agent", self.slow: "./agent.py:slow_agent"},
                    "auth": {
                        "type": "api_key",
                        "keys": {
                            "e2e-admin": {"name": "admin", "permissions": ["admin"]},
                            "e2e-a": {"name": "a", "tenant_id": self.tenant_a, "permissions": ["read", "write"]},
                            "e2e-b": {"name": "b", "tenant_id": self.tenant_b, "permissions": ["read", "write"]},
                        },
                    },
                }
            )
        )
        shutil.copyfile(ADAPTER / "e2e_agent.py", self.directory / "agent.py")
        wheel_location = Path(os.environ["RUNKITE_TEST_RUNNER_WHEEL"])
        wheel = next(wheel_location.glob("runkite_runner-*.whl")) if wheel_location.is_dir() else wheel_location
        self.core = self.directory / "wheel-core"
        with zipfile.ZipFile(wheel) as archive:
            archive.extractall(self.core)
        self.assertTrue((self.core / "runkite_runner" / "generic_worker.py").is_file())
        http_port, grpc_port = free_port(), free_port()
        while grpc_port == http_port:
            grpc_port = free_port()
        self.base = f"http://127.0.0.1:{http_port}"
        self.grpc = f"127.0.0.1:{grpc_port}"
        self.environment = {
            key: os.environ[key] for key in ("PATH", "LANG", "TMPDIR", "SYSTEMROOT") if key in os.environ
        }
        self.environment.update(
            {
                "PYTHONDONTWRITEBYTECODE": "1",
                "DATABASE_PATH": str(self.directory / "state.db"),
                "LOG_FORMAT": "json",
                "RUNNER_TOKEN_PYTHON_AGENT_FRAMEWORK": "e2e-runner-token",
                "RUNNER_TENANTS_PYTHON_AGENT_FRAMEWORK": f"{self.tenant_a},{self.tenant_b}",
                "RUNKITE_MS_E2E_COUNTERS": str(self.counters),
            }
        )
        plane_environment = dict(self.environment)
        if postgres and redis:
            plane_environment.update(POSTGRES_DSN=postgres, REDIS_URL=redis)
        self.plane = self.launch(
            [
                str(Path(os.environ["RUNKITE_TEST_BINARY"]).resolve()),
                "dev",
                "--config",
                str(self.config),
                "--port",
                str(http_port),
                "--grpc-port",
                str(grpc_port),
            ],
            plane_environment,
        )
        wait_for("control-plane readiness", lambda: self.http.get(f"{self.base}/readyz").is_success)
        # Config bootstrap is deliberately default-tenant-only. Seed the actual
        # store through its public Go interface, without weakening HTTP auth.
        subprocess.run(
            [
                str(Path(os.environ["RUNKITE_TEST_SEED_BINARY"]).resolve()),
                str(self.config),
                f"{self.tenant_a},{self.tenant_b}",
            ],
            env=plane_environment,
            cwd=self.directory,
            check=True,
            capture_output=True,
            text=True,
            timeout=20,
        )
        self.worker_environment = {
            **self.environment,
            "PYTHONPATH": os.pathsep.join((str(self.core), str(ROOT / "python" / "adapters"))),
            "RUNNER_TOKEN": "e2e-runner-token",
        }
        imported = subprocess.check_output(
            [sys.executable, "-c", "import runkite_runner; print(runkite_runner.__file__)"],
            env=self.worker_environment,
            cwd=self.directory,
            text=True,
            timeout=10,
        ).strip()
        self.assertTrue(Path(imported).is_relative_to(self.core), "worker core must come from the built wheel")
        self.start_runner()

    def cleanup(self) -> None:
        for process in reversed(self.processes):
            process.stop()
        for relay in self.relays:
            relay.shutdown()
            relay.server_close()
        if hasattr(self, "http"):
            self.http.close()
        self.temporary.cleanup()

    def launch(self, command: list[str], environment: dict[str, str]) -> Process:
        process = Process(command, self.directory, environment, f"process-{len(self.processes)}")
        self.processes.append(process)
        return process

    def start_runner(self, http_address: str | None = None) -> None:
        self.runner = self.launch(
            [
                sys.executable,
                "-m",
                "agent_framework_adapter",
                "--config",
                str(self.config),
                "--grpc-address",
                self.grpc,
                "--http-address",
                http_address or self.base,
            ],
            {**self.worker_environment, "RUNKITE_HTTP_URL": http_address or self.base},
        )

        def ready() -> bool:
            self.assertIsNone(self.runner.child.poll(), self.runner.logs())
            return "Worker ready" in self.runner.logs()

        wait_for("authenticated runner startup", ready)

    def request(self, route: str, body: Any = None, *, key: str = "e2e-a", status: int = 200) -> Any:
        response = self.http.request(
            "GET" if body is None else "POST",
            self.base + route,
            headers={"Authorization": f"Bearer {key}"},
            json=body,
        )
        self.assertEqual(response.status_code, status, f"{route}: {response.text}")
        return response.json() if response.content else None

    def new_thread(self, *, key: str = "e2e-a") -> str:
        return self.request("/threads", {}, key=key)["thread_id"]

    def new_run(self, thread: str, text: str, *, agent: str | None = None, key: str = "e2e-a") -> dict:
        return self.request(
            f"/threads/{thread}/runs",
            {"agent_id": agent or self.agent, "input": {"messages": [{"role": "user", "content": text}]}},
            key=key,
        )

    def completed(self, thread: str, run_id: str, *, key: str = "e2e-a") -> dict:
        def result():
            run = self.request(f"/threads/{thread}/runs/{run_id}", key=key)
            return run if run["status"] in TERMINAL else False

        return wait_for("durable run completion", result)

    def records(self, run_id: str, kind: str) -> list[dict]:
        try:
            rows = [json.loads(line) for line in self.counters.read_text().splitlines() if line]
        except FileNotFoundError:
            return []
        return [row for row in rows if row["run_id"] == run_id and row["kind"] == kind]

    def usage(self, agent: str) -> tuple[int, int, int]:
        rows = self.request(f"/admin-api/usage/summary?agent_id={agent}", key="e2e-admin")
        return tuple(sum(row[field] for row in rows) for field in ("run_count", "tokens_in", "tokens_out"))

    def expect_usage(self, agent: str, expected: tuple[int, int, int]) -> None:
        wait_for("one durable usage settlement", lambda: self.usage(agent) == expected)

    def final_usage(self, run: dict, prompt: int, completion: int) -> None:
        self.assertEqual(run["status"], "success", json.dumps(run))
        usage = run["output"]["usage"]
        self.assertEqual((usage["prompt_tokens"], usage["completion_tokens"]), (prompt, completion))

    def wait_provisional_usage(self, thread: str, run_id: str) -> None:
        route = f"{self.base}/threads/{thread}/runs/{run_id}/stream"
        with self.http.stream("GET", route, headers={"Authorization": "Bearer e2e-a"}, timeout=10) as response:
            self.assertEqual(response.status_code, 200)
            method = ""
            for line in response.iter_lines():
                if line.startswith("event: "):
                    method = line[7:]
                elif method == "values" and line.startswith("data: "):
                    usage = json.loads(line[6:]).get("usage", {})
                    if (usage.get("prompt_tokens"), usage.get("completion_tokens")) == (10, 4):
                        return
        self.fail("the runner never published the first model step's provider usage")

    def crash_after_commit_proxy(self, thread: str) -> tuple[str, threading.Event, dict]:
        test = self
        committed = threading.Event()
        outcome: dict[str, Any] = {}
        state_path = f"/internal/checkpoints/{thread}/adapter-state"

        class Relay(BaseHTTPRequestHandler):
            def log_message(self, *_args: Any) -> None:
                pass

            def forward(self) -> None:
                try:
                    body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                    headers = {
                        key: value
                        for key, value in self.headers.items()
                        if key.lower() not in {"host", "connection", "content-length", "transfer-encoding"}
                    }
                    with httpx.Client(timeout=20, trust_env=False) as client:
                        upstream = client.request(self.command, test.base + self.path, headers=headers, content=body)
                    if self.command == "PUT" and self.path == state_path and upstream.is_success:
                        outcome.update(run_id=self.headers.get("X-Runkite-Run-Id"), receipt=json.loads(body))
                        # The real plane has committed, but the actual worker has
                        # not received its PUT acknowledgement or sent terminal status.
                        test.runner.stop(kill=True)
                        self.close_connection = True
                        # Killing the worker can close its side first. Only
                        # suppress errors from this intentional socket cleanup.
                        with contextlib.suppress(OSError):
                            self.connection.shutdown(socket.SHUT_RDWR)
                        with contextlib.suppress(OSError):
                            self.connection.close()
                        committed.set()
                        return
                    self.send_response(upstream.status_code)
                    for key, value in upstream.headers.items():
                        if key.lower() not in {"content-length", "transfer-encoding", "connection", "content-encoding"}:
                            self.send_header(key, value)
                    self.send_header("Content-Length", str(len(upstream.content)))
                    self.end_headers()
                    self.wfile.write(upstream.content)
                except Exception as exc:
                    outcome["error"] = repr(exc)
                    committed.set()
                    self.close_connection = True

            do_GET = do_POST = do_PUT = forward

        relay = ThreadingHTTPServer(("127.0.0.1", 0), Relay)
        self.relays.append(relay)
        threading.Thread(target=relay.serve_forever, daemon=True).start()
        return f"http://127.0.0.1:{relay.server_port}", committed, outcome

    def test_managed_lifecycle(self) -> None:
        scenario = "authenticated dispatch"
        try:
            self.assertEqual(self.http.get(f"{self.base}/admin-api/runs").status_code, 401)
            thread = self.new_thread()
            first = self.new_run(thread, "tenant-a-first-turn")["run_id"]
            final = self.completed(thread, first)
            self.final_usage(final, 20, 4)
            self.assertIn("users=1; tool_results=1", json.dumps(final["output"]["messages"]))
            self.assertEqual(len(self.records(first, "factory")), 1)
            self.assertEqual(len(self.records(first, "model")), 2)
            self.assertEqual(len(self.records(first, "tool")), 1)
            self.assertEqual(self.records(first, "factory")[0]["tenant_id"], self.tenant_a)
            self.assertEqual(self.request(f"/threads/{thread}")["values"], final["output"])
            self.assertEqual(self.request(f"/admin-api/runs/{first}", key="e2e-admin")["status"], "success")
            self.assertTrue(
                any(row["thread_id"] == thread for row in self.request("/admin-api/threads", key="e2e-admin"))
            )
            replay = self.http.get(
                f"{self.base}/threads/{thread}/runs/{first}/stream", headers={"Authorization": "Bearer e2e-a"}
            )
            self.assertEqual(replay.status_code, 200)
            self.assertIn("event: messages", replay.text)
            self.assertIn("event: end", replay.text)
            self.assertIn("users=1; tool_results=1", replay.text)
            self.expect_usage(self.agent, (1, 20, 4))
            print(f"PASS {self.profile}: authenticated dispatch, stream, Admin and provider usage", flush=True)

            scenario = "native session after worker restart"
            self.runner.stop()
            self.start_runner()
            second = self.new_run(thread, "tenant-a-second-turn")["run_id"]
            final = self.completed(thread, second)
            self.final_usage(final, 20, 4)
            restored = self.records(second, "model")[0]
            self.assertEqual(restored["users"], ["tenant-a-first-turn", "tenant-a-second-turn"])
            self.assertGreaterEqual(restored["tool_results"], 1)
            self.expect_usage(self.agent, (2, 40, 8))
            print(f"PASS {self.profile}: native session and tool history across restart", flush=True)

            scenario = "authenticated tenant isolation"
            self.request(f"/threads/{thread}", key="e2e-b", status=404)
            self.request(f"/threads/{thread}/runs/{first}", key="e2e-b", status=404)
            other_thread = self.new_thread(key="e2e-b")
            other = self.new_run(other_thread, "tenant-b-only-turn", key="e2e-b")["run_id"]
            self.final_usage(self.completed(other_thread, other, key="e2e-b"), 20, 4)
            self.assertEqual(self.records(other, "model")[0]["users"], ["tenant-b-only-turn"])
            self.assertEqual(self.records(other, "factory")[0]["tenant_id"], self.tenant_b)
            self.expect_usage(self.agent, (3, 60, 12))
            print(f"PASS {self.profile}: nondefault tenants isolated through real worker HTTP", flush=True)

            scenario = "cancel retains observed usage once"
            slow_thread = self.new_thread()
            slow_run = self.new_run(slow_thread, "cancel-after-first-tool", agent=self.slow)["run_id"]
            wait_for("second model step stalled", lambda: self.records(slow_run, "stalled"))
            self.wait_provisional_usage(slow_thread, slow_run)
            self.request(f"/threads/{slow_thread}/runs/{slow_run}/cancel", {})
            canceled = self.completed(slow_thread, slow_run)
            self.assertEqual(canceled["status"], "interrupted")
            wait_for("native model cancellation", lambda: self.records(slow_run, "canceled"))
            usage = canceled["output"]["usage"]
            self.assertEqual((usage["prompt_tokens"], usage["completion_tokens"]), (10, 4))
            self.assertEqual(len(self.records(slow_run, "tool")), 1)
            self.expect_usage(self.slow, (1, 10, 4))
            self.request(f"/threads/{slow_thread}/runs/{slow_run}/wait")
            self.expect_usage(self.slow, (1, 10, 4))
            print(f"PASS {self.profile}: cancel reaches model and retains observed 10/4 usage once", flush=True)

            scenario = "crash after PUT commits before response"
            recovery_thread = self.new_thread()
            self.runner.stop()
            proxy, committed, outcome = self.crash_after_commit_proxy(recovery_thread)
            self.start_runner(proxy)
            recovered = self.new_run(recovery_thread, "crash-after-committed-tool")["run_id"]
            self.assertTrue(committed.wait(25), "no checkpoint commit observed before crash")
            self.assertNotIn("error", outcome)
            self.assertEqual(outcome["run_id"], recovered)
            self.assertEqual(outcome["receipt"]["completed"]["run_id"], recovered)
            self.assertNotEqual(self.request(f"/threads/{recovery_thread}/runs/{recovered}")["status"], "success")
            self.start_runner()
            final = self.completed(recovery_thread, recovered)
            self.final_usage(final, 20, 4)
            self.assertGreaterEqual(final["metadata"]["_runkite_attempt_generation"], 2)
            self.assertGreaterEqual(final["metadata"]["_runkite_completion_generation"], 2)
            self.assertEqual(
                final["output"]["agent_framework"]["generation"], final["metadata"]["_runkite_completion_generation"]
            )
            self.assertEqual(len(self.records(recovered, "factory")), 1, "receipt replay repeated factory work")
            self.assertEqual(len(self.records(recovered, "model")), 2, "receipt replay repeated model work")
            self.assertEqual(len(self.records(recovered, "tool")), 1, "receipt replay repeated the tool")
            self.assertEqual(self.request(f"/threads/{recovery_thread}")["values"], final["output"])
            self.expect_usage(self.agent, (4, 80, 16))
            self.request(f"/threads/{recovery_thread}/runs/{recovered}/wait")
            self.expect_usage(self.agent, (4, 80, 16))
            print(
                f"PASS {self.profile}: advanced-generation receipt recovery without duplicate work or billing",
                flush=True,
            )
        except Exception:
            print(f"FAILED scenario: {scenario}", file=sys.stderr)
            for process in self.processes:
                print(process.logs(), file=sys.stderr)
            raise


if __name__ == "__main__":
    unittest.main()
