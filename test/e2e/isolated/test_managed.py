"""Opt-in installed parent worker, real SQL plane and external OpenShell.

The gateway/image must be disposable and explicitly supplied. No model provider
is called. All tool effects and credentials are synthetic and locally owned.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import secrets
import ssl
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import quote, urlparse

import httpx

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))
from isolated_fixture_environment import fixture_inputs, sterile_environment  # noqa: E402

sys.path.insert(0, str(ROOT / "python/adapters/agent_framework_adapter"))
from test_e2e import Process, free_port, wait_for  # noqa: E402


def copy_gateway_tls_bundle(source: Path, destination: Path, gateway: str) -> None:
    """Copy only explicit fixture mTLS files into the child's private CLI state."""
    try:
        if not source.is_absolute() or source.is_symlink():
            raise AssertionError("owned gateway TLS config must be explicitly selected")
        root = source.resolve(strict=True)
        gateway_name = re.sub(r"[^a-zA-Z0-9._-]", "_", gateway)
        relative = Path("openshell") / "gateways" / gateway_name / "mtls"
        bundle = destination / relative
        bundle.mkdir(parents=True, exist_ok=True, mode=0o700)
        for name in ("ca.crt", "tls.crt", "tls.key"):
            selected = root / relative / name
            resolved = selected.resolve(strict=True)
            if selected.is_symlink() or not resolved.is_relative_to(root) or not stat.S_ISREG(selected.stat().st_mode):
                raise AssertionError("owned gateway TLS bundle is invalid")
            with selected.open("rb") as source_file:
                content = source_file.read(65537)
            if len(content) > 65536:
                raise AssertionError("owned gateway TLS bundle exceeds its bound")
            target = bundle / name
            descriptor = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "wb") as output:
                output.write(content)
    except OSError:
        raise AssertionError("owned gateway TLS bundle is unavailable") from None


def wait_for_owned_sandbox_state(
    cli: str, gateway: str, sandbox: str, environment: dict, *, present: bool, timeout=30
) -> float:
    """Observe one exact owned runtime without deleting or reconciling it."""
    if not re.fullmatch(r"rk-[a-f0-9]{16}", sandbox):
        raise AssertionError("cancellation cleanup requires an exact owned sandbox")
    started = time.monotonic()
    deadline = started + timeout
    while time.monotonic() < deadline:
        try:
            result = subprocess.run(
                [
                    cli,
                    "--gateway-endpoint",
                    gateway,
                    "--workspace",
                    "default",
                    "sandbox",
                    "list",
                    "--output",
                    "json",
                    "--selector",
                    f"runkite_attempt={sandbox}",
                ],
                env=environment,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                timeout=min(5, max(0.01, deadline - time.monotonic())),
                check=False,
            )
            if len(result.stdout) > 1 << 20:
                raise AssertionError("cancellation cleanup listing exceeds its bound")
            if result.returncode == 0:
                listing = json.loads(result.stdout)
                if (
                    not isinstance(listing, dict)
                    or not isinstance(listing.get("sandboxes"), list)
                    or listing.get("next_page_token")
                ):
                    raise AssertionError("cancellation cleanup listing is incomplete")
                if len(listing["sandboxes"]) > 1:
                    raise AssertionError("cancellation runtime selector is ambiguous")
                for item in listing["sandboxes"]:
                    name = item.get("name") if isinstance(item, dict) else None
                    metadata = item.get("metadata") if isinstance(item, dict) else None
                    if not name and isinstance(metadata, dict):
                        name = metadata.get("name")
                    if name != sandbox:
                        raise AssertionError("cancellation cleanup listing escaped its owned selector")
                if bool(listing["sandboxes"]) == present:
                    return round(time.monotonic() - started, 3)
        except (OSError, subprocess.TimeoutExpired, ValueError):
            # Temporary gateway failure is not absence or cleanup success.
            pass
        time.sleep(min(0.25, max(0, deadline - time.monotonic())))
    if present:
        raise AssertionError("owned sandbox was not observed present before cancellation")
    raise AssertionError("cancelled owned sandbox remained present before fixture teardown")


def wait_for_owned_sandbox_presence(cli: str, gateway: str, sandbox: str, environment: dict, *, timeout=30) -> float:
    return wait_for_owned_sandbox_state(cli, gateway, sandbox, environment, present=True, timeout=timeout)


def wait_for_owned_sandbox_absence(cli: str, gateway: str, sandbox: str, environment: dict, *, timeout=30) -> float:
    return wait_for_owned_sandbox_state(cli, gateway, sandbox, environment, present=False, timeout=timeout)


