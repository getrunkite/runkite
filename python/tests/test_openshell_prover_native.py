"""Opt-in native OpenShell checker controls, using only synthetic local policies."""

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DOMAINS = ["filesystem", "network_l4", "network_rest", "process", "landlock"]


@unittest.skipUnless(
    os.environ.get("RUNKITE_TEST_OPENSHELL_PROVER"), "set an explicitly verified local prover executable"
)
class NativeProverControls(unittest.TestCase):
    def test_real_release_distinguishes_containment_expansion_invalid_and_unsupported(self):
        prover = Path(os.environ["RUNKITE_TEST_OPENSHELL_PROVER"])
        self.assertTrue(prover.is_absolute() and prover.is_file())
        policy = (ROOT / "examples/isolated_agent/policy.yaml").read_text()
        self.assertIn('run_as_user: "1000"', policy)
        self.assertIn("include_workdir: true", policy)
        self.assertIn("[/usr,", policy)
        cases = (
            ("within", policy, 0, "within_boundary", None),
            ("root_user", policy.replace('run_as_user: "1000"', 'run_as_user: "0"'), 1, "exceeds_boundary", None),
            (
                "unknown_field",
                policy.replace("include_workdir: true", "include_workdir: true\n  unknown_fixture: true"),
                2,
                "error",
                "invalid_input",
            ),
            (
                "unresolved_path",
                policy.replace("[/usr,", "[/synthetic-unresolved-path, /usr,"),
                3,
                "unsupported",
                "unresolved_workdir",
            ),
        )
        records = []
        with tempfile.TemporaryDirectory(prefix="runkite-prover-controls-") as private:
            directory = Path(private)
            environment = {key: os.environ[key] for key in ("PATH", "SYSTEMROOT") if key in os.environ}
            environment.update(HOME=private, XDG_CONFIG_HOME=private, XDG_DATA_HOME=private)
            boundary = directory / "boundary.yaml"
            boundary.write_text(policy)
            version = subprocess.run([str(prover), "--version"], env=environment, capture_output=True, timeout=5)
            self.assertEqual(version.returncode, 0)
            self.assertEqual(version.stdout.strip(), b"openshell-prover 0.1.8")
            for label, candidate, code, result, reason in cases:
                with self.subTest(case=label):
                    source = directory / f"{label}.yaml"
                    source.write_text(candidate)
                    process = subprocess.run(
                        [
                            str(prover),
                            "check",
                            str(source),
                            "--boundary",
                            str(boundary),
                            "-o",
                            "json",
                            "--timeout",
                            "10s",
                        ],
                        env=environment,
                        capture_output=True,
                        timeout=15,
                    )
                    self.assertEqual(process.returncode, code)
                    self.assertLessEqual(len(process.stdout), 1 << 20)
                    response = json.loads(process.stdout)
                    self.assertEqual(response["schema_version"], 1)
                    self.assertEqual(response["prover_version"], "0.1.8")
                    self.assertEqual(response["check"], "boundary")
                    self.assertEqual(response["exit_code"], code)
                    self.assertEqual(response["result"], result)
                    self.assertEqual(response["reason_code"], reason)
                    self.assertEqual(response["inputs"], {"candidate": str(source), "boundary": str(boundary)})
                    self.assertEqual(response["coverage"], None if code == 2 else {"domains": DOMAINS})
                    if code == 0:
                        self.assertIsNone(response["counterexample"])
                    if code == 1:
                        self.assertEqual(response["counterexample"]["domain"], "process")
                    records.append({"case": label, "result": result, "exit_code": code, "reason_code": reason})
        if destination := os.environ.get("RUNKITE_TEST_OPENSHELL_PROVER_REPORT"):
            report = {"schema_version": 1, "passed": True, "prover_version": "0.1.8", "cases": records}
            descriptor = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "w") as output:
                json.dump(report, output, indent=2)
                output.write("\n")


if __name__ == "__main__":
    unittest.main()
