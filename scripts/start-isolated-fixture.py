#!/usr/bin/env python3
"""Start one owned disposable external runtime, run a command, then clean it.

The trusted gateway alone receives the Docker socket. It uses verified release
digests and task-local data. This never installs services or registers a CLI.
"""

import argparse
import json
import os
import platform
import re
import shutil
import signal
import socket
import stat
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


def local_docker_command(socket_name):
    """Pin all owned infrastructure to the same local Unix daemon socket."""
    selected = os.environ.get("DOCKER_HOST", "")
    if selected and not selected.startswith("unix://"):
        raise ValueError("remote or conflicting Docker hosts are unsupported by the local runtime fixture")
    if os.environ.get("DOCKER_CONTEXT", "") not in {"", "default"}:
        raise ValueError("set the explicit local Docker socket instead of a named Docker context for this fixture")
    socket_path = Path(socket_name)
    if not socket_path.is_absolute():
        raise ValueError("fixture Docker socket must be an absolute local Unix path")
    try:
        socket_path = socket_path.resolve(strict=True)
        if not stat.S_ISSOCK(socket_path.stat().st_mode):
            raise ValueError("fixture Docker socket must be a local Unix socket")
    except OSError as error:
        raise ValueError("fixture Docker socket is unavailable") from error
    if selected and (not Path(selected[7:]).is_absolute() or Path(selected[7:]).resolve() != socket_path):
        raise ValueError("remote or conflicting Docker hosts are unsupported by the local runtime fixture")
    return ["docker", "--host", "unix://" + str(socket_path)], str(socket_path)


def fixture_network(host, port, tls):
    """Keep management loopback-only in each supported local engine topology."""
    scheme = "https" if tls else "http"
    if host == "Linux":
        # Linux bridge containers cannot reach a host-published port bound only
        # to 127.0.0.1. Trusted management companions share native host networking;
        # the actual workload still uses the upstream network=none boundary.
        return {
            "profile": "linux-host-loopback",
            "bind": f"127.0.0.1:{port}",
            "health": "",
            "supervisor_mode": "host",
            "callback": f"{scheme}://127.0.0.1:{port}",
            "command": ["--bind-address", "127.0.0.1", "--port", str(port)],
            "compose": {"network_mode": "host"},
        }
    if host == "Darwin":
        return {
            "profile": "darwin-bridge-loopback",
            "bind": "0.0.0.0:8080",
            "health": 'health_bind_address = "0.0.0.0:8081"',
            "supervisor_mode": "bridge",
            "callback": f"{scheme}://host.docker.internal:{port}",
            "command": ["--bind-address", "0.0.0.0", "--port", "8080"],
            "compose": {
                "ports": [f"127.0.0.1:{port}:8080"],
                "extra_hosts": ["host.docker.internal:host-gateway"],
            },
        }
    raise ValueError("the local runtime fixture supports native Linux or macOS engines only")


