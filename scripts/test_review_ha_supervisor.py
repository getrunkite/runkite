#!/usr/bin/env python3
"""Fixture-only checks for process ownership and HA wrapper interruption."""

import fcntl
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
SUPERVISOR = SCRIPTS / "review-ha-supervisor.py"


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
