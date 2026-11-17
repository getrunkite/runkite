#!/usr/bin/env python3
"""Fixture-only checks for process ownership and HA wrapper interruption."""

import fcntl
import importlib.util
import io
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from contextlib import redirect_stderr
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parent
SUPERVISOR = SCRIPTS / "review-ha-supervisor.py"
SPEC = importlib.util.spec_from_file_location("review_ha_supervisor", SUPERVISOR)
SUPERVISOR_MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(SUPERVISOR_MODULE)


def descendant_running(marker):
    # The fixture holds this lock throughout its lifetime. Exit releases the
    # descriptor even when init has not reaped a zombie. No process scan is used.
    with (marker.parent / "descendant.lock").open("a") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return False
        except BlockingIOError:
            return True


def await_file(path):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if path.exists():
            return
        time.sleep(0.02)
    raise AssertionError("fixture process did not become ready")


class SupervisorTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="runkite-review-guard-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.processes = []
        self.groups = []
        self.addCleanup(self.stop_fixtures)

    def stop_fixtures(self):
        for process in self.processes:
            if process.poll() is None:
                process.terminate()
                try:
                    process.communicate(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.communicate(timeout=3)
        for marker in self.groups:
            if marker.exists():
                group = json.loads(marker.read_text())["group"]
                try:
                    os.killpg(group, signal.SIGKILL)
                except ProcessLookupError:
                    pass

    def start(self, command):
        process = subprocess.Popen(
            [sys.executable, str(SUPERVISOR), "--log", str(self.directory / "test.log"), "--"] + command,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        self.processes.append(process)
        return process

    def descendant_command(self, exit_code=None):
        marker = self.directory / "fixture-pids.json"
        ready = self.directory / "descendant-ready"
        lock = self.directory / "descendant.lock"
        self.groups.append(marker)
        child = (
            "import fcntl,pathlib,signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); "
            "lock=open(" + repr(str(lock)) + ",'a'); fcntl.flock(lock,fcntl.LOCK_EX); "
            "pathlib.Path(" + repr(str(ready)) + ").write_text('ready'); time.sleep(300)"
        )
        source = """import json,os,pathlib,subprocess,sys,time
child = subprocess.Popen([sys.executable, '-u', '-c', CHILD])
deadline = time.monotonic() + 4
while not pathlib.Path(READY).exists():
    if time.monotonic() > deadline: sys.exit(91)
    time.sleep(0.01)
pathlib.Path(MARKER).write_text(json.dumps({'group':os.getpgrp(),'child':child.pid}))
print('fixture command ready',flush=True)
FINISH
"""
        source = (
            source.replace("CHILD", repr(child)).replace("READY", repr(str(ready))).replace("MARKER", repr(str(marker)))
        )
        source = source.replace(
            "FINISH", "sys.exit(" + str(exit_code) + ")" if exit_code is not None else "time.sleep(300)"
        )
        return [sys.executable, "-u", "-c", source], marker

    def assert_child_stopped(self, marker):
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline and descendant_running(marker):
            time.sleep(0.02)
        self.assertFalse(descendant_running(marker), "owned descendant survived supervisor cleanup")

    def test_normal_exit_forwards_and_records_output(self):
        process = self.start([sys.executable, "-c", "print('fixture output')"])
        output, _ = process.communicate(timeout=8)
        self.assertEqual(process.returncode, 0, output)
        self.assertIn("fixture output", output)
        self.assertIn("fixture output", (self.directory / "test.log").read_text())

    def test_nonzero_exit_is_preserved(self):
        process = self.start([sys.executable, "-c", "import sys; print('fixture failed'); sys.exit(7)"])
        output, _ = process.communicate(timeout=8)
        self.assertEqual(process.returncode, 7, output)

    def run_with_denied_group_cleanup(self, command, killpg, timeout=None):
        arguments = [str(SUPERVISOR), "--log", str(self.directory / "denied.log")]
        if timeout is not None:
            arguments.extend(["--timeout", str(timeout)])
        arguments.extend(["--", *command])
        diagnostics = io.StringIO()
        popen = subprocess.Popen

        def start(*args, **kwargs):
            process = popen(*args, **kwargs)
            self.processes.append(process)
            return process

        with (
            mock.patch.object(sys, "argv", arguments),
            mock.patch.object(SUPERVISOR_MODULE.signal, "signal"),
            mock.patch.object(SUPERVISOR_MODULE.os, "killpg", side_effect=killpg),
            mock.patch.object(SUPERVISOR_MODULE.subprocess, "Popen", side_effect=start),
            mock.patch.object(SUPERVISOR_MODULE, "forward_available") as forward,
            redirect_stderr(diagnostics),
        ):
            status = SUPERVISOR_MODULE.main()
        self.assertEqual(diagnostics.getvalue(), "owned process-group cleanup unconfirmed\n")
        self.assertEqual(forward.call_args.kwargs["limit"], 1 << 20)
        self.assertIsNotNone(self.processes[-1].poll(), "held child survived direct cleanup fallback")
        return status

    def test_denied_cleanup_cannot_turn_a_success_into_pass(self):
        status = self.run_with_denied_group_cleanup(
            [sys.executable, "-c", "raise SystemExit(0)"], PermissionError("private host diagnostic")
        )
        self.assertEqual(status, 1)

    def test_denied_initial_term_preserves_timeout_and_kills_held_child(self):
        started = time.monotonic()
        status = self.run_with_denied_group_cleanup(
            [sys.executable, "-c", "import time; time.sleep(300)"], PermissionError(), timeout=0.1
        )
        self.assertEqual(status, 124)
        self.assertLess(time.monotonic() - started, 3)

    def test_denied_existence_probe_after_term_preserves_timeout(self):
        killpg = os.killpg

        def deny_probe(group, number):
            if number == 0:
                raise PermissionError("private host diagnostic")
            return killpg(group, number)

        status = self.run_with_denied_group_cleanup(
            [sys.executable, "-c", "import time; time.sleep(300)"], deny_probe, timeout=0.1
        )
        self.assertEqual(status, 124)

    def test_group_existence_permission_denial_remains_unknown_after_reap(self):
        leader = mock.Mock()
        leader.poll.return_value = 0
        with (
            mock.patch.object(SUPERVISOR_MODULE.os, "killpg", side_effect=[None, PermissionError()]),
            self.assertRaises(PermissionError),
        ):
            SUPERVISOR_MODULE.cleanup_group(os.getpgrp() + 1000, grace=110, leader=leader)
        leader.poll.assert_called_once_with()

    def test_deadline_terminates_owned_descendants_and_reports_timeout(self):
        command, marker = self.descendant_command()
        process = subprocess.Popen(
            [
                sys.executable,
                str(SUPERVISOR),
                "--timeout",
                "1",
                "--timeout-grace",
                "0.1",
                "--log",
                str(self.directory / "deadline.log"),
                "--",
                *command,
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        self.processes.append(process)
        output, _ = process.communicate(timeout=6)
        self.assertEqual(process.returncode, 124, output)
        self.assert_child_stopped(marker)

    def test_deadline_options_reject_nonfinite_or_unbounded_grace(self):
        for option, value in (
            ("--timeout", "nan"),
            ("--timeout", "inf"),
            ("--timeout", "0"),
            ("--timeout-grace", "-1"),
            ("--timeout-grace", "121"),
        ):
            result = subprocess.run(
                [
                    sys.executable,
                    str(SUPERVISOR),
                    option,
                    value,
                    "--log",
                    str(self.directory / "invalid.log"),
                    "--",
                    sys.executable,
                    "-c",
                    "raise SystemExit(91)",
                ],
                capture_output=True,
                timeout=5,
            )
            self.assertEqual(result.returncode, 2)

    def test_graceful_term_does_not_wait_for_the_full_long_grace(self):
        process = subprocess.Popen(
            [
                sys.executable,
                str(SUPERVISOR),
                "--timeout",
                "0.3",
                "--timeout-grace",
                "110",
                "--log",
                str(self.directory / "graceful.log"),
                "--",
                sys.executable,
                "-c",
                "import time; time.sleep(300)",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        self.processes.append(process)
        started = time.monotonic()
        output, _ = process.communicate(timeout=5)
        self.assertEqual(process.returncode, 124, output)
        self.assertLess(time.monotonic() - started, 4)

    def test_leader_exit_reaps_its_descendant_only(self):
        unrelated = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(300)"], start_new_session=True)
        self.processes.append(unrelated)
        command, marker = self.descendant_command(exit_code=2)
        process = self.start(command)
        output, _ = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 2, output)
        self.assert_child_stopped(marker)
        self.assertIsNone(unrelated.poll(), "cleanup signaled an unrelated process")

    def test_supervisor_term_cleans_descendants_and_preserves_signal_status(self):
        command, marker = self.descendant_command()
        process = self.start(command)
        await_file(marker)
        process.send_signal(signal.SIGTERM)
        output, _ = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 143, output)
        self.assert_child_stopped(marker)

    def test_wrapper_term_waits_for_owned_session_cleanup(self):
        command, marker = self.descendant_command()
        fixture = self.directory / "fixture.py"
        fixture.write_text(command[-1])
        tools = self.directory / "tools"
        tools.mkdir()
        docker = tools / "docker"
        docker.write_text("""#!/usr/bin/env bash
set -eu
printf '%s\\n' "$*" >> "$FIXTURE_DOCKER_CALLS"
for arg in "$@"; do
  case "$arg" in
    version) exit 0;;
    config) echo 'services: {}'; exit 0;;
    up|logs|down) exit 0;;
    port) if [[ "$*" == *'postgres 5432' ]]; then echo '127.0.0.1:15432'; else echo '127.0.0.1:16379'; fi; exit 0;;
  esac
done
exit 90
""")
        go = tools / "go"
        go.write_text('#!/usr/bin/env bash\nexec "$FIXTURE_PYTHON" -u "$FIXTURE_SCRIPT"\n')
        binary = self.directory / "fixture-runkite"
        binary.write_text("#!/bin/sh\nexit 0\n")
        for path in (docker, go, binary):
            path.chmod(0o755)
        calls = self.directory / "docker-calls.log"
        environment = dict(
            os.environ,
            PATH=str(tools) + os.pathsep + os.environ["PATH"],
            FIXTURE_PYTHON=sys.executable,
            FIXTURE_SCRIPT=str(fixture),
            FIXTURE_DOCKER_CALLS=str(calls),
            RUNKITE_TEST_BINARY=str(binary),
            RUNKITE_TEST_REVIEW_OUTPUT=str(self.directory / "output"),
            RUNKITE_TEST_REVIEW_REPETITIONS="1",
        )
        process = subprocess.Popen(
            ["bash", str(SCRIPTS / "test-review-ha.sh")],
            cwd=SCRIPTS.parent,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        self.processes.append(process)
        await_file(marker)
        process.send_signal(signal.SIGTERM)
        output, _ = process.communicate(timeout=12)
        self.assertEqual(process.returncode, 143, output)
        self.assert_child_stopped(marker)
        self.assertEqual(sum(" down " in line for line in calls.read_text().splitlines()), 1)


if __name__ == "__main__":
    unittest.main()
