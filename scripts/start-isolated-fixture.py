#!/usr/bin/env python3
"""Start one owned disposable external runtime, run a command, then clean it.

The trusted gateway alone receives the Docker socket. It uses verified release
digests and task-local data. This never installs services or registers a CLI.
"""

import argparse
import json
import os
import re
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time
import uuid
from pathlib import Path

GATEWAY = "ghcr.io/nvidia/openshell/gateway@sha256:d66352a54c680b652e5ddd7d19b9c1189568d18016f360b9b8c64014f13d89c4"
SANDBOX = "ghcr.io/nvidia/openshell/sandbox@sha256:f0fe7dbf18d833acb5da99c405231124718f23b524aa8e3c0108e6f37530033b"
SUPERVISOR = (
    "ghcr.io/nvidia/openshell/supervisor@sha256:deadeacfdba6d12ca27e538d0002696369d4de5a4fcad075ab503495503af9d1"
)


def cleanup_fixture(base, cli, log, directory, *, timeout=30, preserve_recovery=False):
    """Confirm owned runtime cleanup before discarding private recovery data."""
    errors = []
    runtime_empty = False
    deadline = time.monotonic() + timeout
    requested = set()
    while time.monotonic() < deadline:
        try:
            result = subprocess.run(
                [*cli, "sandbox", "list", "--output", "json"], capture_output=True, text=True, timeout=15
            )
            if result.returncode != 0:
                errors.append("runtime_list_failed")
                break
            listing = json.loads(result.stdout)
            sandboxes = listing.get("sandboxes")
            if not isinstance(sandboxes, list) or listing.get("next_page_token"):
                errors.append("runtime_list_incomplete")
                break
            if not sandboxes:
                runtime_empty = True
                break
            for sandbox in sandboxes:
                name = sandbox.get("name") or sandbox.get("metadata", {}).get("name")
                if not isinstance(name, str) or not re.fullmatch(r"rk-[a-f0-9]{16}", name):
                    errors.append("runtime_record_outside_fixture_shape")
                    continue
                if name not in requested:
                    deleted = subprocess.run([*cli, "sandbox", "delete", name], stdout=log, stderr=log, timeout=20)
                    if deleted.returncode != 0:
                        errors.append("runtime_delete_failed")
                    requested.add(name)
            if errors:
                break
            time.sleep(0.25)
        except (OSError, subprocess.TimeoutExpired, ValueError, TypeError, AttributeError):
            errors.append("runtime_cleanup_unconfirmed")
            break
    if not runtime_empty and not errors:
        errors.append("runtime_delete_not_confirmed")
    try:
        subprocess.run([*base, "logs", "--no-color"], stdout=log, stderr=log, timeout=20)
    except (OSError, subprocess.TimeoutExpired):
        errors.append("runtime_log_capture_failed")
    try:
        down = subprocess.run([*base, "down", "--volumes", "--remove-orphans"], stdout=log, stderr=log, timeout=60)
        if down.returncode != 0:
            errors.append("fixture_infrastructure_down_failed")
        remaining = subprocess.run([*base, "ps", "--all", "--quiet"], capture_output=True, text=True, timeout=15)
        if remaining.returncode != 0 or remaining.stdout.strip():
            errors.append("fixture_infrastructure_removal_unconfirmed")
    except (OSError, subprocess.TimeoutExpired):
        errors.append("fixture_infrastructure_removal_unconfirmed")
    confirmed = not errors and runtime_empty
    if confirmed and not preserve_recovery:
        # Generated signing keys are deleted only after owned infrastructure
        # and workload disappearance were confirmed. Never discard recovery
        # state for a fixture which might still be running.
        shutil.rmtree(directory)
    return {"confirmed": confirmed, "reason_codes": sorted(set(errors))}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--cli", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--docker-socket", default="/var/run/docker.sock")
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if os.environ.get("RUNKITE_TEST_ISOLATED") != "1":
        parser.error("explicit RUNKITE_TEST_ISOLATED=1 required")
    root = Path(__file__).resolve().parents[1]
    output = Path(args.output_dir).resolve()
    output.mkdir(parents=True, exist_ok=True)
    directory = Path(tempfile.mkdtemp(prefix="runtime-", dir=output))
    directory.chmod(0o700)
    data = directory / "data"
    data.mkdir()
    identity = "rk-isolated-" + uuid.uuid4().hex[:16]
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    endpoint = f"http://127.0.0.1:{port}"
    config = directory / "gateway.toml"
    config.write_text(f'''[openshell]
version = 2
[openshell.gateway]
bind_address = "0.0.0.0:8080"
health_bind_address = "0.0.0.0:8081"
compute_driver = "docker"
disable_tls = true
log_level = "info"
[openshell.gateway.auth]
allow_unauthenticated_users = true
[openshell.gateway.gateway_jwt]
signing_key_path = "{data}/tls/jwt/signing.pem"
public_key_path = "{data}/tls/jwt/public.pem"
kid_path = "{data}/tls/jwt/kid"
gateway_id = "{identity}"
[openshell.drivers.docker]
supervisor_network_mode = "bridge"
grpc_endpoint = "http://host.docker.internal:{port}"
sandbox_runtime_image = "{SANDBOX}"
supervisor_image = "{SUPERVISOR}"
image_pull_policy = "if_not_present"
sandbox_label = "{identity}"
app_armor_profile = "Unconfined"
''')
    compose = directory / "compose.json"
    fixture_user = f"{os.getuid()}:{os.getgid()}"
    compose.write_text(
        json.dumps(
            {
                "services": {
                    "init": {
                        "image": GATEWAY,
                        "user": fixture_user,
                        "command": ["generate-certs", "--output-dir", f"{data}/tls"],
                        "volumes": [f"{data}:{data}"],
                        "environment": {
                            "XDG_DATA_HOME": str(data),
                            "XDG_CONFIG_HOME": str(data / "config"),
                            "XDG_STATE_HOME": str(data / "state"),
                        },
                    },
                    "gateway": {
                        "image": GATEWAY,
                        "user": fixture_user,
                        "group_add": ["0", str(os.stat(args.docker_socket).st_gid)],
                        "command": [],
                        "ports": [f"127.0.0.1:{port}:8080"],
                        "volumes": [
                            f"{args.docker_socket}:/var/run/docker.sock",
                            f"{data}:{data}",
                            f"{config}:/etc/openshell/gateway.toml:ro",
                        ],
                        "environment": {
                            "OPENSHELL_GATEWAY_CONFIG": "/etc/openshell/gateway.toml",
                            "OPENSHELL_DB_URL": f"sqlite:{data}/gateway.db?mode=rwc",
                            "XDG_DATA_HOME": str(data),
                            "XDG_CONFIG_HOME": str(data / "config"),
                            "XDG_STATE_HOME": str(data / "state"),
                        },
                    },
                }
            }
        )
    )
    base = ["docker", "compose", "--env-file", "/dev/null", "--project-name", identity, "-f", str(compose)]
    cli = [str(Path(args.cli).resolve()), "--gateway-endpoint", endpoint]
    cli_environment = {
        **os.environ,
        "XDG_CONFIG_HOME": str(directory / "cli-config"),
        "XDG_DATA_HOME": str(directory / "cli-data"),
        "XDG_STATE_HOME": str(directory / "cli-state"),
    }
    status = 1
    guard = None
    interrupted = None

    def on_signal(number, _frame):
        nonlocal interrupted
        interrupted = number
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)
    log = (output / "runtime.log").open("wb")
    try:
        subprocess.run([*base, "run", "--rm", "init"], check=True, stdout=log, stderr=log, timeout=120)
        subprocess.run([*base, "up", "-d", "gateway"], check=True, stdout=log, stderr=log, timeout=120)
        deadline = time.monotonic() + 90
        while True:
            result = subprocess.run(
                [*cli, "sandbox", "list", "--output", "json"], env=cli_environment, capture_output=True, timeout=10
            )
            if result.returncode == 0:
                break
            if time.monotonic() > deadline:
                raise RuntimeError("disposable OpenShell gateway did not become ready")
            time.sleep(0.5)
        image = (
            subprocess.check_output(
                ["docker", "build", "-q", str(root / "examples/isolated_agent")],
                cwd=root,
                env={**os.environ, "BUILDX_CONFIG": str(directory / "buildx")},
                timeout=240,
                text=True,
            )
            .strip()
            .splitlines()[-1]
        )
        # Prove the negative filesystem fixture actually exists and is readable
        # without the external runtime, so a later denied read is meaningful.
        subprocess.run(
            [
                "docker",
                "run",
                "--rm",
                "--entrypoint",
                "python3",
                image,
                "-c",
                "from pathlib import Path;assert Path('/var/lib/forbidden-fixture/value').read_bytes()",
            ],
            check=True,
            timeout=30,
        )
        environment = {
            **cli_environment,
            "RUNKITE_TEST_ISOLATED_CLI": cli[0],
            "RUNKITE_TEST_ISOLATED_GATEWAY": endpoint,
            "RUNKITE_TEST_ISOLATED_IMAGE": image,
            "RUNKITE_TEST_ISOLATED_OUTPUT": str(output / "managed-evidence.json"),
        }
        (output / "runtime-fixture.json").write_text(
            json.dumps({"gateway": endpoint, "image": image, "release": "0.1.8", "project": identity}, indent=2) + "\n"
        )
        command = args.command[1:] if args.command[:1] == ["--"] else args.command
        if not command:
            parser.error("provide the qualification command after --")
        guard = subprocess.Popen(
            [
                sys.executable,
                str(root / "scripts/review-ha-supervisor.py"),
                "--log",
                str(output / "managed-test.log"),
                "--",
                *command,
            ],
            cwd=root,
            env=environment,
        )
        status = guard.wait()
    except KeyboardInterrupt:
        status = 128 + (interrupted or signal.SIGINT)
    finally:
        supervisor_stopped = True
        try:
            if guard is not None and guard.poll() is None:
                guard.terminate()
                try:
                    guard.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    guard.kill()
                    guard.wait(timeout=2)
        except (OSError, subprocess.TimeoutExpired):
            supervisor_stopped = False
        # A zero child exit cannot hide failed fixture cleanup. Keep private
        # recovery metadata if either workload or infrastructure state is unknown.
        try:
            cleanup = cleanup_fixture(base, cli, log, directory, preserve_recovery=not supervisor_stopped)
            if not supervisor_stopped:
                cleanup["confirmed"] = False
                cleanup["reason_codes"].append("fixture_supervisor_stop_unconfirmed")
        finally:
            log.close()
        (output / "runtime-cleanup.json").write_text(json.dumps(cleanup, indent=2) + "\n")
        if not cleanup["confirmed"]:
            if status == 0:
                status = 1
            print(f"Owned fixture cleanup is unconfirmed. Private recovery directory: {directory}", file=sys.stderr)
    return status


if __name__ == "__main__":
    raise SystemExit(main())
