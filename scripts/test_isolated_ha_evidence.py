"""Offline receipt validation, not native HA qualification."""

import copy
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

from isolated_ha_evidence import (
    SCOPE,
    EvidenceError,
    record_artifacts,
    runtime_from_base,
    validate_evidence,
    write_report,
)


def measured_guards(repetitions):
    return {
        "cancellation_observations": [
            {
                "run_id": f"synthetic-run-{i}",
                "generation": 2,
                "sandbox": "rk-" + f"{i:016x}",
                "observed_present_before_cancel": True,
                "run_live_before_presence": True,
                "run_live_after_presence": True,
                "generation_matched_before_cancel": True,
                "workload_activity_observed": True,
                "activity_prompt_tokens": 5 if i == 0 else 7,
                "activity_completion_tokens": 2 if i == 0 else 3,
                "observed_absent_before_teardown": True,
                "method": "read_only_selector",
                "elapsed_seconds": 0.1,
            }
            for i in range(repetitions + 1)
        ],
        "live_generation_observations": [
            {
                "run_id": f"synthetic-run-{i}",
                "generation": 2,
                "mismatched_generation": 1,
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
            for i in range(1, repetitions + 1)
        ],
    }


class EvidenceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.runtime = self.root / "run.fixture" / "runtime"
        self.runtime.mkdir(parents=True)
        self.reports = {
            "managed-evidence.json": {
                "passed": True,
                "scope": SCOPE,
                "runtime_gateway_tls": True,
                "bounded_repetitions": 2,
                "elapsed_seconds": 4.5,
                "cases": ["native lifecycle", "bounded cycle 1: measured", "bounded cycle 2: measured"],
            },
            "runtime-fixture.json": {"gateway_tls": True, "client_auth": "mtls", "release": "0.1.8"},
            "runtime-cleanup.json": {"confirmed": True, "reason_codes": []},
            "gateway-tls-evidence.json": {"passed": True, "cases": [f"probe {n}" for n in range(5)]},
        }
        self.reports["managed-evidence.json"].update(measured_guards(2))
        self.flush()

    def flush(self):
        for name, value in self.reports.items():
            (self.runtime / name).write_text(json.dumps(value))

    def test_complete_native_source_and_optional_typescript(self):
        source = self.root / "source.json"
        source.write_text(
            json.dumps(
                {
                    "revision": "a" * 40,
                    "version": "0.9.0",
                    "architecture": "x86_64",
                    "kernel": "Linux fixture",
                    "repetitions": 2,
                    "assessment": "maintainer combined fixture, not independent review",
                }
            )
        )
        typescript = self.root / "typescript.json"
        typescript.write_text(json.dumps({"passed": True, "cases": [f"case {n}" for n in range(4)]}))
        backend = self.root / "backend.json"
        backend.write_text(json.dumps({"confirmed": True, "reason_codes": []}))
        result = validate_evidence(self.runtime, 2, source=source, typescript=typescript, backend_cleanup=backend)
        self.assertTrue(result["passed"])
        self.assertTrue(result["backend_cleanup_confirmed"])
        self.assertEqual(result["typescript_cases"], 4)
        self.assertEqual(result["source_context"]["revision"], "a" * 40)
        self.assertEqual(runtime_from_base(self.root), self.runtime)

    def test_missing_or_skipped_execution_cannot_pass(self):
        for name in self.reports:
            with self.subTest(name=name):
                path = self.runtime / name
                path.unlink()
                with self.assertRaises(EvidenceError):
                    validate_evidence(self.runtime, 2)
                self.flush()

    def test_required_measured_contracts_fail_closed(self):
        cases = [
            ("managed-evidence.json", "passed", False),
            ("managed-evidence.json", "scope", "SQLite fixture"),
            ("managed-evidence.json", "runtime_gateway_tls", False),
            ("managed-evidence.json", "bounded_repetitions", True),
            ("managed-evidence.json", "bounded_repetitions", 1),
            ("managed-evidence.json", "elapsed_seconds", 0),
            (
                "managed-evidence.json",
                "cases",
                ["native lifecycle", "bounded cycle 1: measured", "bounded cycle 3: measured"],
            ),
            (
                "managed-evidence.json",
                "cases",
                ["native lifecycle", "bounded cycle 1: measured", "bounded cycle 1: duplicate"],
            ),
            ("runtime-fixture.json", "gateway_tls", False),
            ("runtime-fixture.json", "client_auth", "none"),
            ("runtime-fixture.json", "release", "0.0.0"),
            ("runtime-cleanup.json", "confirmed", False),
            ("runtime-cleanup.json", "reason_codes", ["unconfirmed"]),
            ("gateway-tls-evidence.json", "passed", False),
            ("gateway-tls-evidence.json", "cases", ["one probe"]),
        ]
        original = copy.deepcopy(self.reports)
        for name, field, value in cases:
            with self.subTest(name=name, field=field):
                self.reports = copy.deepcopy(original)
                self.reports[name][field] = value
                self.flush()
                with self.assertRaises(EvidenceError):
                    validate_evidence(self.runtime, 2)

    def test_malformed_duplicate_nonfinite_and_symlink_receipts(self):
        path = self.runtime / "managed-evidence.json"
        for raw in ('{"passed":true,"passed":true}', '{"elapsed_seconds":NaN}', "[]", "not JSON"):
            path.write_text(raw)
            with self.assertRaises(EvidenceError):
                validate_evidence(self.runtime, 2)
        path.unlink()
        path.symlink_to(self.root / "not-read")
        with self.assertRaises(EvidenceError):
            validate_evidence(self.runtime, 2)

    def test_absence_and_live_generation_proofs_cannot_be_incomplete(self):
        original = copy.deepcopy(self.reports)
        for group, field, value in (
            ("cancellation_observations", "observed_present_before_cancel", False),
            ("cancellation_observations", "run_live_before_presence", False),
            ("cancellation_observations", "run_live_after_presence", False),
            ("cancellation_observations", "generation_matched_before_cancel", False),
            ("cancellation_observations", "workload_activity_observed", False),
            ("cancellation_observations", "activity_prompt_tokens", 0),
            ("cancellation_observations", "activity_completion_tokens", True),
            ("cancellation_observations", "observed_absent_before_teardown", False),
            ("cancellation_observations", "method", "after_teardown"),
            ("live_generation_observations", "live_after", False),
            ("live_generation_observations", "usage_mismatched_post", 200),
            ("live_generation_observations", "checkpoint_unchanged", False),
        ):
            with self.subTest(group=group, field=field):
                self.reports = copy.deepcopy(original)
                self.reports["managed-evidence.json"][group][0][field] = value
                self.flush()
                with self.assertRaises(EvidenceError):
                    validate_evidence(self.runtime, 2)
        for field in (
            "observed_present_before_cancel",
            "run_live_before_presence",
            "run_live_after_presence",
            "generation_matched_before_cancel",
            "workload_activity_observed",
            "activity_prompt_tokens",
            "activity_completion_tokens",
        ):
            with self.subTest(missing=field):
                self.reports = copy.deepcopy(original)
                self.reports["managed-evidence.json"]["cancellation_observations"][0].pop(field)
                self.flush()
                with self.assertRaises(EvidenceError):
                    validate_evidence(self.runtime, 2)
        self.reports = copy.deepcopy(original)
        self.reports["managed-evidence.json"]["cancellation_observations"][1]["activity_prompt_tokens"] = 5
        self.flush()
        with self.assertRaises(EvidenceError):
            validate_evidence(self.runtime, 2)
        self.reports = copy.deepcopy(original)
        self.reports["managed-evidence.json"].pop("cancellation_observations")
        self.flush()
        with self.assertRaises(EvidenceError):
            validate_evidence(self.runtime, 2)

    def test_source_and_typescript_failures_remain_required_when_selected(self):
        source = self.root / "source.json"
        for value in ({}, {"revision": "a" * 40}, {"revision": "synthetic-sensitive-value"}):
            source.write_text(json.dumps(value))
            with self.assertRaises(EvidenceError) as error:
                validate_evidence(self.runtime, 2, source=source)
            self.assertNotIn("synthetic-sensitive", str(error.exception))
        typescript = self.root / "typescript.json"
        for value in ({"passed": False, "cases": ["x"] * 4}, {"passed": True, "cases": []}):
            typescript.write_text(json.dumps(value))
            with self.assertRaises(EvidenceError):
                validate_evidence(self.runtime, 2, typescript=typescript)

    def test_run_selection_and_summary_never_reuse_existing_output(self):
        (self.root / "run.second").mkdir()
        with self.assertRaises(EvidenceError):
            runtime_from_base(self.root)
        output = self.root / "summary.json"
        write_report(output, {"passed": True})
        self.assertEqual(output.stat().st_mode & 0o777, 0o600)
        with self.assertRaises(EvidenceError):
            write_report(output, {"passed": False})
        self.assertEqual(json.loads(output.read_text()), {"passed": True})

    def test_prebuilt_artifact_hashes_are_separate_from_checkout_context(self):
        artifact = self.root / "artifact"
        artifact.write_bytes(b"synthetic prepared artifact")
        output = self.root / "artifacts.json"
        record_artifacts(output, artifact, artifact, artifact, self.root)
        result = validate_evidence(self.runtime, 2, artifacts=output)
        context = result["artifact_context"]
        self.assertEqual(context["binary_sha256"], context["runner_wheel_sha256"])
        self.assertEqual(len(context["openshell_cli_sha256"]), 64)
        self.assertIsNone(context["checkout_context"])
        self.assertNotIn(str(artifact), json.dumps(result))


class WrapperTests(unittest.TestCase):
    """Fake tools exercise wrapper controls, never native qualification."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fixture = self.root / "fixture"
        scripts = self.fixture / "scripts"
        scripts.mkdir(parents=True)
        source = Path(__file__).resolve().parent
        for name in (
            "test-isolated-ha.sh",
            "review-ha-supervisor.py",
            "isolated_fixture_environment.py",
            "isolated_ha_evidence.py",
        ):
            shutil.copy2(source / name, scripts / name)
        (self.fixture / "docker-compose.test.yml").write_text("services: {}\n")
        tools = self.root / "tools"
        tools.mkdir()
        self.docker = tools / "docker"
        for name in ("uv", "openssl", "runkite", "openshell"):
            file = tools / name
            file.write_text("#!/bin/sh\nexit 0\n")
            file.chmod(0o700)
        wheel = self.root / "prepared.whl"
        wheel.write_bytes(b"synthetic prepared wheel")
        self.output = self.root / "output"
        self.environment = {
            "PATH": str(tools) + os.pathsep + os.environ["PATH"],
            "RUNKITE_TEST_BINARY": str(tools / "runkite"),
            "RUNKITE_TEST_RUNNER_WHEEL": str(wheel),
            "RUNKITE_TEST_ISOLATED_CLI": str(tools / "openshell"),
            "RUNKITE_TEST_ISOLATED_PYTHON": sys.executable,
            "RUNKITE_TEST_ISOLATED_HA_OUTPUT": str(self.output),
            "RUNKITE_TEST_ISOLATED_REPETITIONS": "1",
            "RUNKITE_TEST_DOCKER_SOCKET": "/synthetic-owned.sock",
            "OPENAI_API_KEY": "synthetic-private-authority",
            "OTEL_EXPORTER_OTLP_ENDPOINT": "https://outside.invalid/otel",
            "AWS_ACCESS_KEY_ID": "synthetic-private-authority",
            "HOME": str(self.root / "unread-user-home"),
            "run_owned_exec": "1",
            "run_owned_reserve": "-999",
        }

    def prepare(self, mode):
        self.docker.write_text(f"""#!{sys.executable}
import os,signal,sys,time
assert 'OPENAI_API_KEY' not in os.environ and 'OTEL_EXPORTER_OTLP_ENDPOINT' not in os.environ
args=sys.argv[1:]
if {mode!r} == 'hang' and 'config' in args:
    signal.signal(signal.SIGTERM, signal.SIG_IGN)
    time.sleep(30)
if 'port' in args: print('127.0.0.1:15432' if 'postgres' in args else '127.0.0.1:16379')
""")
        self.docker.chmod(0o700)
        reports = {
            "managed-evidence.json": {
                "passed": True,
                "scope": SCOPE,
                "runtime_gateway_tls": True,
                "bounded_repetitions": 1,
                "elapsed_seconds": 1.0,
                "cases": ["native lifecycle", "bounded cycle 1: measured"],
            },
            "runtime-fixture.json": {"gateway_tls": True, "client_auth": "mtls", "release": "0.1.8"},
            "runtime-cleanup.json": {"confirmed": True, "reason_codes": []},
            "gateway-tls-evidence.json": {"passed": True, "cases": [f"probe {n}" for n in range(5)]},
        }
        reports["managed-evidence.json"].update(measured_guards(1))
        (self.fixture / "scripts/start-isolated-fixture.py").write_text(f"""import json,os,sys
from pathlib import Path
def local_docker_command(socket): return ['docker'], socket
if __name__ == '__main__':
    assert 'OPENAI_API_KEY' not in os.environ and 'AWS_ACCESS_KEY_ID' not in os.environ
    if {mode!r} == 'success':
        root=Path(sys.argv[sys.argv.index('--output-dir')+1]); root.mkdir(parents=True)
        for name,value in {reports!r}.items(): (root/name).write_text(json.dumps(value))
    else: print('Ran 0 tests. OK')
""")

    def run_wrapper(self, mode):
        self.prepare(mode)
        return subprocess.run(
            ["bash", str(self.fixture / "scripts/test-isolated-ha.sh")],
            env=self.environment,
            text=True,
            capture_output=True,
            timeout=15,
        )

    def test_success_requires_new_receipts_and_backend_removal(self):
        result = self.run_wrapper("success")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        runs = list(self.output.glob("run.*"))
        self.assertEqual(len(runs), 1)
        summary = json.loads((runs[0] / "evidence-summary.json").read_text())
        self.assertTrue(summary["passed"])
        self.assertTrue(summary["backend_cleanup_confirmed"])
        self.assertNotIn("synthetic-private-authority", result.stdout + result.stderr)

    def test_zero_tests_and_old_receipts_cannot_pass(self):
        self.output.mkdir()
        stale = self.output / "run.stale" / "runtime"
        stale.mkdir(parents=True)
        (stale / "managed-evidence.json").write_text('{"passed":true}')
        result = self.run_wrapper("skip")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(list(self.output.glob("run.*"))), 2)
        self.assertEqual(list(self.output.glob("run.*/evidence-summary.json")), [])
        self.assertEqual(json.loads((stale / "managed-evidence.json").read_text()), {"passed": True})

    def test_one_work_deadline_bounds_a_stuck_docker_command(self):
        self.environment["RUNKITE_TEST_ISOLATED_HA_TIMEOUT_SECONDS"] = "1"
        started = time.monotonic()
        result = self.run_wrapper("hang")
        self.assertEqual(result.returncode, 124, result.stdout + result.stderr)
        self.assertLess(time.monotonic() - started, 10)
        self.assertEqual(list(self.output.glob("run.*/evidence-summary.json")), [])

    def test_work_budget_cannot_overflow_shell_arithmetic(self):
        self.environment["RUNKITE_TEST_ISOLATED_HA_TIMEOUT_SECONDS"] = "9" * 100
        result = self.run_wrapper("skip")
        self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
        self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()
