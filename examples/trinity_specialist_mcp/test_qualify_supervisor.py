"""Real child-process cleanup tests. No network or model service is used."""

from __future__ import annotations

import importlib.util
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

specification = importlib.util.spec_from_file_location("trinity_qualify", Path(__file__).with_name("qualify.py"))
qualify = importlib.util.module_from_spec(specification)
specification.loader.exec_module(qualify)


def running(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    try:
        status = subprocess.run(["ps", "-p", str(pid), "-o", "stat="], capture_output=True, text=True, check=False)
    except (PermissionError, FileNotFoundError) as error:
        raise unittest.SkipTest(
            "Owned-process state inspection is unavailable; run the Ubuntu qualification"
        ) from error
    return bool(status.stdout.strip()) and not status.stdout.lstrip().startswith("Z")


class QualificationEnvironmentTests(unittest.TestCase):
    def test_default_go_caches_survive_without_forwarding_home_or_credentials(self):
        ambient = {
            "PATH": "/usr/bin",
            "HOME": "/local-go-home",
            "GH_TOKEN": "not-for-the-qualifier",
            "NODE_AUTH_TOKEN": "not-for-the-qualifier",
            "PYPI_API_TOKEN": "not-for-the-qualifier",
        }
        caches = {
            "GOCACHE": "/local-go-home/.cache/go-build",
            "GOMODCACHE": "/local-go-home/go/pkg/mod",
            "GOPATH": "/local-go-home/go",
        }
        repository = Path("/fixture/repository")
        with patch.object(
            qualify.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(caches))
        ) as query:
            environment = qualify.qualification_environment(repository, ambient)
        self.assertEqual(query.call_args.args[0], ["go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH"])
        options = query.call_args.kwargs
        self.assertEqual(options["cwd"], repository)
        self.assertEqual(options["timeout"], 10)
        self.assertTrue(options["check"])
        self.assertEqual(options["env"]["HOME"], ambient["HOME"])
        for candidate in (options["env"], environment):
            self.assertEqual(candidate["GOTOOLCHAIN"], "local")
            self.assertEqual(candidate["GOPROXY"], "off")
            for name in ("GH_TOKEN", "NODE_AUTH_TOKEN", "PYPI_API_TOKEN"):
                self.assertNotIn(name, candidate)
        self.assertNotIn("HOME", environment)
        for name, value in caches.items():
            self.assertEqual(environment[name], value)

    def test_operator_cache_overrides_are_passed_to_go_and_preserved(self):
        caches = {"GOCACHE": "/operator/build", "GOMODCACHE": "/operator/modules", "GOPATH": "/operator/go"}
        ambient = {"PATH": "/usr/bin", "HOME": "/unused-home", **caches}
        with patch.object(
            qualify.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, json.dumps(caches))
        ) as query:
            environment = qualify.qualification_environment(Path("/fixture/repository"), ambient)
        for name, value in caches.items():
            self.assertEqual(query.call_args.kwargs["env"][name], value)
            self.assertEqual(environment[name], value)
        self.assertNotIn("HOME", environment)

    def test_concurrent_log_creation_is_not_truncated_after_preflight(self):
        with tempfile.TemporaryDirectory(prefix="runkite-trinity-log-race-") as temporary:
            output = (Path(temporary) / "qualification.json").resolve()
            log = output.with_suffix(".json.test.log")
            preserved = b"Existing qualification evidence\n"
            original_exists = Path.exists

            def racing_exists(path):
                if path == log and not original_exists(path):
                    path.write_bytes(preserved)
                    return False
                return original_exists(path)

            arguments = [
                "qualify.py",
                "--python",
                sys.executable,
                "--bridge-binary",
                sys.executable,
                "--output",
                str(output),
            ]
            with (
                patch.object(sys, "argv", arguments),
                patch.object(Path, "exists", racing_exists),
                patch.object(qualify, "qualification_environment", return_value={}),
                patch.object(qualify, "run_owned") as command,
            ):
                with self.assertRaises(FileExistsError):
                    qualify.main()
            self.assertEqual(log.read_bytes(), preserved)
            command.assert_not_called()
            self.assertFalse(output.exists())


@unittest.skipUnless(hasattr(os, "killpg"), "POSIX process-group qualification")
class QualificationSupervisorTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="runkite-trinity-supervisor-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)

    def reap(self, process):
        if process.poll() is None:
            process.kill()
        process.wait(timeout=3)

    def assert_stopped(self, *pids):
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            if not any(running(pid) for pid in pids):
                return
            time.sleep(0.025)
        self.fail("An owned qualification process was left running")

    def command(self, exit_code=None):
        child = "import signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); time.sleep(60)"
        script = (
            "import json,os,pathlib,subprocess,sys,time; "
            f"child=subprocess.Popen([sys.executable,'-c',{child!r}]); "
            f"pathlib.Path({str(self.directory / 'owned.json')!r}).write_text(json.dumps([os.getpid(),child.pid])); "
            "time.sleep(0.2); " + ("time.sleep(60)" if exit_code is None else f"sys.exit({exit_code})")
        )
        return [sys.executable, "-c", script]

    def test_timeout_stops_owned_child_and_grandchild_only(self):
        unrelated = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(60)"])
        self.addCleanup(self.reap, unrelated)
        with (self.directory / "test.log").open("w") as log:
            with self.assertRaises(subprocess.TimeoutExpired):
                qualify.run_owned(
                    self.command(),
                    cwd=self.directory,
                    env={},
                    stdout=log,
                    timeout=1,
                    cleanup_grace=0.2,
                )
        self.assert_stopped(*json.loads((self.directory / "owned.json").read_text()))
        self.assertIsNone(unrelated.poll(), "cleanup affected an unrelated process")
        unrelated.terminate()
        unrelated.wait(timeout=3)

    def test_normal_and_nonzero_exit_remove_surviving_grandchildren(self):
        for exit_code in (0, 7):
            with self.subTest(exit_code=exit_code), (self.directory / "test.log").open("w") as log:
                completed = qualify.run_owned(
                    self.command(exit_code),
                    cwd=self.directory,
                    env={},
                    stdout=log,
                    timeout=5,
                    cleanup_grace=0.2,
                )
                self.assertEqual(completed.returncode, exit_code)
                self.assert_stopped(*json.loads((self.directory / "owned.json").read_text()))

    def test_interrupt_cleans_workers_before_propagating(self):
        driver = self.directory / "driver.py"
        driver.write_text(
            "import importlib.util,subprocess,sys\n"
            f"s=importlib.util.spec_from_file_location('q',{str(Path(qualify.__file__))!r})\n"
            "q=importlib.util.module_from_spec(s);s.loader.exec_module(q)\n"
            "try:\n"
            f" q.run_owned({self.command()!r},cwd={str(self.directory)!r},env={{}},stdout=subprocess.DEVNULL,timeout=30,cleanup_grace=0.2)\n"
            "except KeyboardInterrupt:\n"
            " sys.exit(130)\n"
        )
        process = subprocess.Popen([sys.executable, str(driver)])
        self.addCleanup(self.reap, process)
        deadline = time.monotonic() + 5
        while not (self.directory / "owned.json").exists():
            if time.monotonic() > deadline:
                self.fail("The owned process did not start")
            time.sleep(0.025)
        process.send_signal(signal.SIGINT)
        self.assertEqual(process.wait(timeout=5), 130)
        self.assert_stopped(*json.loads((self.directory / "owned.json").read_text()))


if __name__ == "__main__":
    unittest.main()
