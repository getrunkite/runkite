#!/usr/bin/env python3
"""Qualify the stdio bridge with real Runkite APIs and a real local worker.

The Go qualifier binds HTTP and gRPC only on ephemeral loopback sockets. It
exercises the official MCP SDK and copied Trinity launcher layout, not a live
Trinity runtime or the Go production CLI. No provider account is used.
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path


def _stop_owned_group(process: subprocess.Popen, grace_seconds: float) -> None:
    """Clean this command's new process group, including surviving descendants."""
    process_group = process.pid
    try:
        os.killpg(process_group, signal.SIGTERM)
    except ProcessLookupError:
        process.wait()
        return
    deadline = time.monotonic() + grace_seconds
    while time.monotonic() < deadline:
        process.poll()  # Reap the direct child while allowing descendants to exit.
        try:
            os.killpg(process_group, 0)
        except ProcessLookupError:
            process.wait()
            return
        time.sleep(0.025)
    try:
        os.killpg(process_group, signal.SIGKILL)
    except ProcessLookupError:
        pass
    process.wait(timeout=2)


def run_owned(command, *, cwd, env, stdout, timeout, cleanup_grace=2.0):
    """Run a command without leaving its workers behind if the command fails."""
    process = subprocess.Popen(
        command,
        cwd=cwd,
        env=env,
        stdout=stdout,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    try:
        returncode = process.wait(timeout=timeout)
        return subprocess.CompletedProcess(command, returncode)
    finally:
        # Go tests normally clean their workers themselves. A panic, timeout or
        # interrupted supervisor can bypass that cleanup; only this command's
        # explicitly created process group receives these termination signals.
        _stop_owned_group(process, cleanup_grace)


def qualification_environment(repository: Path, ambient=None) -> dict[str, str]:
    """Discover the existing Go caches before removing the caller's home."""
    ambient = os.environ if ambient is None else ambient
    cache_names = ("GOCACHE", "GOMODCACHE", "GOPATH")
    environment = {
        name: ambient[name] for name in ("PATH", "TMPDIR", "LANG", "SYSTEMROOT", *cache_names) if name in ambient
    }
    environment.update(CGO_ENABLED="0", GOPROXY="off", GOTOOLCHAIN="local", PYTHONDONTWRITEBYTECODE="1")
    query_environment = dict(environment)
    # Only the bounded local Go query needs the caller's config/home paths.
    # Builds and workers receive explicit caches, never these paths or tokens.
    for name in ("HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "GOENV"):
        if name in ambient:
            query_environment[name] = ambient[name]
    try:
        queried = subprocess.run(
            ["go", "env", "-json", *cache_names],
            cwd=repository,
            env=query_environment,
            capture_output=True,
            text=True,
            check=True,
            timeout=10,
        )
        caches = json.loads(queried.stdout)
    except (OSError, subprocess.SubprocessError, ValueError) as error:
        raise RuntimeError("Could not discover existing local Go caches") from error
    if not isinstance(caches, dict) or set(caches) != set(cache_names):
        raise RuntimeError("Local Go cache discovery returned an unexpected response")
    for name in cache_names:
        value = caches[name]
        paths = value.split(os.pathsep) if name == "GOPATH" and isinstance(value, str) else [value]
        if not isinstance(value, str) or not value or any(not Path(path).is_absolute() for path in paths):
            raise RuntimeError("Qualification requires absolute local Go cache paths")
        environment[name] = value
    return environment


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--python",
        type=Path,
        default=Path(sys.executable),
        help="Python interpreter with the existing Runkite LangGraph runner dependencies installed",
    )
    parser.add_argument(
        "--bridge-binary", type=Path, help="Use a previously built bridge instead of building this checkout"
    )
    parser.add_argument(
        "--output",
        type=Path,
        required=True,
        help="New JSON qualification receipt path; existing files are never replaced",
    )
    arguments = parser.parse_args()
    output = arguments.output.resolve()
    if output.exists():
        parser.error("choose a new output path instead of overwriting a qualification receipt")
    # Keep a virtual environment's executable path. Resolving its symlink to
    # the base interpreter would silently discard the installed dependencies.
    python = Path(os.path.abspath(arguments.python))
    if not python.is_file() or not os.access(python, os.X_OK):
        parser.error("--python must name an existing Python executable")
    repository = Path(__file__).resolve().parents[2]
    output.parent.mkdir(parents=True, exist_ok=True)
    environment = qualification_environment(repository)
    # Existing module/dependency caches are prerequisites. This qualifier does
    # not install packages, download a Go toolchain or read publishing tokens.
    with tempfile.TemporaryDirectory(prefix="runkite-trinity-qualification-") as temporary:
        binary = (
            arguments.bridge_binary.resolve() if arguments.bridge_binary else Path(temporary) / "runkite-specialist-mcp"
        )
        if arguments.bridge_binary and not binary.is_file():
            parser.error("--bridge-binary must name an existing bridge executable")
        build_log = output.with_suffix(output.suffix + ".build.log")
        test_log = output.with_suffix(output.suffix + ".test.log")
        if build_log.exists() or test_log.exists():
            parser.error("qualification log paths already exist; choose a new output path")
        if not arguments.bridge_binary:
            with build_log.open("x") as log:
                build = run_owned(
                    ["go", "build", "-o", str(binary), "./examples/trinity_specialist_mcp"],
                    cwd=repository,
                    env=environment,
                    stdout=log,
                    timeout=180,
                )
            if build.returncode:
                print(f"Bridge build failed; see {build_log}", file=sys.stderr)
                return build.returncode
        environment.update(
            RUNKITE_TEST_TRINITY="1",
            RUNKITE_TEST_TRINITY_BRIDGE_BINARY=str(binary),
            RUNKITE_TEST_TRINITY_PYTHON=str(python),
            RUNKITE_TEST_TRINITY_OUTPUT=str(output),
        )
        with test_log.open("x") as log:
            test = run_owned(
                [
                    "go",
                    "test",
                    "./internal/api",
                    "-run",
                    "^TestTrinitySpecialistQualification$",
                    "-count=1",
                    "-v",
                    "-timeout=4m",
                ],
                cwd=repository,
                env=environment,
                stdout=log,
                timeout=260,
            )
        if test.returncode:
            print(f"Qualification failed; see {test_log}", file=sys.stderr)
            return test.returncode
        if not output.is_file() or json.loads(output.read_text()).get("passed") is not True:
            raise RuntimeError("Go test did not produce a passing qualification receipt")
    print(f"Provider-free qualification passed: {output}")
    print(
        "Scope: real SQLite API/gRPC fixture and Python worker; official MCP stdio client and copied launcher, not the Trinity runtime."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
