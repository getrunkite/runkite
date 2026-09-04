#!/usr/bin/env python3
"""Daemonize a command so it survives the parent Cursor/agent shell exiting."""

from __future__ import annotations

import os
import sys


def main() -> None:
    if len(sys.argv) < 3:
        print("usage: daemonize.py LOGFILE CMD [ARGS...]", file=sys.stderr)
        sys.exit(2)
    logfile = os.path.abspath(sys.argv[1])
    cmd = sys.argv[2:]
    # Keep caller's cwd (repo root) — relative binary paths like ./runkite
    # and sqlite ./runkite.db must resolve. Only detach the session.
    cwd = os.getcwd()

    if os.fork() > 0:
        sys.exit(0)
    os.setsid()
    if os.fork() > 0:
        sys.exit(0)

    os.chdir(cwd)
    fd = os.open(logfile, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
    os.dup2(fd, 1)
    os.dup2(fd, 2)
    os.close(fd)
    devnull = os.open("/dev/null", os.O_RDONLY)
    os.dup2(devnull, 0)
    os.close(devnull)
    os.execvp(cmd[0], cmd)


if __name__ == "__main__":
    main()
