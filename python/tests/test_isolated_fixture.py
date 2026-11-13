"""Failure-path qualification for owned runtime cleanup, without Docker calls."""

import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

ROOT = Path(__file__).resolve().parents[2]


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
        def execute(args, **kwargs):
            value = self.command(args, **kwargs)
            if "list" in args:
                value.stdout = json.dumps({"sandboxes": [{"name": "rk-" + "a" * 16}]})
            return value

        with (
            patch.object(fixture.subprocess, "run", side_effect=execute),
            patch.object(fixture.time, "monotonic", side_effect=[0, 0, 2]),
            patch.object(fixture.time, "sleep"),
        ):
            result = fixture.cleanup_fixture(self.base, self.cli, io.BytesIO(), self.directory, timeout=1)
        self.assertFalse(result["confirmed"])
        self.assertIn("runtime_delete_not_confirmed", result["reason_codes"])
        self.assertTrue(self.directory.exists())

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
        for child_status in (0, 7):
            with self.subTest(child_status=child_status), tempfile.TemporaryDirectory() as directory:
                socket_file = Path(directory) / "fixture-socket"
                socket_file.touch()
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
                self.assertEqual(gateway["ports"], ["127.0.0.1:12345:8080"])
                config = (runtime / "gateway.toml").read_text()
                self.assertIn('bind_address = "0.0.0.0:8080"', config)
                self.assertIn('supervisor_network_mode = "bridge"', config)
                self.assertIn('grpc_endpoint = "http://host.docker.internal:12345"', config)
                build = next(call for call in build_image.call_args_list if "build" in call.args[0])
                self.assertEqual(Path(build.kwargs["env"]["BUILDX_CONFIG"]).resolve(), (runtime / "buildx").resolve())

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
        with (
            patch("runkite_runner.isolated.OpenShellRuntime", return_value=runtime),
            patch.dict(os.environ, {"RUNKITE_TEST_ISOLATED_OUTPUT": str(output)}),
        ):
            with self.assertRaisesRegex(AssertionError, "cleanup was not confirmed"):
                case.cleanup()
        self.assertFalse(json.loads(output.read_text())["passed"])


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
