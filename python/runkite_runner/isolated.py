"""Experimental trusted-worker bridge to an external OpenShell workload.

This protocol is not a sandbox by itself. The selected external runtime must
enforce its boundary. No framework module is imported into the trusted worker.
"""

from __future__ import annotations

import argparse
import asyncio
import contextlib
import hashlib
import json
import math
import os
import re
import tempfile
import uuid
from decimal import Decimal
from pathlib import Path
from urllib.parse import urlparse

from .completion import CompletionDeliveryError
from .connectors import proxy_connector_mcp
from .execution_pool import pool_runner_kind
from .generic_worker import make_event_factory, run_worker
from .logging_config import setup_logging
from .opaque_checkpoint import OpaqueCheckpointClient

MAX_FRAME = 1 << 20
MAX_RECEIPTS = 128
MAX_RUN_BYTES = 64 << 20
_IMAGE = re.compile(r"(?:[^\s]+@sha256:|sha256:)[a-f0-9]{64}\Z")


def _pairs(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate bridge field")
        result[key] = value
    return result


def _exact_float(raw):
    value = float(raw)
    if not math.isfinite(value) or Decimal(str(value)) != Decimal(raw):
        raise ValueError("bridge number cannot round-trip without precision loss")
    return value


def decode_frame(raw: bytes) -> dict:
    if len(raw) > MAX_FRAME:
        raise ValueError("bridge frame exceeds limit")
    result = json.loads(
        raw,
        object_pairs_hook=_pairs,
        parse_float=_exact_float,
        parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")),
    )
    if not isinstance(result, dict):
        raise ValueError("bridge frame must be an object")
    return result


def encode_frame(value: dict) -> bytes:
    raw = json.dumps(value, allow_nan=False, ensure_ascii=False, separators=(",", ":")).encode()
    if len(raw) > MAX_FRAME:
        raise ValueError("bridge frame exceeds limit")
    return raw + b"\n"


def workload_input(assignment: dict, state) -> dict:
    # Never forward assignment config, identity claims, manifests, environment
    # or credential references. The workload image owns its SDK configuration.
    return {
        "type": "execute",
        "input": assignment.get("input"),
        "state": state,
        "run": {key: assignment.get(key) for key in ("run_id", "thread_id", "graph_id", "generation", "tenant_id")},
    }


class OpenShellRuntime:
    """Pinned CLI transport. Management credentials remain on the trusted side."""

    def __init__(
        self,
        cli: str,
        image: str,
        policy_file: str,
        command: list[str],
        providers: list[str] | None = None,
        *,
        gateway_endpoint: str,
        workspace: str = "default",
    ):
        if not _IMAGE.fullmatch(image):
            raise ValueError("isolated image must use an explicit SHA-256 digest")
        if not command or not all(isinstance(arg, str) and arg and "\x00" not in arg for arg in command):
            raise ValueError("isolated command must be a nonempty argument array")
        self.cli, self.image, self.command = cli, image, list(command)
        endpoint = urlparse(gateway_endpoint)
        if (
            endpoint.username
            or endpoint.password
            or endpoint.query
            or endpoint.fragment
            or endpoint.path not in {"", "/"}
            or not endpoint.hostname
        ):
            raise ValueError("isolated gateway endpoint must be an explicit nonsecret origin")
        if endpoint.scheme != "https" and not (endpoint.scheme == "http" and endpoint.hostname in {"127.0.0.1", "::1"}):
            raise ValueError("isolated gateway requires HTTPS or a loopback fixture")
        if os.environ.get("OPENSHELL_GATEWAY_INSECURE", "").lower() not in {"", "0", "false"}:
            raise ValueError("isolated gateway TLS verification must remain enabled")
        if not re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}", workspace):
            raise ValueError("invalid isolated workspace")
        self.gateway_endpoint, self.workspace = gateway_endpoint, workspace
        self.providers = providers or []
        if not isinstance(self.providers, list) or not all(
            isinstance(name, str) and re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}", name) for name in self.providers
        ):
            raise ValueError("isolated providers must be registered nonsecret names")
        self.policy_file = str(Path(policy_file).resolve(strict=True))
        policy = Path(self.policy_file).read_bytes()
        if len(policy) > MAX_FRAME:
            raise ValueError("isolated policy exceeds limit")
        self._policy_bytes = policy
        self.descriptor = hashlib.sha256(
            encode_frame(
                {
                    "image": image,
                    "command": command,
                    "providers": self.providers,
                    "gateway_endpoint": gateway_endpoint,
                    "workspace": workspace,
                    "policy_sha256": hashlib.sha256(policy).hexdigest(),
                }
            )
        ).hexdigest()

    async def _control(self, *args: str) -> bytes:
        proc = await asyncio.create_subprocess_exec(
            self.cli,
            "--gateway-endpoint",
            self.gateway_endpoint,
            "--workspace",
            self.workspace,
            *args,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
        )
        try:
            async with asyncio.timeout(45):
                data = bytearray()
                while chunk := await proc.stdout.read(min(65536, MAX_FRAME + 1 - len(data))):
                    data.extend(chunk)
                    if len(data) > MAX_FRAME:
                        raise RuntimeError("external runtime management response exceeds limit")
                if await proc.wait() != 0:
                    raise RuntimeError("external runtime management failed")
                return bytes(data)
        finally:
            if proc.returncode is None:
                proc.kill()
                await proc.wait()

    async def verify(self) -> None:
        version = (await self._control("--version")).decode().strip()
        if not re.fullmatch(r"openshell(?:-cli)? v?0\.1\.8(?:\s.*)?", version, re.IGNORECASE):
            raise ValueError("isolated runtime requires the qualified OpenShell 0.1.8 CLI")

    async def launch(self, name: str):
        providers = [value for name in self.providers for value in ("--provider", name)]
        # The descriptor binds the bytes captured at construction. Never let a
        # later config-file replacement alter the policy behind that identity.
        # The private directory also prevents another local user substituting
        # the handoff path while the CLI reads it.
        with tempfile.TemporaryDirectory(prefix="runkite-isolated-policy-") as directory:
            snapshot = Path(directory) / "policy.yaml"
            descriptor = os.open(snapshot, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(descriptor, "wb") as output:
                output.write(self._policy_bytes)
            await self._control(
                "sandbox",
                "create",
                "--name",
                name,
                "--from",
                self.image,
                "--policy",
                str(snapshot),
                "--label",
                f"runkite_attempt={name}",
                "--no-auto-providers",
                "--approval-mode",
                "manual",
                "--detach",
                *providers,
                "--",
                "/bin/sleep",
                "infinity",
            )
        return await asyncio.create_subprocess_exec(
            self.cli,
            "--gateway-endpoint",
            self.gateway_endpoint,
            "--workspace",
            self.workspace,
            "sandbox",
            "exec",
            "--name",
            name,
            "--no-tty",
            "--no-login-shell",
            "--",
            *self.command,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
            limit=MAX_FRAME + 1,
        )

    async def cleanup(self, name: str) -> bool:
        await self._control("sandbox", "delete", name)
        # A successful delete request alone is only acceptance. Empty complete
        # authenticated listings confirm that this owned record disappeared.
        async with asyncio.timeout(15):
            while True:
                listing = decode_frame(
                    await self._control("sandbox", "list", "--output", "json", "--selector", f"runkite_attempt={name}")
                )
                if listing.get("sandboxes") == [] and not listing.get("next_page_token"):
                    return True
                await asyncio.sleep(0.25)


class IsolatedAdapter:
    def __init__(self, http_address: str, runner_kind: str, runner_token: str | None, *, runtime=None, client=None):
        self.http_address, self.runner_kind, self.runner_token = http_address, runner_kind, runner_token
        self.runtime, self.client = runtime, client

    async def load_config(self, path: str) -> None:
        config = decode_frame(Path(path).read_bytes())
        self.graphs = set(config.get("graphs", {}))
        if self.runtime is None:
            opts = config.get("isolated_execution")
            if not isinstance(opts, dict) or opts.get("provider") != "openshell":
                raise ValueError("isolated_execution.provider must be openshell")
            if set(opts) - {
                "provider",
                "cli",
                "image",
                "policy_file",
                "command",
                "providers",
                "gateway_endpoint",
                "workspace",
            }:
                raise ValueError("unknown isolated execution option")
            self.runtime = OpenShellRuntime(
                opts.get("cli", "openshell"),
                opts["image"],
                str(Path(path).parent / opts["policy_file"]),
                opts["command"],
                opts.get("providers"),
                gateway_endpoint=opts["gateway_endpoint"],
                workspace=opts.get("workspace", "default"),
            )
        await self.runtime.verify()

    async def execute(self, assignment: dict, event_callback, cancel_event: asyncio.Event) -> str:
        async with asyncio.timeout(600):
            return await self._execute(assignment, event_callback, cancel_event)

    async def _execute(self, assignment: dict, event_callback, cancel_event: asyncio.Event) -> str:
        if assignment.get("graph_id") not in self.graphs:
            raise ValueError("isolated graph is not configured")
        if not self.http_address:
            raise ValueError("isolated execution requires durable proxy checkpoints")
        incarnation = assignment.get("run_incarnation_id")
        if not isinstance(incarnation, str) or not incarnation:
            raise CompletionDeliveryError("isolated execution requires a durable run incarnation")
        client = self.client or OpaqueCheckpointClient(
            http_base_url=self.http_address,
            framework="isolated-v1",
            runner_kind=self.runner_kind,
            runner_token=self.runner_token,
        )
        run_id, thread_id = assignment["run_id"], assignment["thread_id"]
        make_event = make_event_factory(run_id)
        proc = None
        name = "rk-" + uuid.uuid4().hex[:16]
        launched = False
        cleaned = False

        async def cleanup():
            nonlocal cleaned
            confirmed = False
            with contextlib.suppress(Exception):
                confirmed = await asyncio.shield(self.runtime.cleanup(name))
            cleaned = True
            await event_callback(
                make_event(
                    "custom",
                    {
                        "event": "execution_cleanup",
                        "source": "trusted_worker",
                        "provider": "openshell",
                        "sandbox": name,
                        "confirmed": confirmed,
                    },
                )
            )

        try:
            raw, etag = await client.get_versioned(thread_id)
            state = (
                decode_frame(raw)
                if raw
                else {"version": 1, "descriptor": self.runtime.descriptor, "state": None, "receipts": {}}
            )
            if state.get("version") != 1 or state.get("descriptor") != self.runtime.descriptor:
                raise CompletionDeliveryError("isolated state belongs to a different workload descriptor")
            receipt = state["receipts"].get(incarnation)
            if receipt:
                if (
                    receipt.get("run_id") != run_id
                    or receipt.get("status") not in {"success", "error", "interrupted"}
                    or not re.fullmatch(r"rk-[a-f0-9]{16}", receipt.get("sandbox", ""))
                ):
                    raise CompletionDeliveryError("invalid isolated receipt binding")
                name = receipt["sandbox"]
                launched = True
                await cleanup()
                await event_callback(make_event("values", receipt["output"]))
                await event_callback(make_event("error" if receipt["status"] == "error" else "end", {}))
                return receipt["status"]
            if state.get("intent"):
                raise CompletionDeliveryError(
                    "isolated launch outcome requires reconciliation; execution was not retried"
                )
            if len(state["receipts"]) >= MAX_RECEIPTS:
                raise CompletionDeliveryError("isolated receipt capacity reached; start a new thread")
            if cancel_event.is_set():
                return "interrupted"
            state["intent"] = {
                "run_id": run_id,
                "run_incarnation_id": incarnation,
                "generation": assignment.get("generation", 0),
                "sandbox": name,
            }
            intent = encode_frame(state)
            await client.put_cas(thread_id, intent, etag=etag, create_only=etag is None)
            persisted, etag = await client.get_versioned(thread_id)
            if persisted != intent:
                raise CompletionDeliveryError("isolated launch intent changed before execution")
            launched = True  # create errors can leave an uncertain sandbox.
            proc = await self.runtime.launch(name)
            proc.stdin.write(encode_frame(workload_input(assignment, state["state"])))
            await proc.stdin.drain()
            used_calls = set()
            received_bytes = 0
            received_frames = 0
            result = None
            while True:
                if cancel_event.is_set():
                    raise asyncio.CancelledError
                raw_frame = await proc.stdout.readline()
                received_bytes += len(raw_frame)
                received_frames += 1
                if received_bytes > MAX_RUN_BYTES or received_frames > 10000:
                    raise ValueError("isolated workload output limit exceeded")
                if not raw_frame:
                    raise RuntimeError("isolated workload closed before its result")
                frame = decode_frame(raw_frame)
                kind = frame.get("type")
                if (
                    kind == "event"
                    and set(frame) == {"type", "method", "data"}
                    and frame["method"] in {"messages/partial", "updates", "custom"}
                ):
                    data = (
                        {"source": "workload", "event": frame["data"]} if frame["method"] == "custom" else frame["data"]
                    )
                    await event_callback(make_event(frame["method"], data))
                elif kind == "tool_call" and set(frame) == {"type", "id", "connector", "tool", "arguments"}:
                    call_id = frame["id"]
                    if not isinstance(call_id, str) or not call_id or len(call_id) > 128 or call_id in used_calls:
                        raise ValueError("invalid or reused bridge call ID")
                    if (
                        not isinstance(frame["connector"], str)
                        or not isinstance(frame["tool"], str)
                        or not isinstance(frame["arguments"], dict)
                    ):
                        raise ValueError("invalid bridge tool request")
                    used_calls.add(call_id)
                    allowed = assignment.get("allowed_tools")
                    if len(used_calls) > 128 or (allowed is not None and frame["tool"] not in allowed):
                        raise ValueError("isolated bridge tool is not allowed")
                    response = await proxy_connector_mcp(
                        {"configurable": {"run_id": run_id, "generation": assignment.get("generation", 0)}},
                        frame["connector"],
                        {
                            "jsonrpc": "2.0",
                            "id": call_id,
                            "method": "tools/call",
                            "params": {"name": frame["tool"], "arguments": frame["arguments"]},
                        },
                        control_plane_url=self.http_address,
                    )
                    proc.stdin.write(encode_frame({"type": "tool_result", "id": call_id, "response": response}))
                    await proc.stdin.drain()
                elif (
                    kind == "result"
                    and set(frame) == {"type", "status", "state", "output"}
                    and frame["status"] in {"success", "error", "interrupted"}
                    and isinstance(frame["output"], dict)
                ):
                    result = frame
                    break
                else:
                    raise ValueError("invalid isolated workload frame")
            proc.stdin.close()
            async with asyncio.timeout(10):
                if await proc.wait() != 0:
                    raise RuntimeError("isolated workload delivery failed")
            state["state"] = result["state"]
            state.pop("intent")
            state["receipts"][incarnation] = {
                "run_id": run_id,
                "status": result["status"],
                "output": result["output"],
                "sandbox": name,
            }
            await client.put_cas(thread_id, encode_frame(state), etag=etag)
            await cleanup()
            await event_callback(make_event("values", result["output"]))
            await event_callback(make_event("error" if result["status"] == "error" else "end", {}))
            return result["status"]
        except asyncio.CancelledError:
            raise
        except CompletionDeliveryError:
            raise
        except Exception as exc:
            # A failure after a committed intent never becomes a second model
            # or tool execution on reclaim, including ambiguous CAS responses.
            raise CompletionDeliveryError(
                "isolated execution or state delivery failed; inspect retained intent"
            ) from exc
        finally:
            if proc is not None and proc.returncode is None:
                proc.kill()
                await proc.wait()
            if launched and not cleaned:
                with contextlib.suppress(Exception):
                    await cleanup()
            if self.client is None:
                await client.aclose()


def main() -> None:
    setup_logging()
    parser = argparse.ArgumentParser(description="Experimental Runkite external OpenShell worker")
    parser.add_argument("--config", required=True)
    parser.add_argument("--grpc-address", default=os.environ.get("RUNKITE_GRPC_URL", "localhost:50051"))
    parser.add_argument("--http-address", default=os.environ.get("RUNKITE_HTTP_URL", "http://localhost:2026"))
    parser.add_argument("--runner-kind", default="python-isolated")
    parser.add_argument("--execution-pool", default=os.environ.get("RUNKITE_EXECUTION_POOL"))
    args = parser.parse_args()
    kind = pool_runner_kind(args.runner_kind, args.execution_pool)
    adapter = IsolatedAdapter(args.http_address, kind, os.environ.get("RUNNER_TOKEN"))
    asyncio.run(run_worker(adapter, args.config, args.grpc_address, kind, 1, http_address=args.http_address))


if __name__ == "__main__":
    main()
