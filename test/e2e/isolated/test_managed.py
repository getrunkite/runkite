"""Opt-in installed parent worker, real SQL plane and external OpenShell.

The gateway/image must be disposable and explicitly supplied. No model provider
is called. All tool effects and credentials are synthetic and locally owned.
"""

from __future__ import annotations

import asyncio
import json
import os
import secrets
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

import httpx

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "python/adapters/agent_framework_adapter"))
from test_e2e import Process, free_port, wait_for  # noqa: E402


@unittest.skipUnless(os.environ.get("RUNKITE_TEST_ISOLATED") == "1", "explicit disposable runtime opt-in required")
class ManagedIsolatedE2E(unittest.TestCase):
    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(prefix="runkite-isolated-e2e-"))
        self.processes = []
        self.servers = []
        self.runtime_names = set()
        self.evidence = {
            "passed": False,
            "scope": "real SQLite plane + installed trusted parent + external OpenShell",
            "cases": [],
        }
        self.addCleanup(self.cleanup)
        self.gateway = os.environ["RUNKITE_TEST_ISOLATED_GATEWAY"]
        self.assertIn(
            urlparse(self.gateway).hostname, {"127.0.0.1", "::1"}, "only a dedicated loopback gateway is allowed"
        )
        self.cli = str(Path(os.environ["RUNKITE_TEST_ISOLATED_CLI"]).resolve())
        self.image = os.environ["RUNKITE_TEST_ISOLATED_IMAGE"]
        self.api_key, self.runner_token, self.downstream_secret = (secrets.token_urlsafe(32) for _ in range(3))
        self.effects = 0
        self.lock = threading.Lock()
        test = self

        class Tool(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def do_POST(self):
                request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
                if self.headers.get("Authorization") != "Bearer " + test.downstream_secret:
                    self.send_error(401)
                    return
                if request.get("method") == "tools/call":
                    with test.lock:
                        test.effects += 1
                body = json.dumps(
                    {
                        "jsonrpc": "2.0",
                        "id": request.get("id"),
                        "result": {"content": [{"type": "text", "text": "synthetic effect accepted"}]},
                    }
                ).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        target = ThreadingHTTPServer(("127.0.0.1", 0), Tool)
        self.servers.append(target)
        threading.Thread(target=target.serve_forever, daemon=True).start()
        self.base, self.grpc = f"http://127.0.0.1:{free_port()}", f"127.0.0.1:{free_port()}"
        self.http = httpx.Client(timeout=120, trust_env=False)
        self.policy = ROOT / "examples/isolated_agent/policy.yaml"
        self.config = self.directory / "langgraph.json"
        self.config.write_text(
            json.dumps(
                {
                    "runner_kind": "python-isolated",
                    "execution_pool": "fixture",
                    "execution_pools": {
                        "fixture": {
                            "runner_kind": "python-isolated",
                            "tenants": ["default"],
                            "agents": ["isolated_echo"],
                        }
                    },
                    "graphs": {"isolated_echo": "workload.py:main"},
                    "auth": {
                        "type": "api_key",
                        "keys": {
                            "${RUNKITE_FIXTURE_API_KEY}": {
                                "name": "fixture-admin",
                                "tenant_id": "default",
                                "permissions": ["admin"],
                            }
                        },
                    },
                    "connectors": {
                        "fixture": {
                            "auth": {"type": "bearer", "bearer_token": "${FIXTURE_DOWNSTREAM_SECRET}"},
                            "mcp": {"url": f"http://127.0.0.1:{target.server_port}/mcp"},
                        }
                    },
                    "policy": {
                        "audit_required": True,
                        "action_evidence": True,
                        "grants": [
                            {
                                "tenant_id": "default",
                                "agent_id": "isolated_echo",
                                "connector": "fixture",
                                "tools": {"allow": ["write"]},
                            }
                        ],
                    },
                    "reclaim": {"max_age": "3s", "max_retries": 3},
                    "isolated_execution": {
                        "provider": "openshell",
                        "cli": self.cli,
                        "gateway_endpoint": self.gateway,
                        "image": self.image,
                        "policy_file": str(self.policy),
                        "command": ["python3", "/app/workload.py"],
                    },
                }
            )
        )
        self.environment = {
            key: value
            for key, value in os.environ.items()
            if key
            not in {
                "PYTHONPATH",
                "POSTGRES_DSN",
                "MYSQL_DSN",
                "MONGO_URI",
                "REDIS_URL",
                "NATS_URL",
                "KAFKA_URL",
                "DATABASE_PATH",
                "LANGGRAPH_CONFIG",
                "RUNNER_TOKEN",
            }
            and not key.startswith("RUNNER_TOKEN_")
            and not key.startswith("RUNNER_TENANTS_")
        }
        self.plane_environment = {
            **self.environment,
            "DATABASE_PATH": str(self.directory / "state.db"),
            "RUNKITE_FIXTURE_API_KEY": self.api_key,
            "RUNNER_TOKEN_POOL_FIXTURE": self.runner_token,
            "FIXTURE_DOWNSTREAM_SECRET": self.downstream_secret,
            "LOG_FORMAT": "json",
        }
        self.worker_environment = {
            **self.environment,
            "RUNNER_TOKEN": self.runner_token,
            "RUNKITE_EXECUTION_POOL": "fixture",
        }
        self.install_parent()
        self.plane = self.launch(
            [
                os.environ["RUNKITE_TEST_BINARY"],
                "dev",
                "--config",
                str(self.config),
                "--port",
                self.base.rsplit(":", 1)[1],
                "--grpc-port",
                self.grpc.rsplit(":", 1)[1],
            ],
            self.plane_environment,
        )
        wait_for("isolated plane readiness", lambda: self.http.get(self.base + "/readyz").is_success)
        self.start_parent()

    def install_parent(self):
        wheel = Path(os.environ["RUNKITE_TEST_RUNNER_WHEEL"]).resolve()
        self.assertTrue(wheel.is_file())
        consumer = self.directory / "consumer"
        subprocess.run(
            [sys.executable, "-I", "-m", "venv", "--without-pip", str(consumer)],
            check=True,
            capture_output=True,
            timeout=30,
        )
        python = consumer / "bin/python"
        result = subprocess.run(
            ["uv", "--no-config", "pip", "install", "--python", str(python), f"runkite-runner @ {wheel.as_uri()}"],
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=240,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.parent_command = consumer / "bin/runkite-runner-isolated"
        self.assertTrue(self.parent_command.is_file())
        location = subprocess.check_output(
            [str(python), "-I", "-c", "import runkite_runner.isolated;print(runkite_runner.isolated.__file__)"],
            cwd=self.directory,
            env=self.worker_environment,
            text=True,
        ).strip()
        self.assertTrue(Path(location).is_relative_to(consumer), "trusted parent must come from the installed wheel")

    def launch(self, command, environment):
        process = Process(
            [str(value) for value in command], self.directory, environment, f"process-{len(self.processes)}"
        )
        self.processes.append(process)
        return process

    def start_parent(self, address=None):
        self.parent = self.launch(
            [
                self.parent_command,
                "--config",
                self.config,
                "--grpc-address",
                self.grpc,
                "--http-address",
                address or self.base,
                "--execution-pool",
                "fixture",
            ],
            self.worker_environment,
        )
        wait_for(
            "installed isolated worker ready",
            lambda: self.parent.child.poll() is None and "Worker ready" in self.parent.logs(),
            timeout=45,
        )

    def request(self, path, body=None):
        response = self.http.request(
            "GET" if body is None else "POST",
            self.base + path,
            headers={"Authorization": "Bearer " + self.api_key},
            json=body,
        )
        self.assertTrue(response.is_success, f"{path}: {response.status_code} {response.text}")
        return response.json() if response.content else None

    def thread(self):
        return self.request("/threads", {})["thread_id"]

    def create_run(self, thread, value):
        return self.request(f"/threads/{thread}/runs", {"agent_id": "isolated_echo", "input": value})["run_id"]

    def final(self, thread, run):
        def done():
            value = self.request(f"/threads/{thread}/runs/{run}")
            return value if value["status"] in {"success", "error", "timeout", "interrupted"} else None

        return wait_for("isolated durable result", done, timeout=120)

    def state(self, thread, run, generation=1):
        response = self.http.get(
            self.base + f"/internal/checkpoints/{thread}/adapter-state",
            headers={
                "X-Runner-Kind": "pool-fixture",
                "X-Runner-Token": self.runner_token,
                "X-Runkite-Run-Id": run,
                "X-Runkite-Generation": str(generation),
            },
        )
        return response.json() if response.status_code == 200 else None

    def relay_after_receipt(self, thread):
        test = self
        committed = threading.Event()
        observed = {}

        class Relay(BaseHTTPRequestHandler):
            def log_message(self, *_args):
                pass

            def forward(self):
                try:
                    body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
                    headers = {
                        key: value
                        for key, value in self.headers.items()
                        if key.lower() not in {"host", "connection", "content-length", "transfer-encoding"}
                    }
                    with httpx.Client(timeout=120, trust_env=False) as client:
                        upstream = client.request(self.command, test.base + self.path, headers=headers, content=body)
                    state = (
                        json.loads(body)
                        if self.command == "PUT"
                        and self.path == f"/internal/checkpoints/{thread}/adapter-state"
                        and upstream.is_success
                        else {}
                    )
                    if state.get("receipts") and not state.get("intent"):
                        observed.update(state)
                        test.parent.stop(kill=True)
                        self.close_connection = True
                        committed.set()
                        return
                    self.send_response(upstream.status_code)
                    for key, value in upstream.headers.items():
                        if key.lower() not in {"content-length", "transfer-encoding", "connection", "content-encoding"}:
                            self.send_header(key, value)
                    self.send_header("Content-Length", str(len(upstream.content)))
                    self.end_headers()
                    self.wfile.write(upstream.content)
                except Exception as error:
                    observed["relay_error"] = type(error).__name__
                    committed.set()
                    self.close_connection = True

            do_GET = do_POST = do_PUT = forward

        relay = ThreadingHTTPServer(("127.0.0.1", 0), Relay)
        self.servers.append(relay)
        threading.Thread(target=relay.serve_forever, daemon=True).start()
        return f"http://127.0.0.1:{relay.server_port}", committed, observed

    def test_managed_runtime_lifecycle(self):
        thread = self.thread()
        first = self.create_run(thread, {"text": "first", "tool": "write"})
        stream = self.http.get(
            self.base + f"/threads/{thread}/runs/{first}/stream", headers={"Authorization": "Bearer " + self.api_key}
        )
        self.assertEqual(stream.status_code, 200)
        self.assertIn("messages/partial", stream.text)
        self.assertNotIn(self.runner_token, stream.text)
        self.assertNotIn(self.downstream_secret, stream.text)
        output = self.final(thread, first)["output"]
        self.assertEqual(output["turn"], 1)
        self.assertFalse(output["bootstrap_environment_present"])
        self.assertFalse(output["forbidden_fixture_readable"])
        self.assertEqual(output["host_socket_status"], "absent")
        self.assertNotEqual(output["uid"], 0)
        self.assertEqual(self.effects, 1)
        self.assertTrue(any(row["thread_id"] == thread for row in self.request("/admin-api/threads")))
        self.assertTrue(any(row["run_id"] == first for row in self.request("/admin-api/runs")))
        actions = self.request("/admin-api/connector-actions?run_id=" + first)
        self.assertTrue(any(row["state"] == "response_observed" for row in actions))
        self.evidence["cases"].append(
            "installed parent, Admin Threads/Runs, streaming, confined workload and server-injected MCP effect"
        )
        self.parent.stop()
        self.start_parent()
        second = self.create_run(thread, {"text": "after restart", "tool": "blocked"})
        self.assertEqual(self.final(thread, second)["output"]["turn"], 2)
        self.assertEqual(self.effects, 1, "denied MCP must not reach target")
        self.evidence["cases"].append(
            "real SQL opaque state continuity after parent restart and denied tool has no effect"
        )

        recovery = self.thread()
        self.parent.stop()
        relay, committed, receipt = self.relay_after_receipt(recovery)
        self.start_parent(relay)
        replay = self.create_run(recovery, {"tool": "write"})
        self.assertTrue(committed.wait(120), "no real checkpoint receipt commit observed")
        self.assertNotIn("relay_error", receipt)
        self.runtime_names.update(item["sandbox"] for item in receipt["receipts"].values())
        count = self.effects
        self.start_parent()
        final = self.final(recovery, replay)
        self.assertEqual(final["status"], "success")
        self.assertGreaterEqual(final["metadata"]["_runkite_completion_generation"], 2)
        self.assertEqual(self.effects, count, "committed receipt recovery must not repeat MCP")
        self.evidence["cases"].append(
            "crash after committed SQL receipt reclaims at later generation without repeating tool"
        )

        uncertain_thread = self.thread()
        uncertain = self.create_run(uncertain_thread, {"tool": "write", "sleep": True})
        intent = wait_for(
            "durable isolated launch intent", lambda: self.state(uncertain_thread, uncertain), timeout=120
        )
        self.runtime_names.add(intent["intent"]["sandbox"])
        wait_for("tool effect before worker crash", lambda: self.effects > count, timeout=120)
        self.parent.stop(kill=True)
        count = self.effects
        self.start_parent()
        final = self.final(uncertain_thread, uncertain)
        self.assertNotEqual(final["status"], "success")
        self.assertEqual(self.effects, count)
        stale = self.http.post(
            self.base + "/internal/connectors/fixture/session",
            headers={
                "X-Runner-Kind": "pool-fixture",
                "X-Runner-Token": self.runner_token,
                "X-Runkite-Run-Id": uncertain,
                "X-Runkite-Generation": "1",
            },
            json={},
        )
        self.assertEqual(stale.status_code, 403)
        self.evidence["cases"].append(
            "uncertain launch intent refuses replay after reclaim; stale attempt cannot obtain connector authority"
        )

        cancelled_thread = self.thread()
        cancelled = self.create_run(cancelled_thread, {"sleep": True})
        intent = wait_for("cancelled launch intent", lambda: self.state(cancelled_thread, cancelled), timeout=120)
        self.runtime_names.add(intent["intent"]["sandbox"])
        self.request(f"/threads/{cancelled_thread}/runs/{cancelled}/cancel", {})
        self.assertEqual(self.final(cancelled_thread, cancelled)["status"], "interrupted")
        self.evidence["cases"].append(
            "managed cancellation retains interrupted durable status and owned runtime handle"
        )
        self.evidence["passed"] = True

    def cleanup(self):
        cleanup_errors = []
        for process in reversed(self.processes):
            process.stop()
        for server in self.servers:
            server.shutdown()
            server.server_close()
        if hasattr(self, "http"):
            self.http.close()
        if self.runtime_names:
            sys.path.insert(0, str(ROOT / "python"))
            from runkite_runner.isolated import OpenShellRuntime

            runtime = OpenShellRuntime(
                self.cli, self.image, str(self.policy), ["python3", "/app/workload.py"], gateway_endpoint=self.gateway
            )
            for name in self.runtime_names:
                try:
                    if not asyncio.run(runtime.cleanup(name)):
                        self.evidence["passed"] = False
                        cleanup_errors.append(name)
                except Exception:
                    self.evidence["passed"] = False
                    self.evidence.setdefault("unconfirmed_cleanup", []).append(name)
                    cleanup_errors.append(name)
        self.evidence["logs"] = str(self.directory)
        output = Path(os.environ.get("RUNKITE_TEST_ISOLATED_OUTPUT", str(self.directory / "evidence.json")))
        output.write_text(json.dumps(self.evidence, indent=2) + "\n")
        if cleanup_errors:
            self.fail("Owned runtime cleanup was not confirmed; inspect retained fixture evidence")


if __name__ == "__main__":
    unittest.main()
