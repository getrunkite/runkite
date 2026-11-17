"""Failure-path qualification for owned runtime cleanup, without Docker calls."""

import importlib.util
import io
import json
import os
import shutil
import ssl
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "python"))


def load_source(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


fixture = load_source("isolated_fixture_cleanup", ROOT / "scripts/start-isolated-fixture.py")


class FixtureCleanupTests(unittest.TestCase):
    def setUp(self):
        self.directory_owner = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory_owner.cleanup)
        self.directory = Path(self.directory_owner.name) / "private-recovery"
        self.directory.mkdir(mode=0o700)
        (self.directory / "gateway-state").write_text("synthetic fixture recovery metadata")
        self.base = ["docker", "compose", "--project-name", "owned-fixture"]
        self.cli = ["openshell", "--gateway-endpoint", "http://127.0.0.1:12345"]
        self.calls = []

    def command(self, args, **_kwargs):
        self.calls.append(args)
        return subprocess.CompletedProcess(args, 0, '{"sandboxes":[]}' if "list" in args else "", "")

    def test_confirmed_cleanup_removes_private_recovery_only_after_both_checks(self):
        with patch.object(fixture.subprocess, "run", side_effect=self.command):
            result = fixture.cleanup_fixture(self.base, self.cli, io.BytesIO(), self.directory)
        self.assertTrue(result["confirmed"])
        self.assertFalse(self.directory.exists())
        self.assertTrue(any("down" in command for command in self.calls))
        self.assertTrue(any("ps" in command for command in self.calls))

    def test_failed_infrastructure_down_preserves_recovery_state(self):
        def execute(args, **kwargs):
            value = self.command(args, **kwargs)
            if "down" in args:
                value.returncode = 7
            return value

        with patch.object(fixture.subprocess, "run", side_effect=execute):
            result = fixture.cleanup_fixture(self.base, self.cli, io.BytesIO(), self.directory)
        self.assertFalse(result["confirmed"])
        self.assertIn("fixture_infrastructure_down_failed", result["reason_codes"])
        self.assertTrue((self.directory / "gateway-state").is_file())

    def test_failed_listing_or_delete_does_not_become_cleanup_success(self):
        name = "rk-" + "a" * 16
        for failure in ("list", "delete"):
            with self.subTest(failure=failure):

                def execute(args, failure=failure, **kwargs):
                    value = self.command(args, **kwargs)
                    if "list" in args:
                        value.stdout = json.dumps({"sandboxes": [{"name": name}]})
                    if failure in args:
                        value.returncode = 8
                    return value

                with patch.object(fixture.subprocess, "run", side_effect=execute):
                    result = fixture.cleanup_fixture(self.base, self.cli, io.BytesIO(), self.directory)
                self.assertFalse(result["confirmed"])
                self.assertTrue(self.directory.exists())

    def test_accepted_delete_still_requires_disappearance_confirmation(self):
        clock = [0]

        def execute(args, **kwargs):
            value = self.command(args, **kwargs)
            if "list" in args:
                value.stdout = json.dumps({"sandboxes": [{"name": "rk-" + "a" * 16}]})
            return value

        with (
            patch.object(fixture.subprocess, "run", side_effect=execute),
            patch.object(fixture.time, "monotonic", side_effect=lambda: clock[0]),
            patch.object(fixture.time, "sleep", side_effect=lambda _: clock.__setitem__(0, 2)),
        ):
            result = fixture.cleanup_fixture(self.base, self.cli, io.BytesIO(), self.directory, timeout=1)
        self.assertFalse(result["confirmed"])
        self.assertIn("runtime_delete_not_confirmed", result["reason_codes"])
        self.assertTrue(self.directory.exists())

    def test_per_delete_deadline_reserves_time_for_owned_infrastructure_removal(self):
        clock, deletes = [0.0], []

        def execute(args, **kwargs):
            value = self.command(args, **kwargs)
            if "list" in args:
                value.stdout = json.dumps({"sandboxes": [{"name": "rk-" + char * 16} for char in "abc"]})
                clock[0] += 0.25
            if "delete" in args:
                deletes.append(kwargs["timeout"])
                clock[0] += kwargs["timeout"]
            return value

        with (
            patch.object(fixture.subprocess, "run", side_effect=execute),
            patch.object(fixture.time, "monotonic", side_effect=lambda: clock[0]),
        ):
            result = fixture.cleanup_fixture(self.base, self.cli, io.BytesIO(), self.directory, timeout=1)
        self.assertEqual(deletes, [0.75])
        self.assertFalse(result["confirmed"])
        self.assertTrue(self.directory.exists())
        self.assertTrue(any("down" in command for command in self.calls))
        self.assertTrue(any("ps" in command for command in self.calls))

    def test_unrecognized_or_incomplete_records_are_never_broadly_deleted(self):
        for listing in (
            {"sandboxes": [{"name": "another-users-workload"}]},
            {"sandboxes": [], "next_page_token": "not-complete"},
        ):
            with self.subTest(listing=listing):
                self.calls.clear()

                def execute(args, listing=listing, **kwargs):
                    value = self.command(args, **kwargs)
                    if "list" in args:
                        value.stdout = json.dumps(listing)
                    return value

                with patch.object(fixture.subprocess, "run", side_effect=execute):
                    result = fixture.cleanup_fixture(self.base, self.cli, io.BytesIO(), self.directory)
                self.assertFalse(result["confirmed"])
                self.assertFalse(any("delete" in command for command in self.calls))
                self.assertTrue(self.directory.exists())

    def test_main_cleanup_failure_overrides_success_without_erasing_prior_failure(self):
        for child_status, host, socket_gid in ((0, "Darwin", 0), (7, "Darwin", 20), (0, "Linux", 0), (7, "Linux", 118)):
            with (
                self.subTest(child_status=child_status, host=host, socket_gid=socket_gid),
                tempfile.TemporaryDirectory() as directory,
            ):
                socket_file = Path(directory) / "fixture-socket"
                socket_file.touch()
                real_stat = os.stat

                def fixture_stat(
                    name, *args, real_stat=real_stat, socket_file=socket_file, socket_gid=socket_gid, **kwargs
                ):
                    result = real_stat(name, *args, **kwargs)
                    if os.fspath(name) == str(socket_file):
                        fields = list(result)
                        fields[5] = socket_gid
                        return os.stat_result(fields)
                    return result

                guard = Mock()
                guard.wait.return_value = child_status
                guard.poll.return_value = child_status
                listener = Mock()
                listener.getsockname.return_value = ("127.0.0.1", 12345)
                socket_context = Mock()
                socket_context.__enter__ = Mock(return_value=listener)
                socket_context.__exit__ = Mock(return_value=False)
                argv = [
                    "fixture",
                    "--cli",
                    "synthetic-openshell",
                    "--output-dir",
                    directory,
                    "--docker-socket",
                    str(socket_file),
                    "--",
                    "synthetic-test",
                ]
                with (
                    patch.object(sys, "argv", argv),
                    patch.dict(os.environ, {"RUNKITE_TEST_ISOLATED": "1"}),
                    patch.object(
                        fixture,
                        "local_docker_command",
                        return_value=(["docker", "--host", "unix://" + str(socket_file)], str(socket_file)),
                    ),
                    patch.object(fixture.platform, "system", return_value=host),
                    patch.object(fixture.os, "stat", side_effect=fixture_stat),
                    patch.object(fixture.socket, "socket", return_value=socket_context),
                    patch.object(fixture.subprocess, "run", side_effect=self.command),
                    patch.object(fixture.subprocess, "check_output", return_value="sha256:" + "a" * 64) as build_image,
                    patch.object(fixture.subprocess, "Popen", return_value=guard),
                    patch.object(
                        fixture,
                        "cleanup_fixture",
                        return_value={"confirmed": False, "reason_codes": ["fixture_infrastructure_down_failed"]},
                    ),
                    patch.object(fixture.signal, "signal"),
                    patch("sys.stderr", new=io.StringIO()),
                ):
                    status = fixture.main()
                self.assertEqual(status, 1 if child_status == 0 else child_status)
                self.assertFalse(json.loads((Path(directory) / "runtime-cleanup.json").read_text())["confirmed"])
                retained = list(Path(directory).glob("runtime-*/compose.json"))
                self.assertEqual(len(retained), 1)
                runtime = retained[0].parent
                compose = json.loads(retained[0].read_text())
                gateway = compose["services"]["gateway"]
                self.assertEqual(gateway["group_add"], ["0"] if socket_gid == 0 else ["0", str(socket_gid)])
                config = (runtime / "gateway.toml").read_text()
                metadata = json.loads((Path(directory) / "runtime-fixture.json").read_text())
                self.assertEqual(metadata["host_platform"], host)
                if host == "Darwin":
                    self.assertEqual(gateway["command"], ["--bind-address", "0.0.0.0", "--port", "8080"])
                    self.assertEqual(gateway["ports"], ["127.0.0.1:12345:8080"])
                    self.assertNotIn("network_mode", gateway)
                    self.assertIn('bind_address = "0.0.0.0:8080"', config)
                    self.assertIn('supervisor_network_mode = "bridge"', config)
                    self.assertIn('grpc_endpoint = "http://host.docker.internal:12345"', config)
                    self.assertEqual(metadata["network_profile"], "darwin-bridge-loopback")
                else:
                    self.assertEqual(gateway["command"], ["--bind-address", "127.0.0.1", "--port", "12345"])
                    self.assertEqual(gateway["network_mode"], "host")
                    self.assertNotIn("ports", gateway)
                    self.assertIn('bind_address = "127.0.0.1:12345"', config)
                    self.assertNotIn("health_bind_address", config)
                    self.assertNotIn("0.0.0.0", config)
                    self.assertIn('supervisor_network_mode = "host"', config)
                    self.assertIn('grpc_endpoint = "http://127.0.0.1:12345"', config)
                    self.assertEqual(metadata["network_profile"], "linux-host-loopback")
                build = next(call for call in build_image.call_args_list if "build" in call.args[0])
                self.assertEqual(build.args[0][:3], ["docker", "--host", "unix://" + str(socket_file)])
                self.assertEqual(
                    Path(build.kwargs["env"]["BUILDX_CONFIG"]).resolve(), (runtime / "environment/buildx").resolve()
                )
                self.assertEqual(guard.wait.call_args_list[0].kwargs["timeout"], 905)

    def test_managed_e2e_cleanup_failure_fails_unittest_after_writing_evidence(self):
        managed = load_source("isolated_managed_cleanup_contract", ROOT / "test/e2e/isolated/test_managed.py")
        case = managed.ManagedIsolatedE2E("test_managed_runtime_lifecycle")
        case.processes, case.servers = [], []
        case.runtime_names = {"rk-" + "a" * 16}
        case.evidence = {"passed": True}
        case.cli, case.image = "synthetic-cli", "sha256:" + "a" * 64
        case.policy, case.gateway, case.directory = self.directory / "policy", "http://127.0.0.1:12345", self.directory
        runtime = SimpleNamespace(cleanup=AsyncMock(return_value=False))
        output = self.directory / "evidence.json"
        case.inputs = {"RUNKITE_TEST_ISOLATED_OUTPUT": str(output)}
        with (
            patch("runkite_runner.isolated.OpenShellRuntime", return_value=runtime),
            patch.dict(os.environ, {"RUNKITE_TEST_ISOLATED_OUTPUT": str(output)}),
        ):
            with self.assertRaisesRegex(AssertionError, "cleanup was not confirmed"):
                case.cleanup()
        self.assertFalse(json.loads(output.read_text())["passed"])


class ManagedAssignmentProofTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.managed = load_source("isolated_managed_assignment_contract", ROOT / "test/e2e/isolated/test_managed.py")

    def setUp(self):
        self.case = self.managed.ManagedIsolatedE2E("test_managed_runtime_lifecycle")
        self.case.lock = threading.Lock()
        self.case.run_proofs = {}
        self.case.owned_run_ids = {"owned-run"}
        self.case.runner_token = "synthetic-runner-token"
        self.headers = {"X-Runkite-Run-Id": "owned-run", "X-Runkite-Run-Incarnation": "original-record"}

    def observe(self, headers=None, **options):
        self.case.observe_assignment_proof(
            options.get("method", "GET"),
            options.get("path", "/internal/runs/owned-run/status"),
            self.headers if headers is None else headers,
            options.get("status", 200),
        )

    def test_only_successful_owned_preflight_can_supply_the_original_proof(self):
        for options in (
            {"status": 401},
            {"status": 403},
            {"status": 500},
            {"method": "POST"},
            {"path": "/internal/runs/owned-run/usage"},
            {"path": "/internal/runs/owned-run/status?ignored=1"},
        ):
            with self.subTest(options=options):
                self.observe(**options)
                self.assertEqual(self.case.run_proofs, {})
        self.case.observe_assignment_proof(
            "GET",
            "/internal/runs/foreign-run/status",
            {**self.headers, "X-Runkite-Run-Id": "foreign-run"},
            200,
        )
        self.assertEqual(self.case.run_proofs, {})
        self.observe()
        self.assertEqual(self.case.bound_headers("owned-run")["X-Runkite-Run-Incarnation"], "original-record")

    def test_missing_proof_never_sends_a_probe_or_reads_a_replacement(self):
        self.case.request = Mock(side_effect=AssertionError("no public record read expected"))
        self.case.client_for = Mock(side_effect=AssertionError("no unbound probe expected"))
        self.assertIsNone(self.case.state("owned-thread", "owned-run"))
        with self.assertRaisesRegex(AssertionError, "original assignment proof has not been observed"):
            self.case.bound_headers("owned-run")
        self.case.request.assert_not_called()
        self.case.client_for.assert_not_called()

    def test_malformed_or_replacement_proof_cannot_replace_the_original(self):
        for proof in (None, "", "bad record", "bad\nrecord", "é", "x" * 257):
            with self.subTest(proof=proof):
                with self.assertRaisesRegex(AssertionError, "no valid original record proof"):
                    self.observe({**self.headers, "X-Runkite-Run-Incarnation": proof})
                self.assertEqual(self.case.run_proofs, {})
        self.observe()
        with self.assertRaisesRegex(AssertionError, "fixture run record identity changed"):
            self.observe({**self.headers, "X-Runkite-Run-Incarnation": "replacement-record"})
        self.assertEqual(self.case.run_proofs, {"owned-run": "original-record"})

    def test_checkpoint_and_stale_generation_headers_keep_the_observed_record(self):
        self.observe()
        self.case.base = "https://127.0.0.1:12345"
        self.case.parent_identity = "a"
        self.case.request = Mock(
            return_value={"metadata": {"_runkite_attempt_generation": 9}, "incarnation_id": "replacement-record"}
        )
        client = Mock()
        client.get.return_value = SimpleNamespace(status_code=200, json=lambda: {"intent": {"generation": 9}})
        self.case.client_for = Mock(return_value=client)
        self.assertEqual(self.case.state("owned-thread", "owned-run"), {"intent": {"generation": 9}})
        headers = client.get.call_args.kwargs["headers"]
        self.assertEqual(headers["X-Runkite-Run-Incarnation"], "original-record")
        self.assertEqual(headers["X-Runkite-Generation"], "9")
        stale_headers = self.case.bound_headers("owned-run", 1)
        self.assertEqual(stale_headers["X-Runkite-Run-Incarnation"], "original-record")
        self.assertEqual(stale_headers["X-Runkite-Generation"], "1")

    def test_run_is_owned_before_the_installed_worker_can_preflight(self):
        def create(path, body):
            self.assertEqual(path, "/threads/owned-thread/runs")
            run = body["run_id"]
            self.assertIn(run, self.case.owned_run_ids)
            self.case.observe_assignment_proof(
                "GET",
                f"/internal/runs/{run}/status",
                {**self.headers, "X-Runkite-Run-Id": run},
                200,
            )
            return {"run_id": run}

        self.case.request = create
        run = self.case.create_run("owned-thread", {"text": "synthetic"})
        self.assertEqual(self.case.bound_headers(run)["X-Runkite-Run-Incarnation"], "original-record")


