#!/usr/bin/env python3
"""Exercise the bridge against an explicitly owned loopback OpenShell fixture.

This does not provision a gateway, modify global CLI registrations, or qualify
Runkite HA/SDK integration. It writes the exact scope into its JSON evidence.
"""

import argparse
import asyncio
import json
import os
from pathlib import Path
from urllib.parse import urlparse

from runkite_runner.isolated import IsolatedAdapter, OpenShellRuntime, decode_frame


class FixtureCheckpoints:
    def __init__(self):
        self.raw = None
        self.version = 0

    async def get_versioned(self, _thread):
        return self.raw, f'"{self.version}"' if self.version else None

    async def put_cas(self, _thread, value, *, etag, create_only=False):
        expected = f'"{self.version}"' if self.version else None
        if etag != expected or create_only != (self.version == 0):
            raise RuntimeError("fixture CAS rejected stale write")
        self.raw = value
        self.version += 1


class ObservedRuntime(OpenShellRuntime):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.started = []

    async def launch(self, name):
        self.started.append(name)
        return await super().launch(name)


async def qualify(args):
    endpoint = urlparse(args.gateway)
    if os.environ.get("RUNKITE_TEST_ISOLATED") != "1" or endpoint.hostname not in {"127.0.0.1", "::1"}:
        raise ValueError("qualification requires explicit opt-in and a disposable literal-loopback gateway")
    runtime = ObservedRuntime(
        args.cli, args.image, args.policy, ["python3", "/app/workload.py"], gateway_endpoint=args.gateway
    )
    await runtime.verify()
    client = FixtureCheckpoints()
    adapter = IsolatedAdapter("http://fixture.invalid", "pool-fixture", None, runtime=runtime, client=client)
    adapter.graphs = {"isolated_echo"}
    events = []

    async def event(value):
        events.append(value)

    assignment = {
        "run_id": "synthetic-run",
        "run_incarnation_id": "fixture-first",
        "thread_id": "synthetic-thread",
        "graph_id": "isolated_echo",
        "generation": 1,
        "tenant_id": "default",
        "input": {"text": "offline fixture", "probe_urls": args.blocked_url},
    }
    try:
        result = await adapter.execute(assignment, event, asyncio.Event())
        output = decode_frame(client.raw)["receipts"]["fixture-first"]["output"]
        if (
            result != "success"
            or output["uid"] == 0
            or output["bootstrap_environment_present"]
            or output["forbidden_fixture_readable"]
            or output.get("host_socket_status") != "absent"
            or len(output["network_probes"]) != len(args.blocked_url)
            or any(output["network_probes"])
        ):
            raise AssertionError("external confinement probe failed")
        if not any(item["method"] == "messages/partial" for item in events):
            raise AssertionError("streaming was not observed")
        if not any(
            item["data"].get("event") == "execution_cleanup" and item["data"].get("confirmed")
            for item in events
            if isinstance(item["data"], dict)
        ):
            raise AssertionError("owned runtime cleanup was not confirmed")
        assignment["generation"] = 2
        await adapter.execute(assignment, event, asyncio.Event())
        if len(runtime.started) != 1:
            raise AssertionError("receipt replay started a second workload")
        assignment["run_incarnation_id"] = "fixture-replacement"
        await adapter.execute(assignment, event, asyncio.Event())
        if (
            len(runtime.started) != 2
            or decode_frame(client.raw)["receipts"]["fixture-replacement"]["output"]["turn"] != 2
        ):
            raise AssertionError("replacement identity or conversation continuity failed")
        return {
            "passed": True,
            "runtime": "OpenShell 0.1.8",
            "image": args.image,
            "scope": [
                "real external workload",
                "non-root",
                "baked forbidden file denied",
                "bootstrap environment absent",
                "streamed event",
                "owned deletion confirmed",
                "incarnation receipt replay",
                "opaque state continuity",
                *(["configured blocked URL probes"] if args.blocked_url else []),
            ],
            "checkpoint_backend": "in-memory CAS fixture, not a Runkite server",
            "not_qualified": [
                "control-plane HA",
                "CNI egress",
                "live model/provider",
                "upstream attestation",
                "arbitrary existing SDK compatibility",
                *([] if args.blocked_url else ["runtime network reachability (no blocked URL probes supplied)"]),
            ],
            "sandbox_names": runtime.started,
        }
    finally:
        for name in runtime.started:
            # Never target another test or a sandbox discovered by a broad scan.
            try:
                await runtime.cleanup(name)
            except Exception:
                pass


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--cli", required=True)
    parser.add_argument("--gateway", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--policy", default="examples/isolated_agent/policy.yaml")
    parser.add_argument("--blocked-url", action="append", default=[])
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    evidence = None
    try:
        evidence = asyncio.run(qualify(args))
    except Exception as error:
        evidence = {"passed": False, "error_type": type(error).__name__, "error": str(error)}
        raise
    finally:
        Path(args.output).write_text(json.dumps(evidence, indent=2) + "\n")


if __name__ == "__main__":
    main()
