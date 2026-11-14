#!/usr/bin/env python3
"""Run one test command in an owned POSIX session and bound descendant cleanup."""

import argparse
import os
import signal
import subprocess
import sys
import time
from pathlib import Path


def group_exists(group):
    try:
        os.killpg(group, 0)
        return True
    except ProcessLookupError:
        return False


def cleanup_group(group):
    # start_new_session makes the command's PID its process-group ID. Never
    # signal the supervisor's session or discover processes by name.
    if group <= 0 or group == os.getpgrp():
        raise RuntimeError("refusing to signal an unowned test process group")
    try:
        os.killpg(group, signal.SIGTERM)
    except ProcessLookupError:
        return
    deadline = time.monotonic() + 2
    while time.monotonic() < deadline:
        if not group_exists(group):
            return
        time.sleep(0.05)
    try:
        os.killpg(group, signal.SIGKILL)
    except ProcessLookupError:
        pass


def forward_available(reader, limit=1 << 20):
    while limit is None or limit > 0:
        data = reader.read(65536 if limit is None else min(65536, limit))
        if not data:
            return
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()
        if limit is not None:
            limit -= len(data)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--log", required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command:
        parser.error("a test command is required after --")

    interrupted = None

    def on_signal(number, _frame):
        nonlocal interrupted
        if interrupted is None:
            interrupted = number

    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGTERM, on_signal)
    process = None
    status = 1
    path = Path(args.log)
    with path.open("wb", buffering=0) as log, path.open("rb") as reader:
        try:
            process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            while process.poll() is None and interrupted is None:
                forward_available(reader)
                time.sleep(0.05)
            if interrupted is not None:
                status = 128 + interrupted
            else:
                code = process.wait()
                status = code if code >= 0 else 128 - code
        except FileNotFoundError:
            print("Review HA test command was not found", file=sys.stderr)
            status = 127
        finally:
            if process is not None:
                cleanup_group(process.pid)
                try:
                    process.wait(timeout=1)
                except subprocess.TimeoutExpired:
                    # The owned group has already received SIGKILL. Do not
                    # substitute a new process lookup for that stable group.
                    pass
            forward_available(reader, limit=None)
    return status


if __name__ == "__main__":
    sys.exit(main())