class FixtureNetworkTests(unittest.TestCase):
    def test_ha_wrapper_rejects_remote_selection_before_invoking_docker(self):
        with tempfile.TemporaryDirectory(prefix="rk-wrap-", dir="/tmp") as directory:
            root = Path(directory)
            commands = root / "bin"
            commands.mkdir()
            marker = root / "docker-invoked"
            for command in ("docker", "uv", "openssl"):
                executable = commands / command
                executable.write_text("#!/bin/sh\nprintf invoked > '" + str(marker) + "'\nexit 97\n")
                executable.chmod(0o700)
            for name in ("plane", "cli", "runner.whl"):
                artifact = root / name
                artifact.write_text("#!/bin/sh\nexit 97\n")
                artifact.chmod(0o700)
            socket_file = root / "s"
            socket_file.write_text("not a socket: remote selectors must fail before local socket lookup")
            for selector, expected in (
                ({"DOCKER_HOST": "tcp://remote.invalid:2375"}, "remote or conflicting"),
                ({"DOCKER_CONTEXT": "remote-fixture"}, "named Docker context"),
            ):
                with self.subTest(selector=selector):
                    environment = {
                        "PATH": str(commands) + ":/usr/bin:/bin",
                        "RUNKITE_TEST_BINARY": str(root / "plane"),
                        "RUNKITE_TEST_RUNNER_WHEEL": str(root / "runner.whl"),
                        "RUNKITE_TEST_ISOLATED_CLI": str(root / "cli"),
                        "RUNKITE_TEST_DOCKER_SOCKET": str(socket_file),
                        **selector,
                    }
                    result = subprocess.run(
                        ["/bin/bash", str(ROOT / "scripts/test-isolated-ha.sh")],
                        env=environment,
                        capture_output=True,
                        text=True,
                        timeout=15,
                    )
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn(expected, result.stderr)
                    self.assertFalse(marker.exists(), "preflight must reject before any Docker command")

    def test_tls_callbacks_preserve_verification_and_unsupported_hosts_fail(self):
        linux = fixture.fixture_network("Linux", 12345, True)
        self.assertEqual(linux["callback"], "https://127.0.0.1:12345")
        self.assertEqual(linux["compose"], {"network_mode": "host"})
        mac = fixture.fixture_network("Darwin", 12345, True)
        self.assertEqual(mac["callback"], "https://host.docker.internal:12345")
        self.assertEqual(mac["compose"]["ports"], ["127.0.0.1:12345:8080"])
        with self.assertRaisesRegex(ValueError, "native Linux or macOS"):
            fixture.fixture_network("Windows", 12345, True)

    def test_local_daemon_is_explicit_and_remote_selectors_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "fixture.sock"
            path.touch()
            with patch.dict(os.environ, {}, clear=True):
                with self.assertRaisesRegex(ValueError, "local Unix socket"):
                    fixture.local_docker_command(str(path))
            for selected in (
                "tcp://remote.invalid:2375",
                "ssh://remote.invalid",
                "unix:///different/socket",
                "unix://relative",
            ):
                with (
                    self.subTest(selected=selected),
                    patch.dict(os.environ, {"DOCKER_HOST": selected}, clear=True),
                    patch.object(fixture.stat, "S_ISSOCK", return_value=True),
                ):
                    with self.assertRaisesRegex(ValueError, "remote or conflicting"):
                        fixture.local_docker_command(str(path))
            with (
                patch.dict(os.environ, {"DOCKER_CONTEXT": "remote-fixture"}, clear=True),
                patch.object(fixture.stat, "S_ISSOCK", return_value=True),
            ):
                with self.assertRaisesRegex(ValueError, "named Docker context"):
                    fixture.local_docker_command(str(path))
            with (
                patch.dict(os.environ, {"DOCKER_HOST": "unix://" + str(path)}, clear=True),
                patch.object(fixture.stat, "S_ISSOCK", return_value=True),
            ):
                command, mounted = fixture.local_docker_command(str(path))
                self.assertEqual(command, ["docker", "--host", "unix://" + str(path.resolve())])
                self.assertEqual(mounted, str(path.resolve()))


