"""Image qualification failure paths, with no real Docker calls."""

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("runner_image_qualification", ROOT / "scripts/qualify-runner-image.py")
qualification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qualification)


class RunnerImageEvidenceTests(unittest.TestCase):
    def invoke(self, probe, cleanup, expected_error=None):
        inspected = subprocess.CompletedProcess([], 0, "sha256:" + "a" * 64 + "\n", "")
        with tempfile.TemporaryDirectory(prefix="runner-image-evidence-") as directory:
            output = Path(directory) / "evidence.json"
            with (
                patch.dict(os.environ, {"RUNKITE_TEST_RUNNER_IMAGE": "1"}),
                patch.object(sys, "argv", ["qualify", "--image", "synthetic-local-image", "--output", str(output)]),
                patch.object(qualification.subprocess, "run", side_effect=[inspected, probe, cleanup]) as execute,
            ):
                if expected_error:
                    with self.assertRaises(expected_error):
                        qualification.main()
                else:
                    qualification.main()
            evidence = json.loads(output.read_text())
        self.assertEqual(execute.call_count, 3)
        run = execute.call_args_list[1].args[0]
        removed = execute.call_args_list[2].args[0]
        name = run[run.index("--name") + 1]
        self.assertRegex(name, r"^runkite-image-[a-f0-9]{16}$")
        self.assertEqual(removed, ["docker", "rm", "--force", name])
        self.assertEqual(run[run.index("--network") + 1], "none")
        self.assertIn("--read-only", run)
        self.assertIn("--pull=never", run)
        return evidence

    def test_probe_success_requires_confirmed_cleanup(self):
        evidence = self.invoke(
            subprocess.CompletedProcess([], 0, '{"passed":true,"cases":["synthetic-native-probe"]}', ""),
            subprocess.CompletedProcess([], 0, "owned-container\n", ""),
        )
        self.assertTrue(evidence["passed"])
        self.assertEqual(evidence["cases"], ["synthetic-native-probe"])
        self.assertNotIn("cleanup_error", evidence)

    def test_cleanup_nonzero_invalidates_successful_native_probe(self):
        evidence = self.invoke(
            subprocess.CompletedProcess([], 0, '{"passed":true}', ""),
            subprocess.CompletedProcess([], 1, "", "synthetic cleanup rejected"),
            RuntimeError,
        )
        self.assertFalse(evidence["passed"])
        self.assertIn("synthetic cleanup rejected", evidence["cleanup_error"])

    def test_cleanup_timeout_invalidates_successful_native_probe(self):
        evidence = self.invoke(
            subprocess.CompletedProcess([], 0, '{"passed":true}', ""),
            subprocess.TimeoutExpired(["docker", "rm"], 30),
            subprocess.TimeoutExpired,
        )
        self.assertFalse(evidence["passed"])
        self.assertIn("timed out", evidence["cleanup_error"])

    def test_probe_failure_stays_failed_after_cleanup_succeeds(self):
        evidence = self.invoke(
            subprocess.CalledProcessError(1, ["docker", "run"], output="fixture output", stderr="fixture failure"),
            subprocess.CompletedProcess([], 0, "owned-container\n", ""),
            subprocess.CalledProcessError,
        )
        self.assertFalse(evidence["passed"])
        self.assertEqual(evidence["error"], "CalledProcessError")
        self.assertEqual(evidence["stdout"], "fixture output")
        self.assertEqual(evidence["stderr"], "fixture failure")
        self.assertNotIn("cleanup_error", evidence)


if __name__ == "__main__":
    unittest.main()
