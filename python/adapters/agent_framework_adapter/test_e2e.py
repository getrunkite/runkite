"""Opt-in real plane and fully installed managed Agent Framework runner tests.

Set RUNKITE_TEST_BINARY, RUNKITE_TEST_SEED_BINARY and RUNKITE_TEST_RUNNER_WHEEL.
Optional RUNKITE_TEST_POSTGRES_DSN + RUNKITE_TEST_REDIS_URL select dedicated
external test services; otherwise this uses SQLite and the in-process queue.
The harness installs the wheel's agent-framework extra into a fresh consumer
venv with uv. UV_CACHE_DIR and UV_OFFLINE are honored for dependency preparation.
The Microsoft profile uses fictional USD rates to prove real spend settlement
and admission caps, without contacting a provider.
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
    adapter_package = "agent_framework_adapter"
    sdk_module = "agent_framework"
    sdk_distribution = "agent-framework-core"
    sdk_version = "1.23.0"
    install_extra = "agent-framework"
    runner_command_name = "runkite-runner-agent-framework"
    runner_description = "Runkite managed Microsoft Agent Framework runner"
    example_directory = "managed_agent_framework"
    fixture = ADAPTER / "e2e_agent.py"
    runner_kind_env = "PYTHON_AGENT_FRAMEWORK"
    counters_env = "RUNKITE_MS_E2E_COUNTERS"
    metadata_key = "agent_framework"
    fixture_label = "ms"
    extra_graph_factories: tuple[str, ...] = (
        "error_agent",
        "budget_agent",
        "allowed_agent",
        "blocked_agent",
        "deny_all_agent",
    )
    extra_runner_args: tuple[str, ...] = ()
    extra_worker_environment: dict[str, str] = {}
    extra_test_requirements: tuple[str, ...] = ()

    def additional_configuration(self) -> dict[str, Any]:
        return {
            "allowed_tools": {
                self.extra_graphs["allowed_agent"]: ["record_effect"],
                self.extra_graphs["blocked_agent"]: ["a_different_tool"],
                self.extra_graphs["deny_all_agent"]: [],
            },
            "finops": {
                # Fictional prices, deliberately unrelated to provider prices.
                "pricebook": {"offline-managed-ms-e2e": {"input_per_1k": 1, "output_per_1k": 2}},
                "budgets": {
                    "agents": {f"{self.tenant_a}/{self.extra_graphs['budget_agent']}": {"max_usd_per_day": 0.02}}
                },
            },
        }

    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix=f"runkite-{self.fixture_label}-e2e-")
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
        # Exercise encoded identity headers in every managed SDK profile
        # that inherits this installed-wheel harness.
        self.tenant_a = f"{self.fixture_label}-e2e-a-雪-{prefix}"
        self.tenant_b = f"{self.fixture_label}-e2e-b-{prefix}"
        self.agent = f"{self.fixture_label}_e2e_{prefix}"
        self.slow = f"{self.fixture_label}_e2e_slow_{prefix}"
        self.extra_graphs = {name: f"{self.fixture_label}_{name}_{prefix}" for name in self.extra_graph_factories}
        self.counters = self.directory / "counters.jsonl"
        self.config = self.directory / "langgraph.json"
        example = json.loads((ROOT / "examples" / self.example_directory / "langgraph.json").read_text())
        self.config.write_text(
            json.dumps(
                {
                    "runner_kind": example["runner_kind"],
                    "graphs": {
                        self.agent: "./agent.py:agent",
                        self.slow: "./agent.py:slow_agent",
                        **{graph: f"./agent.py:{name}" for name, graph in self.extra_graphs.items()},
                    },
                    "auth": {
                        "type": "api_key",
                        "keys": {
                            "e2e-admin": {"name": "admin", "permissions": ["admin"]},
                            "e2e-a": {"name": "a", "tenant_id": self.tenant_a, "permissions": ["read", "write"]},
                            "e2e-b": {"name": "b", "tenant_id": self.tenant_b, "permissions": ["read", "write"]},
                        },
                    },
                    **self.additional_configuration(),
                }
            )
        )
        shutil.copyfile(self.fixture, self.directory / "agent.py")
        wheel_location = Path(os.environ["RUNKITE_TEST_RUNNER_WHEEL"]).resolve()
        wheel = next(wheel_location.glob("runkite_runner-*.whl")) if wheel_location.is_dir() else wheel_location
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
                f"RUNNER_TOKEN_{self.runner_kind_env}": "e2e-runner-token",
                f"RUNNER_TENANTS_{self.runner_kind_env}": f"{self.tenant_a},{self.tenant_b}",
                self.counters_env: str(self.counters),
            }
        )
        self.worker_environment = {
            **self.environment,
            **self.extra_worker_environment,
            "RUNNER_TOKEN": "e2e-runner-token",
        }
        self.install_runner(wheel)
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
        self.start_runner()

    def install_runner(self, wheel: Path) -> None:
        self.assertTrue(wheel.is_file(), f"Missing built wheel: {wheel}")
        self.consumer = self.directory / "consumer"
        subprocess.run(
            [sys.executable, "-I", "-m", "venv", "--without-pip", str(self.consumer)],
            env=self.environment,
            cwd=self.directory,
            check=True,
            capture_output=True,
            text=True,
            timeout=30,
        )
        scripts = self.consumer / ("Scripts" if os.name == "nt" else "bin")
        self.consumer_python = scripts / ("python.exe" if os.name == "nt" else "python")
        self.runner_command = scripts / (self.runner_command_name + (".exe" if os.name == "nt" else ""))
        installer_environment = {
            **self.environment,
            **{key: os.environ[key] for key in ("UV_CACHE_DIR", "UV_OFFLINE") if key in os.environ},
            "UV_PYTHON_DOWNLOADS": "never",
        }

        def install(*requirements: str) -> None:
            installed = subprocess.run(
                ["uv", "--no-config", "pip", "install", "--python", str(self.consumer_python), *requirements],
                env=installer_environment,
                cwd=self.directory,
                capture_output=True,
                text=True,
                timeout=240,
            )
            self.assertEqual(
                installed.returncode, 0, f"Fresh consumer installation failed:\n{installed.stdout}{installed.stderr}"
            )

        install(f"runkite-runner[{self.install_extra}] @ {wheel.as_uri()}")
        self.assertNotIn("PYTHONPATH", self.worker_environment)
        probe = subprocess.check_output(
            [
                str(self.consumer_python),
                "-I",
                "-c",
                "import importlib.metadata as m, json, sys; "
                f"import {self.sdk_module}, {self.adapter_package}, runkite_runner; "
                "print(json.dumps({'prefix': sys.prefix, 'path': sys.path, "
                "'modules': {x.__name__: x.__file__ for x in "
                f"(runkite_runner, {self.adapter_package}, {self.sdk_module})}}, "
                f"'sdk': m.version({self.sdk_distribution!r}), "
                "'entrypoint': {e.name: e.value for e in m.distribution('runkite-runner').entry_points}"
                "}))",
            ],
            env=self.worker_environment,
            cwd=self.directory,
            text=True,
            timeout=20,
        )
        details = json.loads(probe)
        self.assertEqual(Path(details["prefix"]).resolve(), self.consumer.resolve())
        for name, source in details["modules"].items():
            self.assertTrue(
                Path(source).resolve().is_relative_to(self.consumer.resolve()),
                f"{name} must resolve inside the fresh consumer, got {source}",
            )
        for directory in details["path"]:
            self.assertFalse(Path(directory).resolve().is_relative_to(ROOT), f"Checkout import path: {directory}")
        self.assertEqual(details["sdk"], self.sdk_version)
        self.assertEqual(details["entrypoint"][self.runner_command_name], f"{self.adapter_package}.__main__:main")
        self.assertTrue(self.runner_command.is_file(), "The wheel must install its dedicated console command")
        for command in (
            [str(self.runner_command), "--help"],
            [str(self.consumer_python), "-I", "-m", self.adapter_package, "--help"],
        ):
            help_text = subprocess.check_output(
                command, env=self.worker_environment, cwd=self.directory, text=True, timeout=20
            )
            self.assertIn(self.runner_description, help_text)
        self.check_core_install()
        if self.extra_test_requirements:
            install(*self.extra_test_requirements)

    def check_core_install(self) -> None:
        """Profile-specific assertions before app-owned test providers install."""

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
                str(self.runner_command),
                "--config",
                str(self.config),
                "--grpc-address",
                self.grpc,
                "--http-address",
                http_address or self.base,
                *self.extra_runner_args,
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
        if self.install_extra == "agent-framework":
            self.assertEqual(usage["model"], "offline-managed-ms-e2e")

    def expect_cost(self, agent: str, expected: float) -> None:
        rows = self.request(f"/admin-api/usage/summary?agent_id={agent}", key="e2e-admin")
        self.assertAlmostEqual(sum(row["usd_estimate"] for row in rows), expected, places=9)

    def wait_provisional_usage(self, thread: str, run_id: str, *, key: str = "e2e-a") -> None:
        route = f"{self.base}/threads/{thread}/runs/{run_id}/stream"
        with self.http.stream("GET", route, headers={"Authorization": f"Bearer {key}"}, timeout=10) as response:
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

    @unittest.skipUnless(
        os.environ.get("RUNKITE_TEST_CHAT_BRIDGE"), "Set RUNKITE_TEST_CHAT_BRIDGE for native UI transport"
    )
    def test_managed_chat_bridge(self) -> None:
        """A fresh test namespace keeps lifecycle accounting assertions intact."""
        try:
            node = shutil.which("node")
            self.assertIsNotNone(node, "Native UI transport checks require Node 22.13+")
            helper = ROOT / "examples/managed_ai_sdk/python-bridge.e2e-helper.ts"
            self.assertTrue(helper.is_file(), "Missing native UI transport helper")

            recovery_thread = self.new_thread()
            self.runner.stop()
            proxy, committed, outcome = self.crash_after_commit_proxy(recovery_thread)
            self.start_runner(proxy)
            recovered = self.new_run(recovery_thread, "bridge crash after committed tool")["run_id"]
            self.assertTrue(committed.wait(25), "no checkpoint commit observed before bridge recovery")
            self.assertNotIn("error", outcome)
            self.assertEqual(outcome["run_id"], recovered)
            self.assertEqual(outcome["receipt"]["completed"]["run_id"], recovered)
            self.assertNotEqual(self.request(f"/threads/{recovery_thread}/runs/{recovered}")["status"], "success")
            self.start_runner()
            final = self.completed(recovery_thread, recovered)
            self.final_usage(final, 20, 4)
            generation = final["metadata"]["_runkite_completion_generation"]
            self.assertGreaterEqual(generation, 2)
            self.assertEqual(final["output"][self.metadata_key]["generation"], generation)
            configuration = {
                "baseUrl": self.base,
                "apiKey": "e2e-a",
                "profile": self.install_extra,
                "agentId": self.agent,
                "slowAgentId": self.slow,
                "tenantId": self.tenant_a,
                "recovered": {"threadId": recovery_thread, "runId": recovered, "generation": generation},
            }
            if self.install_extra == "pydantic-ai":
                configuration["typedAgentId"] = self.extra_graphs["typed_output_only"]
            native = subprocess.run(
                [node, "--experimental-strip-types", str(helper)],
                input=json.dumps(configuration),
                cwd=helper.parent,
                env=self.environment,
                text=True,
                capture_output=True,
                timeout=120,
            )
            self.assertEqual(native.returncode, 0, f"Native UI transport failed:\n{native.stdout}{native.stderr}")
            summary = json.loads(native.stdout)
            self.assertEqual(summary["recoveredRunId"], recovered)
            for run_id in (summary["firstRunId"], summary["secondRunId"], recovered):
                self.assertEqual(len(self.records(run_id, "factory")), 1, "UI attach constructed another agent")
                self.assertEqual(len(self.records(run_id, "model")), 2, "UI attach repeated model work")
                self.assertEqual(len(self.records(run_id, "tool")), 1, "UI attach repeated a tool effect")
            slow_run = summary["slowRunId"]
            wait_for("native cancellation after app Stop", lambda: self.records(slow_run, "canceled"))
            self.assertEqual(len(self.records(slow_run, "factory")), 1)
            self.assertEqual(len(self.records(slow_run, "model")), 2)
            self.assertEqual(len(self.records(slow_run, "tool")), 1)
            self.expect_usage(self.agent, (3, 60, 12))
            self.expect_usage(self.slow, (1, 10, 4))
            if self.install_extra == "pydantic-ai":
                typed_run = summary["typedRunId"]
                self.assertEqual(len(self.records(typed_run, "factory")), 1)
                self.assertEqual(len(self.records(typed_run, "model")), 2)
                self.assertFalse(self.records(typed_run, "tool"))
                self.expect_usage(self.extra_graphs["typed_output_only"], (1, 20, 4))
            print(
                f"PASS {self.profile}: native UI text, history, replay, detach/Stop and generation refusal without repeat work",
                flush=True,
            )
            if self.install_extra == "pydantic-ai":
                print(
                    f"PASS {self.profile}: native UI typed-only durable JSON without invented assistant text",
                    flush=True,
                )
        except Exception:
            for process in self.processes:
                print(process.logs(), file=sys.stderr)
            raise

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
            if self.install_extra == "agent-framework":
                self.assertEqual(usage["model"], "offline-managed-ms-e2e")
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
                final["output"][self.metadata_key]["generation"], final["metadata"]["_runkite_completion_generation"]
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
            # The OpenAI subclass retains these five shared scenarios and
            # supplies its own additional graphs and pricing assertions.
            if self.install_extra == "agent-framework":
                scenario = "Microsoft fictional pricing and USD hard cap"
                self.check_microsoft_pricing_and_cap()
                scenario = "Microsoft pre-execution tool allowlist and committed history"
                self.check_microsoft_tool_allowlist()
        except Exception:
            print(f"FAILED scenario: {scenario}", file=sys.stderr)
            for process in self.processes:
                print(process.logs(), file=sys.stderr)
            raise

    def check_microsoft_tool_allowlist(self) -> None:
        allowed_graph = self.extra_graphs["allowed_agent"]
        allowed_thread = self.new_thread()
        allowed_run = self.new_run(allowed_thread, "explicitly-permitted-tool", agent=allowed_graph)["run_id"]
        self.final_usage(self.completed(allowed_thread, allowed_run), 20, 4)
        self.assertEqual(len(self.records(allowed_run, "factory")), 1)
        self.assertEqual(len(self.records(allowed_run, "model")), 2)
        self.assertEqual(len(self.records(allowed_run, "tool")), 1)
        self.expect_usage(allowed_graph, (1, 20, 4))
        self.expect_cost(allowed_graph, 0.028)

        for name in ("blocked_agent", "deny_all_agent"):
            graph = self.extra_graphs[name]
            thread = self.new_thread()
            baseline = self.new_run(thread, "remember-before-denial", agent=graph)["run_id"]
            baseline_run = self.completed(thread, baseline)
            self.final_usage(baseline_run, 10, 2)
            saved_messages = baseline_run["output"]["messages"]
            self.assertFalse(self.records(baseline, "tool"))

            rejected_text = f"do-not-execute-{name}"
            denied = self.new_run(thread, rejected_text, agent=graph)["run_id"]
            final = self.completed(thread, denied)
            self.assertEqual(final["status"], "error", json.dumps(final))
            self.assertEqual(len(self.records(denied, "factory")), 1)
            self.assertEqual(len(self.records(denied, "model")), 1)
            self.assertFalse(self.records(denied, "tool"), f"{name} allowed a local function effect")
            usage = final["output"]["usage"]
            self.assertEqual((usage["prompt_tokens"], usage["completion_tokens"]), (10, 2))
            self.assertEqual(usage["model"], "offline-managed-ms-e2e")
            self.assertEqual(self.request(f"/threads/{thread}")["values"]["messages"], saved_messages)

            replay = self.http.get(
                f"{self.base}/threads/{thread}/runs/{denied}/stream",
                headers={"Authorization": "Bearer e2e-a"},
            )
            self.assertEqual(replay.status_code, 200)
            auth_events = []
            method = ""
            for line in replay.text.splitlines():
                if line.startswith("event: "):
                    method = line[7:]
                elif method == "tool_auth" and line.startswith("data: "):
                    auth_events.append(json.loads(line[6:]))
            self.assertEqual(len(auth_events), 1, replay.text)
            denial = auth_events[0]
            self.assertEqual(denial["stage"], "tool.call")
            self.assertEqual(denial["effect"], "deny")
            self.assertEqual(denial["tool"], "record_effect")
            self.assertEqual(denial["reason_code"], "tool_not_allowed")
            self.assertEqual(denial["generation"], final["metadata"]["_runkite_completion_generation"])
            self.expect_usage(graph, (2, 20, 4))
            self.expect_cost(graph, 0.028)
            self.request(f"/threads/{thread}/runs/{denied}/wait")
            self.expect_usage(graph, (2, 20, 4))
            self.expect_cost(graph, 0.028)

            restored = self.new_run(thread, "read-after-denial", agent=graph)["run_id"]
            self.final_usage(self.completed(thread, restored), 10, 2)
            self.assertEqual(
                self.records(restored, "model")[0]["users"], ["remember-before-denial", "read-after-denial"]
            )
            self.assertFalse(self.records(restored, "tool"))
            self.expect_usage(graph, (3, 30, 6))
            self.expect_cost(graph, 0.042)
        print(
            f"PASS {self.profile}: Microsoft allowlists and explicit deny-all prevent effects, preserve history and settle denial usage once",
            flush=True,
        )

    def check_microsoft_pricing_and_cap(self) -> None:
        self.expect_cost(self.agent, 0.112)  # Four successful 20/4 turns, including receipt recovery.
        self.expect_cost(self.slow, 0.018)  # One interrupted 10/4 turn.
        error_agent = self.extra_graphs["error_agent"]
        failed_thread = self.new_thread()
        failed_run = self.new_run(failed_thread, "error-after-first-tool", agent=error_agent)["run_id"]
        failed = self.completed(failed_thread, failed_run)
        self.assertEqual(failed["status"], "error", json.dumps(failed))
        usage = failed["output"]["usage"]
        self.assertEqual((usage["prompt_tokens"], usage["completion_tokens"]), (10, 4))
        self.assertEqual(usage["model"], "offline-managed-ms-e2e")
        self.assertEqual(len(self.records(failed_run, "factory")), 1)
        self.assertEqual(len(self.records(failed_run, "model")), 2)
        self.assertEqual(len(self.records(failed_run, "tool")), 1)
        self.assertEqual(len(self.records(failed_run, "failed")), 1)
        self.expect_usage(error_agent, (1, 10, 4))
        self.expect_cost(error_agent, 0.018)
        self.request(f"/threads/{failed_thread}/runs/{failed_run}/wait")
        self.expect_usage(error_agent, (1, 10, 4))
        self.expect_cost(error_agent, 0.018)
        print(f"PASS {self.profile}: Microsoft success/cancel/error/receipt fictional USD settles once", flush=True)

        budget_agent = self.extra_graphs["budget_agent"]
        cap_key = f"{self.tenant_a}/{budget_agent}"
        configured = self.request("/admin-api/finops", key="e2e-admin")["effective"]
        self.assertEqual(configured["budgets"]["agents"][cap_key]["max_usd_per_day"], 0.02)
        budget_thread = self.new_thread()
        first_run = self.new_run(budget_thread, "spend-before-hard-cap", agent=budget_agent)["run_id"]
        self.final_usage(self.completed(budget_thread, first_run), 20, 4)
        self.expect_usage(budget_agent, (1, 20, 4))
        self.expect_cost(budget_agent, 0.028)
        self.assertEqual(len(self.records(first_run, "factory")), 1)
        self.assertEqual(len(self.records(first_run, "model")), 2)
        self.assertEqual(len(self.records(first_run, "tool")), 1)
        denied_thread = self.new_thread()
        before = self.counters.read_bytes()
        denied = self.request(
            f"/threads/{denied_thread}/runs",
            {"agent_id": budget_agent, "input": {"messages": [{"role": "user", "content": "must-not-execute"}]}},
            status=403,
        )
        self.assertIn("usd budget exceeded", json.dumps(denied).lower())
        self.assertEqual(self.request(f"/threads/{denied_thread}/runs"), [])
        self.assertEqual(self.counters.read_bytes(), before, "a denied create reached the SDK")
        self.expect_usage(budget_agent, (1, 20, 4))
        self.expect_cost(budget_agent, 0.028)
        alerts = self.request(
            f"/admin-api/usage/alerts?tenant_id={self.tenant_a}&agent_id={budget_agent}", key="e2e-admin"
        )
        self.assertTrue(any(row["reason_code"] == "budget_exceeded" for row in alerts))
        print(f"PASS {self.profile}: settled Microsoft USD spend blocks a later create at its hard cap", flush=True)


if __name__ == "__main__":
    unittest.main()