@unittest.skipUnless(os.environ.get("RUNKITE_TEST_ISOLATED") == "1", "explicit disposable runtime opt-in required")
class ManagedIsolatedE2E(unittest.TestCase):
    def setUp(self):
        self.inputs = fixture_inputs(os.environ)
        self.directory = Path(tempfile.mkdtemp(prefix="runkite-isolated-e2e-"))
        self.environment = sterile_environment(self.directory / "environment", source=os.environ)
        self.processes = []
        self.servers = []
        self.runtime_names = set()
        self.ha = self.inputs.get("RUNKITE_TEST_ISOLATED_HA") == "1"
        self.repetitions = int(self.inputs.get("RUNKITE_TEST_ISOLATED_REPETITIONS", "1"))
        self.assertTrue(1 <= self.repetitions <= 20, "bounded repetitions must be from 1 to 20")
        self.started = time.monotonic()
        self.evidence = {
            "passed": False,
            "scope": "two real Postgres/Redis planes + mTLS worker identity + installed parent + OpenShell"
            if self.ha
            else "real SQLite plane + installed trusted parent + external OpenShell",
            "cases": [],
            "cancellation_observations": [],
            "live_generation_observations": [],
            "runtime_gateway_tls": self.inputs.get("RUNKITE_TEST_ISOLATED_GATEWAY_TLS") == "1",
        }
        self.addCleanup(self.cleanup)
        self.gateway = self.inputs["RUNKITE_TEST_ISOLATED_GATEWAY"]
        self.assertIn(
            urlparse(self.gateway).hostname, {"127.0.0.1", "::1"}, "only a dedicated loopback gateway is allowed"
        )
        if self.ha:
            self.assertEqual(urlparse(self.gateway).scheme, "https", "combined qualification requires gateway TLS")
            self.assertTrue(
                self.evidence["runtime_gateway_tls"], "combined fixture must enable authenticated gateway TLS"
            )
        if urlparse(self.gateway).scheme == "https":
            selected_config = self.inputs.get("RUNKITE_TEST_ISOLATED_GATEWAY_CONFIG")
            self.assertTrue(selected_config, "owned gateway TLS config must be explicitly selected")
            copy_gateway_tls_bundle(Path(selected_config), Path(self.environment["XDG_CONFIG_HOME"]), self.gateway)
        self.cli = str(Path(self.inputs["RUNKITE_TEST_ISOLATED_CLI"]).resolve())
        self.image = self.inputs["RUNKITE_TEST_ISOLATED_IMAGE"]
        self.api_key, self.runner_token, self.downstream_secret = (secrets.token_urlsafe(32) for _ in range(3))
        self.effects = 0
        self.lock = threading.Lock()
        self.run_proofs = {}
        self.owned_run_ids = set()
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
        scheme = "https" if self.ha else "http"
        self.bases = [f"{scheme}://127.0.0.1:{free_port()}" for _ in range(2 if self.ha else 1)]
        self.grpcs = [f"127.0.0.1:{free_port()}" for _ in self.bases]
        self.base, self.grpc = self.bases[0], self.grpcs[-1]
        self.parent_identity = "a"
        self.worker_http = {}
        if self.ha:
            self.make_certificates()
        self.http = self.client_for("a")
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
        if self.ha:
            postgres = self.inputs["RUNKITE_TEST_ISOLATED_POSTGRES_DSN"]
            redis = self.inputs["RUNKITE_TEST_ISOLATED_REDIS_URL"]
            self.assertIn(urlparse(postgres).hostname, {"127.0.0.1", "::1"})
            self.assertTrue(urlparse(postgres).path.startswith("/runkite_isolated_ha_"))
            self.assertIn(urlparse(redis).hostname, {"127.0.0.1", "::1"})
            self.assertEqual(urlparse(redis).path, "/0", "qualification requires its own Redis container")
            self.plane_environment.pop("DATABASE_PATH", None)
            self.plane_environment.update(
                {
                    "POSTGRES_DSN": postgres,
                    "REDIS_URL": redis,
                    "RUNKITE_WORKLOAD_IDENTITY": "mtls",
                    "RUNKITE_WORKLOAD_IDENTITIES_FILE": str(self.directory / "identities.json"),
                    "RUNKITE_WORKLOAD_MAX_CERT_TTL": "24h",
                }
            )
            for prefix in ("TLS_", "GRPC_TLS_"):
                self.plane_environment.update(
                    {
                        prefix + "CERT_FILE": str(self.pki / "server.pem"),
                        prefix + "KEY_FILE": str(self.pki / "server.key"),
                        prefix + "CLIENT_CA_FILE": str(self.pki / "ca.pem"),
                    }
                )
        self.install_parent()
        self.planes = []
        for index in range(len(self.bases)):
            self.start_plane(index)
        self.plane = self.planes[0]
        self.assignment_relay, _, _ = self.relay_after_receipt()
        self.start_parent()

    def make_certificates(self):
        self.pki = self.directory / "pki"
        self.pki.mkdir(mode=0o700)

        def openssl(*args):
            result = subprocess.run(
                ["openssl", *args], cwd=self.pki, env=self.environment, capture_output=True, text=True, timeout=30
            )
            self.assertEqual(result.returncode, 0, "owned fixture certificate generation failed")

        (self.pki / "ca.conf").write_text(
            "[req]\ndistinguished_name=dn\nx509_extensions=ca\nprompt=no\n[dn]\nCN=Runkite isolated fixture CA\n[ca]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n"
        )
        openssl(
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            "ca.key",
            "-out",
            "ca.pem",
            "-days",
            "1",
            "-config",
            "ca.conf",
        )
        for name, extension in [
            ("server", "extendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1,DNS:localhost\n"),
            ("a", "extendedKeyUsage=clientAuth\nsubjectAltName=URI:spiffe://fixture.test/worker-a\n"),
            ("b", "extendedKeyUsage=clientAuth\nsubjectAltName=URI:spiffe://fixture.test/worker-b\n"),
            ("rotated-a", "extendedKeyUsage=clientAuth\nsubjectAltName=URI:spiffe://fixture.test/worker-a\n"),
            ("rotated-b", "extendedKeyUsage=clientAuth\nsubjectAltName=URI:spiffe://fixture.test/worker-b\n"),
        ]:
            (self.pki / f"{name}.conf").write_text(
                "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n" + extension
            )
            (self.pki / f"{name}-request.conf").write_text(
                "[req]\ndistinguished_name=dn\nprompt=no\n[dn]\nCN=Runkite fixture " + name + "\n"
            )
            openssl(
                "req",
                "-new",
                "-newkey",
                "rsa:2048",
                "-nodes",
                "-keyout",
                name + ".key",
                "-out",
                name + ".csr",
                "-config",
                name + "-request.conf",
            )
            openssl(
                "x509",
                "-req",
                "-in",
                name + ".csr",
                "-CA",
                "ca.pem",
                "-CAkey",
                "ca.key",
                "-CAcreateserial",
                "-out",
                name + ".pem",
                "-days",
                "1",
                "-extfile",
                name + ".conf",
            )
            openssl("verify", "-CAfile", "ca.pem", name + ".pem")
            (self.pki / (name + ".key")).chmod(0o600)
        (self.pki / "ca.key").chmod(0o600)
        (self.directory / "identities.json").write_text(
            json.dumps(
                {
                    "identities": {
                        "pool-fixture": ["spiffe://fixture.test/worker-a", "spiffe://fixture.test/worker-b"]
                    },
                    "denied_key_sha256": [],
                }
            )
        )

    def tls_context(self, identity):
        context = ssl.create_default_context(cafile=str(self.pki / "ca.pem"))
        context.load_cert_chain(str(self.pki / (identity + ".pem")), str(self.pki / (identity + ".key")))
        return context

    def client_for(self, identity):
        if identity not in self.worker_http:
            self.worker_http[identity] = httpx.Client(
                timeout=120, trust_env=False, verify=self.tls_context(identity) if self.ha else True
            )
        return self.worker_http[identity]

    def start_plane(self, index):
        process = self.launch(
            [
                self.inputs["RUNKITE_TEST_BINARY"],
                "serve" if self.ha else "dev",
                "--config",
                str(self.config),
                "--port",
                self.bases[index].rsplit(":", 1)[1],
                "--grpc-port",
                self.grpcs[index].rsplit(":", 1)[1],
            ],
            self.plane_environment,
        )
        if index < len(self.planes):
            self.planes[index] = process
        else:
            self.planes.append(process)
        wait_for("isolated plane readiness", lambda: self.http.get(self.bases[index] + "/readyz").is_success)

    def install_parent(self):
        wheel = Path(self.inputs["RUNKITE_TEST_RUNNER_WHEEL"]).resolve()
        self.assertTrue(wheel.is_file())
        consumer = self.directory / "consumer"
        subprocess.run(
            [sys.executable, "-I", "-m", "venv", "--without-pip", str(consumer)],
            env=self.environment,
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
        versions = subprocess.check_output(
            [
                str(python),
                "-I",
                "-c",
                "import importlib.metadata,json;print(json.dumps(sorted(({'name':d.metadata['Name'],'version':d.version} for d in importlib.metadata.distributions()),key=lambda d:d['name'].lower())))",
            ],
            cwd=self.directory,
            env=self.worker_environment,
            text=True,
        )
        self.evidence["installed_consumer_distributions"] = json.loads(versions)

    def launch(self, command, environment):
        process = Process(
            [str(value) for value in command], self.directory, environment, f"process-{len(self.processes)}"
        )
        self.processes.append(process)
        return process

    def start_parent(self, address=None, identity=None):
        self.parent_identity = identity or self.parent_identity
        environment = dict(self.worker_environment)
        if self.ha:
            environment.update(
                {
                    "RUNKITE_TLS_CA_FILE": str(self.pki / "ca.pem"),
                    "RUNKITE_TLS_CLIENT_CERT_FILE": str(self.pki / (self.parent_identity + ".pem")),
                    "RUNKITE_TLS_CLIENT_KEY_FILE": str(self.pki / (self.parent_identity + ".key")),
                }
            )
        self.parent = self.launch(
            [
                self.parent_command,
                "--config",
                self.config,
                "--grpc-address",
                self.grpc,
                "--http-address",
                address or self.assignment_relay,
                "--execution-pool",
                "fixture",
            ],
            environment,
        )
        wait_for(
            "installed isolated worker ready",
            lambda: self.parent.child.poll() is None and "Worker ready" in self.parent.logs(),
            timeout=45,
        )

    def request(self, path, body=None, *, replica=0):
        response = self.http.request(
            "GET" if body is None else "POST",
            self.bases[replica] + path,
            headers={"Authorization": "Bearer " + self.api_key},
            json=body,
        )
        self.assertTrue(response.is_success, f"{path}: {response.status_code} {response.text}")
        return response.json() if response.content else None

    def thread(self):
        return self.request("/threads", {})["thread_id"]

    def create_run(self, thread, value):
        run = "isolated-fixture-" + secrets.token_hex(16)
        with self.lock:
            self.owned_run_ids.add(run)
        created = self.request(f"/threads/{thread}/runs", {"run_id": run, "agent_id": "isolated_echo", "input": value})
        self.assertEqual(created["run_id"], run, "created fixture run identity changed")
        return run

    def final(self, thread, run):
        def done():
            value = self.request(f"/threads/{thread}/runs/{run}")
            return value if value["status"] in {"success", "error", "timeout", "interrupted"} else None

        return wait_for("isolated durable result", done, timeout=120)

    def state(self, thread, run, generation=None):
        with self.lock:
            if run not in self.run_proofs:
                return None
        if generation is None:
            current = self.request(f"/threads/{thread}/runs/{run}")
            generation = current.get("metadata", {}).get("_runkite_attempt_generation", 1)
        response = self.client_for(self.parent_identity).get(
            self.base + f"/internal/checkpoints/{thread}/adapter-state",
            headers=self.bound_headers(run, generation),
        )
        return response.json() if response.status_code == 200 else None

    def observe_assignment_proof(self, method, path, headers, status):
        run = headers.get("X-Runkite-Run-Id")
        if method != "GET" or not run or path != f"/internal/runs/{quote(run, safe='')}/status" or status != 200:
            return
        with self.lock:
            if run not in self.owned_run_ids:
                return
        proof = headers.get("X-Runkite-Run-Incarnation")
        self.assertTrue(
            isinstance(proof, str) and 1 <= len(proof) <= 256 and all(0x21 <= ord(value) <= 0x7E for value in proof),
            "installed parent supplied no valid original record proof",
        )
        with self.lock:
            self.assertTrue(self.run_proofs.setdefault(run, proof) == proof, "fixture run record identity changed")

    def relay_after_receipt(self, thread=None):
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
                    with httpx.Client(
                        timeout=120, trust_env=False, verify=test.tls_context(test.parent_identity) if test.ha else True
                    ) as client:
                        upstream = client.request(self.command, test.base + self.path, headers=headers, content=body)
                    test.observe_assignment_proof(self.command, self.path, self.headers, upstream.status_code)
                    state = (
                        json.loads(body)
                        if self.command == "PUT"
                        and self.path == f"/internal/checkpoints/{thread}/adapter-state"
                        and upstream.is_success
                        else {}
                    )
                    if state.get("receipts") and not state.get("intent"):
                        observed.update(state)
                        observed["_fixture_receipt_generation"] = int(self.headers["X-Runkite-Generation"])
                        run = self.headers["X-Runkite-Run-Id"]
                        proof = test.bound_headers(run)["X-Runkite-Run-Incarnation"]
                        test.assertTrue(
                            self.headers["X-Runkite-Run-Incarnation"] == proof, "receipt request proof changed"
                        )
                        observed["_fixture_run_incarnation_id"] = proof
                        test.assertTrue(
                            state["receipts"].get(proof, {}).get("run_id") == run,
                            "committed isolated receipt belongs to another original record",
                        )
                        with httpx.Client(
                            timeout=15,
                            trust_env=False,
                            verify=test.tls_context(test.parent_identity) if test.ha else True,
                        ) as client:
                            snapshot = client.get(test.base + f"/internal/runs/{run}/usage", headers=headers)
                            snapshot.raise_for_status()
                            observed["_fixture_usage_snapshot"] = snapshot.json()["snapshot"]
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
        if self.ha:
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain(str(self.pki / "server.pem"), str(self.pki / "server.key"))
            context.load_verify_locations(str(self.pki / "ca.pem"))
            context.verify_mode = ssl.CERT_REQUIRED
            relay.socket = context.wrap_socket(relay.socket, server_side=True)
        self.servers.append(relay)
        threading.Thread(target=relay.serve_forever, daemon=True).start()
        return f"{'https' if self.ha else 'http'}://127.0.0.1:{relay.server_port}", committed, observed

    def test_managed_runtime_lifecycle(self):
        thread = self.thread()
        first = self.create_run(thread, {"text": "first", "tool": "write"})
        stream = self.http.get(
            self.base + f"/threads/{thread}/runs/{first}/stream", headers={"Authorization": "Bearer " + self.api_key}
        )
        self.assertEqual(stream.status_code, 200)
        if "messages/partial" not in stream.text:
            durable = self.final(thread, first)
            self.fail(
                "first workload produced no incremental event: "
                + json.dumps({"status": durable["status"], "error": durable.get("error")})
            )
        self.assertNotIn(self.runner_token, stream.text)
        self.assertNotIn(self.downstream_secret, stream.text)
        output = self.final(thread, first)["output"]
        self.assertEqual(output["turn"], 1)
        self.assertFalse(output["bootstrap_environment_present"])
        self.assertFalse(output["forbidden_fixture_readable"])
        self.assertEqual(output["host_socket_status"], "absent")
        self.assertNotEqual(output["uid"], 0)
        self.assertEqual(output["usage"]["reporting_status"], "reported")
        self.assertEqual(output["usage"]["total_tokens"], 0)
        self.assertFalse(output["usage"]["unmetered"])
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
        self.start_parent(identity="b" if self.ha else None)
        final = self.final(recovery, replay)
        self.assertEqual(final["status"], "success")
        self.assertGreater(final["metadata"]["_runkite_completion_generation"], receipt["_fixture_receipt_generation"])
        self.assertEqual(self.effects, count, "committed receipt recovery must not repeat MCP")
        self.assertEqual(
            final["output"]["usage"]["reported_generation"], receipt["_fixture_usage_snapshot"]["reported_generation"]
        )
        if self.ha:
            self.assertEqual(
                final["output"]["usage"]["reported_workload"], receipt["_fixture_usage_snapshot"]["workload_owner"]
            )
        self.assertEqual(final["output"]["usage"]["total_tokens"], 0)
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
        stale = self.client_for(self.parent_identity).post(
            self.base + "/internal/connectors/fixture/session",
            headers=self.bound_headers(uncertain, intent["intent"]["generation"]),
            json={},
        )
        self.assertEqual(stale.status_code, 403)
        self.assertIn(stale.json().get("reason_code"), {"run_not_inflight", "run_generation_mismatch"})
        self.evidence["cases"].append(
            "uncertain launch intent refuses replay after reclaim; stale attempt cannot obtain connector authority"
        )

        cancelled_thread = self.thread()
        cancelled = self.create_run(
            cancelled_thread,
            {
                "sleep": True,
                "usage_fixture": {"prompt_tokens": 5, "completion_tokens": 2, "model": "synthetic-cancel"},
            },
        )
        intent = wait_for("cancelled launch intent", lambda: self.state(cancelled_thread, cancelled), timeout=120)
        self.runtime_names.add(intent["intent"]["sandbox"])
        report = self.wait_for_partial_usage(cancelled_thread, cancelled, 5, 2)
        self.cancel_active_workload(cancelled_thread, cancelled, report, intent, 5, 2)
        self.evidence["cases"].append(
            "managed cancellation retains interrupted durable status and owned runtime handle"
        )
        if self.ha:
            self.qualify_replicated_usage()
        self.evidence["elapsed_seconds"] = round(time.monotonic() - self.started, 3)
        self.evidence["bounded_repetitions"] = self.repetitions if self.ha else 1
        self.evidence["passed"] = True

    def bound_headers(self, run, generation=1):
        with self.lock:
            proof = self.run_proofs.get(run)
        self.assertIsNotNone(proof, "original assignment proof has not been observed")
        return {
            "X-Runner-Kind": "pool-fixture",
            "X-Runner-Token": self.runner_token,
            "X-Runkite-Run-Id": run,
            "X-Runkite-Generation": str(generation),
            "X-Runkite-Run-Incarnation": proof,
        }

    def wait_for_partial_usage(self, thread, run, prompt_tokens, completion_tokens):
        def committed():
            current = self.request(f"/threads/{thread}/runs/{run}")
            generation = current.get("metadata", {}).get("_runkite_attempt_generation")
            if current.get("status") != "running" or type(generation) is not int or generation <= 0:
                return None
            response = self.client_for(self.parent_identity).get(
                self.base + f"/internal/runs/{run}/usage",
                headers=self.bound_headers(run, generation),
            )
            if response.status_code != 200:
                return None
            snapshot = response.json()["snapshot"]
            if not isinstance(snapshot, dict):
                return None
            expected = {
                "reported_generation": generation,
                "source": "isolated_workload",
                "status": "reported",
                "complete": False,
                "prompt_tokens": prompt_tokens,
                "completion_tokens": completion_tokens,
            }
            return snapshot if all(snapshot.get(key) == value for key, value in expected.items()) else None

        return wait_for("current live workload partial usage acknowledgement", committed, timeout=120)

    def cancel_active_workload(
        self, thread, run, report, intent, prompt_tokens, completion_tokens, *, replica=0, repeats=1
    ):
        """Prove activity, exact presence and current authority before cancellation."""
        generation = report.get("reported_generation")
        self.assertTrue(type(generation) is int and generation > 0, "cancellation requires a current generation")
        for key, value in {
            "source": "isolated_workload",
            "status": "reported",
            "complete": False,
            "prompt_tokens": prompt_tokens,
            "completion_tokens": completion_tokens,
        }.items():
            self.assertEqual(report.get(key), value, "cancellation requires actual partial workload activity")
        launch = intent.get("intent") if isinstance(intent, dict) else None
        self.assertIsInstance(launch, dict, "cancellation requires its exact current launch intent")
        self.assertEqual(launch.get("run_id"), run, "cancellation intent belongs to another run")
        self.assertEqual(launch.get("generation"), generation, "cancellation activity and intent generations differ")
        self.assertTrue(
            launch.get("run_incarnation_id") == self.bound_headers(run, generation)["X-Runkite-Run-Incarnation"],
            "cancellation intent belongs to another original record",
        )
        sandbox = launch.get("sandbox")
        self.assertTrue(
            isinstance(sandbox, str) and re.fullmatch(r"rk-[a-f0-9]{16}", sandbox),
            "cancellation requires an owned sandbox",
        )
        self.runtime_names.add(sandbox)

        def assert_live():
            value = self.request(f"/threads/{thread}/runs/{run}", replica=replica)
            self.assertEqual(value.get("status"), "running", "cancellation positive control requires a live run")
            self.assertEqual(
                value.get("metadata", {}).get("_runkite_attempt_generation"),
                generation,
                "cancellation positive control requires the same live generation",
            )

        assert_live()
        wait_for_owned_sandbox_presence(self.cli, self.gateway, sandbox, self.environment)
        assert_live()
        # Only the first cancel claims a live-workload control. A repeated API
        # request retains the existing terminal-settlement idempotency probe.
        for _ in range(repeats):
            self.request(f"/threads/{thread}/runs/{run}/cancel", {}, replica=replica)
        final = self.request(f"/threads/{thread}/runs/{run}", replica=replica)
        self.assertEqual(final["status"], "interrupted")
        elapsed = wait_for_owned_sandbox_absence(self.cli, self.gateway, sandbox, self.environment)
        self.runtime_names.discard(sandbox)
        self.evidence["cancellation_observations"].append(
            {
                "run_id": run,
                "generation": generation,
                "sandbox": sandbox,
                "observed_present_before_cancel": True,
                "run_live_before_presence": True,
                "run_live_after_presence": True,
                "generation_matched_before_cancel": True,
                "workload_activity_observed": True,
                "activity_prompt_tokens": prompt_tokens,
                "activity_completion_tokens": completion_tokens,
                "observed_absent_before_teardown": True,
                "method": "read_only_selector",
                "elapsed_seconds": elapsed,
            }
        )
        return final

    def qualify_live_generation_denials(self, thread, run, report):
        """Current live controls isolate generation mismatch from terminal denial."""
        generation = report["reported_generation"]
        self.assertGreater(generation, 0, "generation probe requires a current positive attempt")
        client = self.client_for(self.parent_identity)
        base = self.bases[1]
        current = self.bound_headers(run, generation)
        mismatched = self.bound_headers(run, generation - 1)

        def assert_live():
            value = self.request(f"/threads/{thread}/runs/{run}", replica=1)
            self.assertEqual(value["status"], "running", "generation probe requires a live run")
            self.assertEqual(value["metadata"]["_runkite_attempt_generation"], generation)

        assert_live()
        usage_url = base + f"/internal/runs/{run}/usage"
        positive = client.get(usage_url, headers=current)
        self.assertEqual(positive.status_code, 200, "current live usage read control failed")
        snapshot = positive.json()["snapshot"]
        self.assertEqual(snapshot, report, "live usage changed before the generation probe")
        body = {
            key: value
            for key, value in snapshot.items()
            if key
            in {
                "schema_version",
                "sequence",
                "status",
                "complete",
                "prompt_tokens",
                "completion_tokens",
                "model",
                "cost_usd",
            }
        }
        positive_write = client.post(usage_url, headers=current, json=body)
        self.assertEqual(positive_write.status_code, 200, "current idempotent usage control failed")
        self.assertEqual(positive_write.json()["snapshot"], snapshot)
        negative_usage = client.post(usage_url, headers=mismatched, json=body)
        self.assertEqual(negative_usage.status_code, 403, "mismatched generation obtained live usage authority")
        self.assertEqual(negative_usage.json().get("reason_code"), "run_generation_mismatch")

        checkpoint_url = base + f"/internal/checkpoints/{thread}/adapter-state"
        checkpoint = client.get(checkpoint_url, headers=current)
        self.assertEqual(checkpoint.status_code, 200, "current live checkpoint control failed")
        etag = checkpoint.headers.get("ETag", "")
        self.assertRegex(etag, r'^"[1-9][0-9]{0,18}"$', "checkpoint control requires a strong version")
        self.assertLessEqual(int(etag[1:-1]), (1 << 63) - 1)
        self.assertEqual(checkpoint.headers.get("X-Runkite-Checkpoint-Framework"), "isolated-v1")
        self.assertEqual(
            checkpoint.json()["intent"]["generation"],
            generation,
            "checkpoint control requires the actual current worker's committed intent",
        )
        self.assertTrue(
            checkpoint.json()["intent"].get("run_id") == run
            and checkpoint.json()["intent"].get("run_incarnation_id") == current["X-Runkite-Run-Incarnation"],
            "checkpoint control belongs to another original record",
        )
        negative_checkpoint = client.put(
            checkpoint_url,
            headers={
                **mismatched,
                "If-Match": etag,
                "X-Runkite-Checkpoint-Framework": "isolated-v1",
                "Content-Type": "application/octet-stream",
            },
            content=checkpoint.content,
        )
        self.assertEqual(
            negative_checkpoint.status_code, 403, "mismatched generation obtained checkpoint write authority"
        )
        self.assertEqual(negative_checkpoint.json().get("reason_code"), "run_generation_mismatch")
        after_checkpoint = client.get(checkpoint_url, headers=current)
        self.assertEqual(after_checkpoint.status_code, 200)
        self.assertEqual(after_checkpoint.headers.get("ETag"), etag)
        self.assertEqual(after_checkpoint.content, checkpoint.content)
        after_usage = client.get(usage_url, headers=current)
        self.assertEqual(after_usage.status_code, 200)
        self.assertEqual(after_usage.json()["snapshot"], snapshot)
        assert_live()
        self.evidence["live_generation_observations"].append(
            {
                "run_id": run,
                "generation": generation,
                "mismatched_generation": generation - 1,
                "live_before": True,
                "live_after": True,
                "usage_current_get": 200,
                "usage_idempotent_post": 200,
                "usage_mismatched_post": 403,
                "checkpoint_current_get": 200,
                "checkpoint_mismatched_put": 403,
                "checkpoint_unchanged": True,
                "usage_unchanged": True,
            }
        )

    def qualify_replicated_usage(self):
        self.evidence["cases"].append(
            "two TLS planes share Postgres/Redis; worker gRPC uses replica B while HTTP reports use replica A"
        )
        for repetition in range(self.repetitions):
            before = self.request("/admin-api/usage/summary?agent_id=isolated_echo", replica=1)
            old_in = sum(row["tokens_in"] for row in before)
            old_out = sum(row["tokens_out"] for row in before)
            thread = self.thread()
            run = self.create_run(
                thread,
                {
                    "sleep": True,
                    "usage_fixture": {"prompt_tokens": 7, "completion_tokens": 3, "model": "synthetic-isolated"},
                },
            )

            report = self.wait_for_partial_usage(thread, run, 7, 3)
            generation = report["reported_generation"]
            self.assertFalse(report["complete"])
            self.qualify_live_generation_denials(thread, run, report)
            intent = self.state(thread, run)
            self.runtime_names.add(intent["intent"]["sandbox"])
            # A separately authorized member of the same pool must not borrow
            # the current worker's assignment, even on the other replica.
            wrong = "a" if self.parent_identity == "b" else "b"
            denied = self.client_for(wrong).get(
                self.bases[1] + f"/internal/runs/{run}/usage", headers=self.bound_headers(run, generation)
            )
            self.assertEqual(denied.status_code, 403)
            self.assertEqual(denied.json().get("reason_code"), "workload_owner_mismatch")
            rotated = self.client_for("rotated-" + self.parent_identity).get(
                self.bases[1] + f"/internal/runs/{run}/usage", headers=self.bound_headers(run, generation)
            )
            self.assertEqual(rotated.status_code, 403)
            self.assertEqual(rotated.json().get("reason_code"), "workload_owner_mismatch")
            without_identity = ssl.create_default_context(cafile=str(self.pki / "ca.pem"))
            with httpx.Client(verify=without_identity, timeout=5, trust_env=False) as unbound:
                with self.assertRaises(httpx.HTTPError):
                    unbound.get(
                        self.bases[1] + f"/internal/runs/{run}/usage", headers=self.bound_headers(run, generation)
                    )
            authorized = self.client_for(self.parent_identity).get(
                self.bases[1] + f"/internal/runs/{run}/usage", headers=self.bound_headers(run, generation)
            )
            self.assertEqual(authorized.status_code, 200, "peer probes must target a still-current owned attempt")

            self.planes[0].stop(kill=True)
            final = self.cancel_active_workload(thread, run, report, intent, 7, 3, replica=1, repeats=2)
            value = final["output"]["usage"]
            self.assertEqual((value["prompt_tokens"], value["completion_tokens"]), (7, 3))
            self.assertTrue(value["unmetered"])
            self.assertFalse(value["complete"])
            self.assertEqual(value["reporting_source"], "isolated_workload")
            self.start_plane(0)
            self.plane = self.planes[0]
            after = self.request("/admin-api/usage/summary?agent_id=isolated_echo", replica=1)
            self.assertEqual(sum(row["tokens_in"] for row in after) - old_in, 7)
            self.assertEqual(sum(row["tokens_out"] for row in after) - old_out, 3)
            self.assertEqual(self.request(f"/threads/{thread}/runs/{run}")["output"]["usage"], value)
            self.evidence["cases"].append(
                f"bounded cycle {repetition + 1}: exact worker owner enforced, acknowledged partial survives reporting-replica death, opposite-replica cancellation and restart settle once"
            )

    def cleanup(self):
        cleanup_errors = []
        for process in reversed(self.processes):
            process.stop()
        for server in self.servers:
            server.shutdown()
            server.server_close()
        for client in getattr(self, "worker_http", {}).values():
            client.close()
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
        output = Path(self.inputs.get("RUNKITE_TEST_ISOLATED_OUTPUT", str(self.directory / "evidence.json")))
        output.write_text(json.dumps(self.evidence, indent=2) + "\n")
        if cleanup_errors:
            self.fail("Owned runtime cleanup was not confirmed; inspect retained fixture evidence")


if __name__ == "__main__":
    unittest.main()
