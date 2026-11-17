#!/usr/bin/env python3
"""Run one test command in an owned POSIX session and bound descendant cleanup."""

import argparse
import math
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


def cleanup_group(group, grace=2, leader=None):
    # start_new_session makes the command's PID its process-group ID. Never
    # signal the supervisor's session or discover processes by name.
    if group <= 0 or group == os.getpgrp():
        raise RuntimeError("refusing to signal an unowned test process group")
    try:
        os.killpg(group, signal.SIGTERM)
    except ProcessLookupError:
        return
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        if leader is not None:
            leader.poll()
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
    parser.add_argument("--timeout", type=float, help="optional command deadline in seconds; timeout exits 124")
    parser.add_argument(
        "--timeout-grace", type=float, default=2, help="owned process-group TERM grace, from 0 to 120 seconds"
    )
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.timeout is not None and (not math.isfinite(args.timeout) or args.timeout <= 0):
        parser.error("timeout must be positive and finite")
    if not math.isfinite(args.timeout_grace) or not 0 <= args.timeout_grace <= 120:
        parser.error("timeout grace must be from 0 to 120 seconds")
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
            deadline = None if args.timeout is None else time.monotonic() + args.timeout
            timed_out = False
            while process.poll() is None and interrupted is None:
                if deadline is not None and time.monotonic() >= deadline:
                    timed_out = True
                    break
                forward_available(reader)
                time.sleep(0.05)
            if interrupted is not None:
                status = 128 + interrupted
            elif timed_out:
                status = 124
            else:
                code = process.wait()
                status = code if code >= 0 else 128 - code
        except FileNotFoundError:
            print("Review HA test command was not found", file=sys.stderr)
            status = 127
        finally:
            cleanup_failed = False
            if process is not None:
                try:
                    cleanup_group(process.pid, args.timeout_grace, process)
                except OSError:
                    cleanup_failed = True
                    # Keep the held child handle as a bounded fallback. A
                    # denied group operation does not establish group absence.
                    if process.poll() is None:
                        try:
                            process.kill()
                        except OSError:
                            pass
                try:
                    process.wait(timeout=1)
                except (OSError, subprocess.TimeoutExpired):
                    cleanup_failed = True
            if cleanup_failed:
                print("owned process-group cleanup unconfirmed", file=sys.stderr)
                if status == 0:
                    status = 1
            # A surviving writer must not extend a failed cleanup indefinitely.
            forward_available(reader, limit=1 << 20 if cleanup_failed else None)
    return status


if __name__ == "__main__":
    sys.exit(main())
