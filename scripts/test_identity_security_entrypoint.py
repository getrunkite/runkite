"""Qualification wrapper control tests with fake tools, not qualification proof."""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts/test-identity-security.sh"


class EntrypointTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.fixture = self.directory / "fixture"
        (self.fixture / "scripts").mkdir(parents=True)
        self.script = self.fixture / "scripts/test-identity-security.sh"
        shutil.copy2(SCRIPT, self.script)
        runner = self.fixture / "typescript/runkite-runner"
        (runner / "node_modules/tsx").mkdir(parents=True)
        (runner / "src").mkdir()
        (runner / "src/fixture.test.ts").write_text("// Synthetic wrapper control fixture.\n")
        binary_dir = self.directory / "bin"
        binary_dir.mkdir()
        git = binary_dir / "git"
        git.write_text(
            "#!/bin/bash\nif [[ $3 == rev-parse ]]; then printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\\n'; fi\n"
        )
        git.chmod(0o700)
        self.trace = self.directory / "trace.jsonl"
        self.fail = self.directory / "fail"
        self.output = self.directory / "output"
        self.output.mkdir()
        self.env = {name: os.environ[name] for name in ("PATH", "HOME", "TMPDIR") if name in os.environ}
        self.env.update(
            PATH=str(binary_dir) + os.pathsep + self.env["PATH"],
            RUNKITE_TEST_IDENTITY_OUTPUT=str(self.output),
            POSTGRES_DSN="postgres://synthetic-sensitive-sentinel@outside.example/production",
            REDIS_URL="redis://outside.example/0",
            OPENAI_API_KEY="synthetic-sensitive-sentinel",
            RUNNER_TOKEN_OTHER="synthetic-sensitive-sentinel",
        )
        for name in ("go", "python", "node"):
            file = self.directory / name
            file.write_text(
                f"#!{sys.executable}\n"
                "import json,os,sys\n"
                f"name={name!r}\ntrace={str(self.trace)!r}\n"
                "with open(trace,'a') as out: out.write(json.dumps({'name':name,'args':sys.argv[1:],'keys':sorted(os.environ),'postgres':os.environ.get('POSTGRES_DSN'),'redis':os.environ.get('REDIS_URL')})+'\\n')\n"
                f"if name=='python' and len(sys.argv)>1 and sys.argv[1] in ('-','--version'): os.execv({sys.executable!r},[{sys.executable!r}]+sys.argv[1:])\n"
                f"if name=='go' and len(sys.argv)>1 and sys.argv[1]=='test' and os.path.exists({str(self.fail)!r}): sys.exit(7)\n"
            )
            file.chmod(0o700)
            self.env[f"RUNKITE_TEST_{name.upper()}"] = str(file)

    def run_wrapper(self, *arguments):
        return subprocess.run(
            ["bash", str(self.script), *arguments],
            cwd=self.fixture,
            env=self.env,
            text=True,
            capture_output=True,
            timeout=30,
        )

    def result(self):
        reports = list(self.output.glob("identity-security.*/result.json"))
        self.assertEqual(len(reports), 1)
        return json.loads(reports[0].read_text())

    def test_subprocess_environment_excludes_inherited_authority_and_database(self):
        result = self.run_wrapper()
        self.assertEqual(result.returncode, 0, result.stderr)
        report = self.result()
        self.assertTrue(report["passed"])
        self.assertEqual(report["independent_assessment"], "pending")
        trace = [json.loads(line) for line in self.trace.read_text().splitlines()]
        go_tests = [entry for entry in trace if entry["name"] == "go" and "test" in entry["args"]]
        self.assertEqual(len(go_tests), 2)
        for entry in go_tests:
            self.assertIsNone(entry["postgres"])
            self.assertIsNone(entry["redis"])
            self.assertNotIn("OPENAI_API_KEY", entry["keys"])
            self.assertNotIn("RUNNER_TOKEN_OTHER", entry["keys"])
        logs = "".join(path.read_text() for path in self.output.glob("*/qualification.log"))
        self.assertNotIn("synthetic-sensitive-sentinel", result.stdout + result.stderr + logs)

    def test_failed_gate_is_nonzero_recorded_and_stops_later_phases(self):
        self.fail.touch()
        result = self.run_wrapper()
        self.assertEqual(result.returncode, 7, result.stderr)
        report = self.result()
        self.assertFalse(report["passed"])
        self.assertEqual(report["last_phase"], "go_identity_and_security")
        self.assertNotIn("typescript_runner", result.stdout)

    def test_live_requires_explicit_dedicated_loopback_endpoints(self):
        self.assertNotEqual(self.run_wrapper("--live").returncode, 0)
        self.env.update(
            RUNKITE_TEST_IDENTITY_DISPOSABLE="1",
            RUNKITE_TEST_IDENTITY_POSTGRES_DSN="postgres://fixture:fixture@203.0.113.1:15432/runkite_identity_test",
            RUNKITE_TEST_IDENTITY_REDIS_URL="redis://127.0.0.1:16379/0",
        )
        self.assertNotEqual(self.run_wrapper("--live").returncode, 0)
        self.env["RUNKITE_TEST_IDENTITY_POSTGRES_DSN"] = "postgres://fixture:fixture@127.0.0.1:15432/production"
        self.assertNotEqual(self.run_wrapper("--live").returncode, 0)
        self.env["RUNKITE_TEST_IDENTITY_POSTGRES_DSN"] = (
            "postgres://fixture:fixture@127.0.0.1:15432/runkite_identity_test?sslmode=disable"
        )
        result = self.run_wrapper("--live")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.result()["live_postgres_redis"])
        trace = [json.loads(line) for line in self.trace.read_text().splitlines()]
        final = [entry for entry in trace if entry["name"] == "go" and "test" in entry["args"]][-1]
        self.assertEqual(final["postgres"], self.env["RUNKITE_TEST_IDENTITY_POSTGRES_DSN"])
        self.assertEqual(final["redis"], self.env["RUNKITE_TEST_IDENTITY_REDIS_URL"])


if __name__ == "__main__":
    unittest.main()