class GatewayTLSTests(unittest.TestCase):
    @unittest.skipUnless(shutil.which("openssl"), "owned certificate generation requires OpenSSL")
    def test_private_certificates_and_cli_bundles_do_not_modify_user_state(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pki = root / "pki"
            fixture.make_gateway_pki(pki)
            context = ssl.create_default_context(cafile=str(pki / "ca.pem"))
            context.load_cert_chain(str(pki / "client.pem"), str(pki / "client.key"))
            env = fixture.gateway_tls_environment(
                {"fixture": "preserved"}, root / "cli", "https://127.0.0.1:12345", pki
            )
            self.assertEqual(env["fixture"], "preserved")
            self.assertEqual(env["XDG_CONFIG_HOME"], str(root / "cli"))
            bundle = root / "cli/openshell/gateways/https___127.0.0.1_12345/mtls"
            self.assertEqual((bundle / "tls.key").read_bytes(), (pki / "client.key").read_bytes())
            self.assertEqual((bundle / "tls.key").stat().st_mode & 0o777, 0o600)
            result = subprocess.run(
                ["openssl", "verify", "-CAfile", str(pki / "ca.pem"), str(pki / "untrusted-client.pem")],
                capture_output=True,
            )
            self.assertNotEqual(result.returncode, 0)

    def test_negative_probe_requires_rejection_and_authenticated_recovery(self):
        cli = ["fixture-cli", "--gateway-endpoint", "https://127.0.0.1:12345"]
        for accepted in (False, True):
            with self.subTest(accepted=accepted):
                report = {"passed": False, "cases": []}
                with (
                    patch.object(fixture, "wait_for_gateway") as ready,
                    patch.object(fixture, "gateway_tls_environment", return_value={"synthetic": "negative"}),
                    patch.object(
                        fixture.subprocess, "run", return_value=subprocess.CompletedProcess([], 0 if accepted else 1)
                    ),
                ):
                    if accepted:
                        with self.assertRaisesRegex(RuntimeError, "accepted"):
                            fixture.qualify_gateway_tls([], cli, {}, Path("owned"), Path("pki"), io.BytesIO(), report)
                        self.assertFalse(report["passed"])
                    else:
                        fixture.qualify_gateway_tls([], cli, {}, Path("owned"), Path("pki"), io.BytesIO(), report)
                        self.assertEqual(ready.call_count, 3)
                        self.assertEqual(len(report["cases"]), 3)
                        self.assertFalse(report["passed"], "preflight alone cannot establish outage recovery")

    def test_outage_requires_request_failure_restart_and_verified_recovery(self):
        for failure in ("nonzero", "timeout", "accepted", "recovery"):
            with self.subTest(failure=failure):
                commands = []
                report = {"passed": False, "cases": []}

                def execute(args, commands=commands, failure=failure, **_kwargs):
                    commands.append(args)
                    if "list" in args:
                        if failure == "timeout":
                            raise subprocess.TimeoutExpired(args, 15)
                        return subprocess.CompletedProcess(args, 0 if failure == "accepted" else 1)
                    return subprocess.CompletedProcess(args, 0)

                with (
                    patch.object(fixture.subprocess, "run", side_effect=execute),
                    patch.object(
                        fixture,
                        "wait_for_gateway",
                        side_effect=RuntimeError("recovery failed") if failure == "recovery" else None,
                    ),
                ):

                    def call(report=report):
                        return fixture.qualify_gateway_tls(
                            ["compose"],
                            ["cli", "--gateway-endpoint", "https://127.0.0.1:12345"],
                            {},
                            Path("owned"),
                            Path("pki"),
                            io.BytesIO(),
                            report,
                            outage=True,
                        )

                    if failure in {"accepted", "recovery"}:
                        with self.assertRaises(RuntimeError):
                            call()
                        self.assertFalse(report["passed"])
                    else:
                        call()
                        self.assertTrue(report["passed"])
                self.assertTrue(
                    any("up" in command for command in commands), "failed probe must still restart owned gateway"
                )


class WorkloadProbeTests(unittest.TestCase):
    def setUp(self):
        self.workload = load_source("isolated_workload_probe_contract", ROOT / "examples/isolated_agent/workload.py")

    def test_http_errors_and_non_200_responses_still_prove_network_reachability(self):
        from urllib.error import HTTPError, URLError

        for status in (200, 204, 401, 403, 404, 500):
            with self.subTest(status=status):
                if status >= 400:
                    response = HTTPError("https://synthetic.invalid", status, "synthetic response", {}, None)
                    with patch.object(self.workload, "urlopen", side_effect=response):
                        self.assertTrue(self.workload.network_reachable("https://synthetic.invalid"))
                else:
                    response = Mock()
                    response.__enter__ = Mock(return_value=SimpleNamespace(status=status))
                    response.__exit__ = Mock(return_value=False)
                    with patch.object(self.workload, "urlopen", return_value=response):
                        self.assertTrue(self.workload.network_reachable("https://synthetic.invalid"))
        with patch.object(self.workload, "urlopen", side_effect=URLError("synthetic connection failure")):
            self.assertFalse(self.workload.network_reachable("https://synthetic.invalid"))

    def test_socket_presence_is_not_probed_with_regular_file_reads(self):
        import stat

        path = Mock()
        path.lstat.return_value = SimpleNamespace(st_mode=stat.S_IFSOCK | 0o600)
        path.read_bytes.side_effect = OSError("Unix sockets cannot be read as regular files")
        with patch.object(self.workload, "Path", return_value=path):
            self.assertEqual(self.workload.socket_path_status("synthetic-socket"), "visible_socket")
        path.read_bytes.assert_not_called()
        with tempfile.TemporaryDirectory() as directory:
            actual = Path(directory) / "file"
            actual.write_bytes(b"synthetic")
            self.assertEqual(self.workload.socket_path_status(str(actual)), "visible_other")
            self.assertEqual(self.workload.socket_path_status(str(actual.parent / "absent")), "absent")

    def test_permission_denied_and_probe_error_never_claim_socket_absence(self):
        path = Mock()
        for error, expected in (
            (PermissionError(), "denied"),
            (OSError(), "unknown_error"),
            (FileNotFoundError(), "absent"),
        ):
            with self.subTest(expected=expected), patch.object(self.workload, "Path", return_value=path):
                path.lstat.side_effect = error
                self.assertEqual(self.workload.socket_path_status("synthetic-socket"), expected)


def load_managed_helpers():
    selected = importlib.util.spec_from_file_location(
        "isolated_managed_security_helpers", ROOT / "test/e2e/isolated/test_managed.py"
    )
    module = importlib.util.module_from_spec(selected)
    selected.loader.exec_module(module)
    return module


class ManagedFixtureSecurityTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.managed = load_managed_helpers()

    def test_gateway_tls_copy_uses_only_explicit_owned_bundle_and_private_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            gateway = "https://127.0.0.1:1234"
            relative = Path("openshell/gateways/https___127.0.0.1_1234/mtls")
            source = root / "owned-config"
            (source / relative).mkdir(parents=True)
            for name in ("ca.crt", "tls.crt", "tls.key"):
                (source / relative / name).write_text("synthetic " + name)
            (source / "unrelated-user-config").write_text("synthetic unrelated secret")
            destination = root / "child-config"
            self.managed.copy_gateway_tls_bundle(source, destination, gateway)
            for name in ("ca.crt", "tls.crt", "tls.key"):
                copied = destination / relative / name
                self.assertEqual(copied.read_text(), "synthetic " + name)
                self.assertEqual(stat.S_IMODE(copied.stat().st_mode), 0o600)
            self.assertFalse((destination / "unrelated-user-config").exists())
            (source / relative / "tls.key").unlink()
            (source / relative / "tls.key").symlink_to(root / "outside-key")
            (root / "outside-key").write_text("synthetic outside key")
            with self.assertRaises(AssertionError):
                self.managed.copy_gateway_tls_bundle(source, root / "other-child", gateway)

    def test_cancellation_absence_is_read_only_scoped_and_requires_complete_listing(self):
        sandbox = "rk-" + "a" * 16
        calls = []
        responses = [
            subprocess.CompletedProcess([], 0, json.dumps({"sandboxes": [{"metadata": {"name": sandbox}}]}).encode()),
            subprocess.CompletedProcess([], 0, b'{"sandboxes":[]}'),
        ]

        def execute(command, **kwargs):
            calls.append((command, kwargs))
            return responses.pop(0)

        with (
            patch.object(self.managed.subprocess, "run", side_effect=execute),
            patch.object(self.managed.time, "sleep"),
        ):
            elapsed = self.managed.wait_for_owned_sandbox_absence(
                "/owned/openshell", "https://127.0.0.1:1234", sandbox, {"HOME": "/owned/home"}, timeout=1
            )
        self.assertGreaterEqual(elapsed, 0)
        self.assertEqual(len(calls), 2)
        for command, kwargs in calls:
            self.assertIn("list", command)
            self.assertEqual(command[-2:], ["--selector", "runkite_attempt=" + sandbox])
            self.assertNotIn("delete", command)
            self.assertEqual(kwargs["env"], {"HOME": "/owned/home"})
            self.assertEqual(kwargs["stderr"], subprocess.DEVNULL)
            self.assertLessEqual(kwargs["timeout"], 1)
        for listing in (
            {"sandboxes": [], "next_page_token": "incomplete"},
            {"sandboxes": [{"name": "unrelated"}]},
            {"sandboxes": "invalid"},
        ):
            result = subprocess.CompletedProcess([], 0, json.dumps(listing).encode())
            with patch.object(self.managed.subprocess, "run", return_value=result), self.assertRaises(AssertionError):
                self.managed.wait_for_owned_sandbox_absence(
                    "/owned/openshell", "https://127.0.0.1:1234", sandbox, {}, timeout=1
                )

    def test_gateway_failure_or_retained_sandbox_never_becomes_cancellation_success(self):
        sandbox = "rk-" + "a" * 16
        for response in (
            subprocess.CompletedProcess([], 1, b"synthetic-private-error"),
            subprocess.CompletedProcess([], 0, json.dumps({"sandboxes": [{"name": sandbox}]}).encode()),
        ):
            with (
                patch.object(self.managed.subprocess, "run", return_value=response),
                patch.object(self.managed.time, "sleep"),
                patch.object(self.managed.time, "monotonic", side_effect=[0, 0, 0, 0, 2, 2]),
                self.assertRaisesRegex(AssertionError, "before fixture teardown"),
            ):
                self.managed.wait_for_owned_sandbox_absence(
                    "/owned/openshell", "https://127.0.0.1:1234", sandbox, {}, timeout=1
                )

    def test_cancellation_requires_an_exact_owned_name_before_any_cli_call(self):
        with patch.object(self.managed.subprocess, "run") as execute, self.assertRaises(AssertionError):
            self.managed.wait_for_owned_sandbox_absence(
                "/owned/openshell", "https://127.0.0.1:1234", "unrelated", {}, timeout=1
            )
        execute.assert_not_called()


class LiveGenerationProbeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.managed = load_managed_helpers()

    def invoke(self, *, failure=None):
        import httpx

        case = self.managed.ManagedIsolatedE2E("test_managed_runtime_lifecycle")
        case.parent_identity = "a"
        case.api_key = "synthetic"
        case.runner_token = "synthetic-runner"
        case.lock = threading.Lock()
        case.run_proofs = {"run": "original-record"}
        case.bases = ["https://plane-a.invalid", "https://plane-b.invalid"]
        case.evidence = {"live_generation_observations": []}
        snapshot = {
            "schema_version": 1,
            "sequence": 2,
            "status": "reported",
            "complete": False,
            "prompt_tokens": 7,
            "completion_tokens": 3,
            "source": "isolated_workload",
            "reported_generation": 4,
        }
        calls = []

        def handle(request):
            calls.append(request)
            path = request.url.path
            generation = request.headers.get("X-Runkite-Generation")
            if path.startswith("/threads/"):
                return httpx.Response(
                    200,
                    json={
                        "status": "success" if failure == "terminal" else "running",
                        "metadata": {"_runkite_attempt_generation": 4},
                    },
                )
            self.assertEqual(request.headers.get("X-Runkite-Run-Incarnation"), "original-record")
            if generation != "4":
                return httpx.Response(403, json={"reason_code": "run_generation_mismatch"})
            if path.endswith("/usage"):
                if failure == "current_usage_denied":
                    return httpx.Response(403)
                return httpx.Response(200, json={"snapshot": snapshot})
            return httpx.Response(
                200,
                json={
                    "intent": {
                        "generation": 4,
                        "run_id": "run",
                        "run_incarnation_id": "wrong-record" if failure == "wrong_record" else "original-record",
                    }
                },
                headers={
                    "ETag": 'W/"2"' if failure == "weak_etag" else '"2"',
                    "X-Runkite-Checkpoint-Framework": "isolated-v1",
                },
            )

        with httpx.Client(transport=httpx.MockTransport(handle)) as client:
            case.http = client
            case.client_for = lambda _: client
            if failure:
                with self.assertRaises(AssertionError):
                    case.qualify_live_generation_denials("thread", "run", snapshot)
                self.assertEqual(case.evidence["live_generation_observations"], [])
            else:
                case.qualify_live_generation_denials("thread", "run", snapshot)
        return case.evidence, calls

    def test_live_positive_controls_precede_mismatched_generation_writes(self):
        evidence, calls = self.invoke()
        self.assertEqual(len(evidence["live_generation_observations"]), 1)
        row = evidence["live_generation_observations"][0]
        self.assertEqual((row["generation"], row["mismatched_generation"]), (4, 3))
        self.assertEqual(
            (row["usage_idempotent_post"], row["usage_mismatched_post"], row["checkpoint_mismatched_put"]),
            (200, 403, 403),
        )
        self.assertTrue(row["checkpoint_unchanged"] and row["usage_unchanged"])
        writes = [
            (r.method, r.url.path, r.headers.get("X-Runkite-Generation")) for r in calls if r.method in {"POST", "PUT"}
        ]
        self.assertEqual(
            writes,
            [
                ("POST", "/internal/runs/run/usage", "4"),
                ("POST", "/internal/runs/run/usage", "3"),
                ("PUT", "/internal/checkpoints/thread/adapter-state", "3"),
            ],
        )

    def test_terminal_run_denied_positive_control_or_weak_etag_cannot_claim_success(self):
        for failure in ("terminal", "current_usage_denied", "weak_etag", "wrong_record"):
            with self.subTest(failure=failure):
                self.invoke(failure=failure)


class ManagedCancellationPositiveControlTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.managed = load_managed_helpers()

    def test_presence_requires_exact_nonempty_complete_read_only_listing(self):
        sandbox = "rk-" + "a" * 16
        result = subprocess.CompletedProcess([], 0, json.dumps({"sandboxes": [{"name": sandbox}]}).encode())
        with patch.object(self.managed.subprocess, "run", return_value=result) as execute:
            self.managed.wait_for_owned_sandbox_presence(
                "/owned/openshell", "https://127.0.0.1:1234", sandbox, {}, timeout=1
            )
        self.assertIn("list", execute.call_args.args[0])
        self.assertNotIn("delete", execute.call_args.args[0])
        self.assertEqual(execute.call_args.args[0][-1], "runkite_attempt=" + sandbox)
        for listing in (
            {"sandboxes": []},
            {"sandboxes": [{"name": "unrelated"}]},
            {"sandboxes": [{"name": sandbox}], "next_page_token": "incomplete"},
        ):
            value = subprocess.CompletedProcess([], 0, json.dumps(listing).encode())
            with (
                self.subTest(listing=listing),
                patch.object(self.managed.subprocess, "run", return_value=value),
                patch.object(self.managed.time, "sleep"),
                patch.object(self.managed.time, "monotonic", side_effect=[0, 0, 0, 0, 2, 2]),
                self.assertRaises(AssertionError),
            ):
                self.managed.wait_for_owned_sandbox_presence(
                    "/owned/openshell", "https://127.0.0.1:1234", sandbox, {}, timeout=1
                )

    def invoke(self, failure=None):
        case = self.managed.ManagedIsolatedE2E("test_managed_runtime_lifecycle")
        case.cli, case.gateway, case.environment = "/owned/openshell", "https://127.0.0.1:1234", {}
        case.runner_token = "synthetic-runner"
        case.lock = threading.Lock()
        case.run_proofs = {"run": "original-record"}
        sandbox = "rk-" + "a" * 16
        case.runtime_names = {sandbox}
        case.evidence = {"cancellation_observations": []}
        report = {
            "reported_generation": 4,
            "source": "isolated_workload",
            "status": "reported",
            "complete": False,
            "prompt_tokens": 5,
            "completion_tokens": 2,
        }
        intent = {
            "intent": {"run_id": "run", "run_incarnation_id": "original-record", "generation": 4, "sandbox": sandbox}
        }
        events = []
        cancelled = False
        live_reads = 0

        def request(path, body=None, **kwargs):
            nonlocal cancelled, live_reads
            if path.endswith("/cancel"):
                events.append("cancel")
                cancelled = True
                return None
            if not cancelled:
                live_reads += 1
            status = "interrupted" if cancelled else "success" if failure == "not_live" else "running"
            generation = (
                5
                if failure == "wrong_live_generation" or (failure == "changed_after_presence" and live_reads > 1)
                else 4
            )
            return {"status": status, "metadata": {"_runkite_attempt_generation": generation}}

        case.request = request

        def presence(*args, **kwargs):
            events.append("presence")
            if failure in {"empty_presence", "foreign_presence", "incomplete_presence"}:
                raise AssertionError("owned selector presence not established")
            return 0.001

        def absence(*args, **kwargs):
            events.append("absence")
            return 0.002

        if failure == "no_activity":
            report["source"] = "unknown"
        if failure == "wrong_report_generation":
            report["reported_generation"] = 3
        if failure == "wrong_original_record":
            intent["intent"]["run_incarnation_id"] = "replacement-record"
        with (
            patch.object(self.managed, "wait_for_owned_sandbox_presence", side_effect=presence),
            patch.object(self.managed, "wait_for_owned_sandbox_absence", side_effect=absence),
        ):
            if failure:
                with self.assertRaises(AssertionError):
                    case.cancel_active_workload("thread", "run", report, intent, 5, 2)
                self.assertNotIn("cancel", events)
                self.assertNotIn("absence", events)
                self.assertIn(sandbox, case.runtime_names)
                self.assertEqual(case.evidence["cancellation_observations"], [])
            else:
                case.cancel_active_workload("thread", "run", report, intent, 5, 2)
                self.assertEqual(events, ["presence", "cancel", "absence"])
                self.assertNotIn(sandbox, case.runtime_names)
                row = case.evidence["cancellation_observations"][0]
                for key in (
                    "observed_present_before_cancel",
                    "run_live_before_presence",
                    "run_live_after_presence",
                    "generation_matched_before_cancel",
                    "workload_activity_observed",
                    "observed_absent_before_teardown",
                ):
                    self.assertIs(row[key], True)
                self.assertEqual((row["activity_prompt_tokens"], row["activity_completion_tokens"]), (5, 2))

    def test_cancel_follows_activity_live_generation_and_proven_presence(self):
        self.invoke()

    def test_cancel_never_issued_without_presence_activity_and_matching_live_generation(self):
        for failure in (
            "empty_presence",
            "foreign_presence",
            "incomplete_presence",
            "no_activity",
            "not_live",
            "wrong_live_generation",
            "wrong_report_generation",
            "wrong_original_record",
            "changed_after_presence",
        ):
            with self.subTest(failure=failure):
                self.invoke(failure)