def cleanup_fixture(base, cli, log, directory, *, timeout=30, preserve_recovery=False, environment=None):
    """Confirm owned runtime cleanup before discarding private recovery data."""
    errors = []
    runtime_empty = False
    deadline = time.monotonic() + timeout
    requested = set()
    while time.monotonic() < deadline:
        try:
            result = subprocess.run(
                [*cli, "sandbox", "list", "--output", "json"],
                capture_output=True,
                text=True,
                timeout=15,
                env=environment,
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
                    deleted = subprocess.run(
                        [*cli, "sandbox", "delete", name], stdout=log, stderr=log, timeout=20, env=environment
                    )
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


def make_gateway_pki(directory):
    """Issue disposable certificates without touching user trust or CLI state."""
    directory.mkdir(mode=0o700)

    def openssl(*args):
        result = subprocess.run(["openssl", *args], cwd=directory, capture_output=True, timeout=30)
        if result.returncode != 0:
            raise RuntimeError("owned gateway fixture certificate generation failed")

    for ca in ("ca", "untrusted-ca"):
        (directory / (ca + ".conf")).write_text(
            "[req]\ndistinguished_name=dn\nx509_extensions=ca\nprompt=no\n[dn]\nCN=Runkite fixture "
            + ca
            + "\n[ca]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n"
        )
        openssl(
            "req",
            "-x509",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            ca + ".key",
            "-out",
            ca + ".pem",
            "-days",
            "1",
            "-config",
            ca + ".conf",
        )
    for name, ca, extension in (
        (
            "server",
            "ca",
            "extendedKeyUsage=serverAuth\nsubjectAltName=IP:127.0.0.1,DNS:localhost,DNS:host.docker.internal,DNS:host.openshell.internal\n",
        ),
        ("client", "ca", "extendedKeyUsage=clientAuth\n"),
        ("untrusted-client", "untrusted-ca", "extendedKeyUsage=clientAuth\n"),
    ):
        (directory / (name + ".conf")).write_text(
            "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n" + extension
        )
        (directory / (name + "-request.conf")).write_text(
            "[req]\ndistinguished_name=dn\nprompt=no\n[dn]\nCN=Runkite gateway fixture " + name + "\n"
        )
        openssl(
            "req",
            "-new",
            "-newkey",
            "rsa:2048",
            "-nodes",
            "-keyout",
            name + ".key",
            "-out",
            name + ".csr",
            "-config",
            name + "-request.conf",
        )
        openssl(
            "x509",
            "-req",
            "-in",
            name + ".csr",
            "-CA",
            ca + ".pem",
            "-CAkey",
            ca + ".key",
            "-CAcreateserial",
            "-out",
            name + ".pem",
            "-days",
            "1",
            "-extfile",
            name + ".conf",
        )
        openssl("verify", "-CAfile", ca + ".pem", name + ".pem")
    for key in directory.glob("*.key"):
        key.chmod(0o600)


def gateway_tls_environment(environment, config, endpoint, pki, *, ca="ca", client="client"):
    # The pinned CLI resolves a direct endpoint's TLS bundle under the sanitized
    # gateway name. This is task-local XDG state, never the user's CLI registry.
    name = re.sub(r"[^a-zA-Z0-9._-]", "_", endpoint)
    bundle = config / "openshell" / "gateways" / name / "mtls"
    bundle.mkdir(parents=True, mode=0o700)
    for source, target in ((ca + ".pem", "ca.crt"), (client + ".pem", "tls.crt"), (client + ".key", "tls.key")):
        shutil.copyfile(pki / source, bundle / target)
        (bundle / target).chmod(0o600)
    return {**environment, "XDG_CONFIG_HOME": str(config)}


def wait_for_gateway(cli, environment, *, timeout=90):
    deadline = time.monotonic() + timeout
    while True:
        result = subprocess.run(
            [*cli, "sandbox", "list", "--output", "json"], env=environment, capture_output=True, timeout=10
        )
        if result.returncode == 0:
            return
        if time.monotonic() > deadline:
            raise RuntimeError("disposable OpenShell gateway did not become ready")
        time.sleep(0.5)


def qualify_gateway_tls(base, cli, environment, directory, pki, log, report, *, outage=False):
    if not outage:
        wait_for_gateway(cli, environment)
        report["cases"].append("authorized pinned CLI verifies gateway and authenticates with owned client certificate")
        for label, ca, client in (
            ("untrusted_server_ca", "untrusted-ca", "client"),
            ("untrusted_client_certificate", "ca", "untrusted-client"),
        ):
            rejected = gateway_tls_environment(environment, directory / label, cli[2], pki, ca=ca, client=client)
            result = subprocess.run(
                [*cli, "sandbox", "list", "--output", "json"], env=rejected, capture_output=True, timeout=15
            )
            if result.returncode == 0:
                raise RuntimeError("gateway TLS fixture accepted " + label)
            report["cases"].append(label + " rejected without disabling verification")
            wait_for_gateway(cli, environment, timeout=15)
        return
    subprocess.run([*base, "stop", "gateway"], check=True, stdout=log, stderr=log, timeout=45)
    try:
        try:
            result = subprocess.run(
                [*cli, "sandbox", "list", "--output", "json"], env=environment, capture_output=True, timeout=15
            )
            if result.returncode == 0:
                raise RuntimeError("stopped gateway unexpectedly accepted a TLS request")
        except subprocess.TimeoutExpired:
            pass
        report["cases"].append("gateway outage rejects management requests instead of downgrading to plaintext")
    finally:
        subprocess.run([*base, "up", "-d", "gateway"], check=True, stdout=log, stderr=log, timeout=90)
    wait_for_gateway(cli, environment)
    report["cases"].append("authorized TLS management recovers after bounded gateway restart")
    report["passed"] = True


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--cli", required=True)
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--docker-socket", default="/var/run/docker.sock")
    parser.add_argument(
        "--tls", action="store_true", help="qualify an authenticated TLS gateway using disposable local certificates"
    )
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if os.environ.get("RUNKITE_TEST_ISOLATED") != "1":
        parser.error("explicit RUNKITE_TEST_ISOLATED=1 required")
    try:
        docker, args.docker_socket = local_docker_command(args.docker_socket)
        host = platform.system()
        fixture_network(host, 1, args.tls)
    except ValueError as error:
        parser.error(str(error))
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
    network = fixture_network(host, port, args.tls)
    endpoint = f"{'https' if args.tls else 'http'}://127.0.0.1:{port}"
    pki = data / "gateway-pki"
    if args.tls:
        make_gateway_pki(pki)
    gateway_security = (
        f'''disable_tls = false
enable_loopback_service_http = false
guest_tls_ca = "{pki}/ca.pem"
[openshell.gateway.tls]
cert_path = "{pki}/server.pem"
key_path = "{pki}/server.key"
client_ca_path = "{pki}/ca.pem"
[openshell.gateway.mtls_auth]
enabled = true
[openshell.gateway.auth]
allow_unauthenticated_users = false
'''
        if args.tls
        else """disable_tls = true
[openshell.gateway.auth]
allow_unauthenticated_users = true
"""
    )
    config = directory / "gateway.toml"
    config.write_text(f'''[openshell]
version = 2
[openshell.gateway]
bind_address = "{network["bind"]}"
{network["health"]}
compute_driver = "docker"
log_level = "info"
{gateway_security}
[openshell.gateway.gateway_jwt]
signing_key_path = "{data}/tls/jwt/signing.pem"
public_key_path = "{data}/tls/jwt/public.pem"
kid_path = "{data}/tls/jwt/kid"
gateway_id = "{identity}"
[openshell.drivers.docker]
supervisor_network_mode = "{network["supervisor_mode"]}"
grpc_endpoint = "{network["callback"]}"
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
                        "group_add": list(dict.fromkeys(["0", str(os.stat(args.docker_socket).st_gid)])),
                        "command": network["command"],
                        **network["compose"],
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
    base = [*docker, "compose", "--env-file", "/dev/null", "--project-name", identity, "-f", str(compose)]
    cli = [str(Path(args.cli).resolve()), "--gateway-endpoint", endpoint]
    cli_environment = {
        **os.environ,
        "XDG_CONFIG_HOME": str(directory / "cli-config"),
        "XDG_DATA_HOME": str(directory / "cli-data"),
        "XDG_STATE_HOME": str(directory / "cli-state"),
    }
    if args.tls:
        cli_environment = gateway_tls_environment(cli_environment, directory / "cli-config", endpoint, pki)
    tls_report = {
        "passed": False,
        "scope": "disposable gateway authenticated TLS, no external PKI or rotation",
        "cases": [],
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
        wait_for_gateway(cli, cli_environment)
        if args.tls:
            qualify_gateway_tls(base, cli, cli_environment, directory, pki, log, tls_report)
        image = (
            subprocess.check_output(
                [*docker, "build", "-q", str(root / "examples/isolated_agent")],
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
                *docker,
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
            "RUNKITE_TEST_ISOLATED_GATEWAY_TLS": "1" if args.tls else "0",
        }
        (output / "runtime-fixture.json").write_text(
            json.dumps(
                {
                    "gateway": endpoint,
                    "gateway_tls": args.tls,
                    "client_auth": "mtls" if args.tls else "none",
                    "image": image,
                    "release": "0.1.8",
                    "project": identity,
                    "host_platform": host,
                    "network_profile": network["profile"],
                    "supervisor_network_mode": network["supervisor_mode"],
                },
                indent=2,
            )
            + "\n"
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
        if args.tls and status == 0:
            qualify_gateway_tls(base, cli, cli_environment, directory, pki, log, tls_report, outage=True)
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
            cleanup = cleanup_fixture(
                base, cli, log, directory, preserve_recovery=not supervisor_stopped, environment=cli_environment
            )
            if not supervisor_stopped:
                cleanup["confirmed"] = False
                cleanup["reason_codes"].append("fixture_supervisor_stop_unconfirmed")
        finally:
            log.close()
        (output / "runtime-cleanup.json").write_text(json.dumps(cleanup, indent=2) + "\n")
        if args.tls:
            tls_report["passed"] = tls_report["passed"] and cleanup["confirmed"]
            (output / "gateway-tls-evidence.json").write_text(json.dumps(tls_report, indent=2) + "\n")
        if not cleanup["confirmed"]:
            if status == 0:
                status = 1
            print(f"Owned fixture cleanup is unconfirmed. Private recovery directory: {directory}", file=sys.stderr)
    return status


if __name__ == "__main__":
    raise SystemExit(main())
