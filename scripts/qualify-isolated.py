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

from isolated_policy_qualification import PolicyQualification
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


class FixtureUsage:
    def __init__(self):
        self.value = None

    async def get(self, _run):
        return self.value

    async def report(self, _run, value):
        self.value = {**value, "source": "isolated_workload", "reported_generation": 1}
        return self.value


class ObservedRuntime(OpenShellRuntime):
    def __init__(self, *args, prover=None, **kwargs):
        super().__init__(*args, **kwargs)
        self.started = []
        if prover and self.providers:
            raise ValueError("effective-policy qualification requires a provider-free owned fixture")
        self.policy_qualification = PolicyQualification(self._policy_bytes, self.descriptor, prover) if prover else None
        self._policy_pending = set()

    async def _control(self, *args):
        result = await super()._control(*args)
        if self.policy_qualification is not None and args[:2] == ("sandbox", "create"):
            name = args[args.index("--name") + 1]
            # Base launch has created only its sleeping owned sandbox here.
            # Inspect before it can execute the workload, not after returning a
            # live process whose input has merely been withheld.
            await self.policy_qualification.before_exec(name, super()._control)
            self._policy_pending.add(name)
        return result

    async def launch(self, name):
        self.started.append(name)
        return await super().launch(name)

    async def cleanup(self, name):
        failure = None
        if name in self._policy_pending:
            self._policy_pending.remove(name)
            try:
                await self.policy_qualification.after_exec(name, super()._control)
            except BaseException as error:
                failure = error
        try:
            return await super().cleanup(name)
        finally:
            # Failed inspection must not prevent deletion or become a success
            # when the adapter catches a cleanup exception for its event stream.
            if failure is not None:
                raise failure


async def qualify(args):
    endpoint = urlparse(args.gateway)
    if os.environ.get("RUNKITE_TEST_ISOLATED") != "1" or endpoint.hostname not in {"127.0.0.1", "::1"}:
        raise ValueError("qualification requires explicit opt-in and a disposable literal-loopback gateway")
    runtime = ObservedRuntime(
        args.cli,
        args.image,
        args.policy,
        ["python3", "/app/workload.py"],
        gateway_endpoint=args.gateway,
        prover=args.prover,
    )
    await runtime.verify()
    client = FixtureCheckpoints()
    usage = FixtureUsage()
    adapter = IsolatedAdapter(
        "http://fixture.invalid", "pool-fixture", None, runtime=runtime, client=client, usage_client=usage
    )
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
        usage.value = None  # A recreated SQL run has a fresh private snapshot.
        await adapter.execute(assignment, event, asyncio.Event())
        if (
            len(runtime.started) != 2
            or decode_frame(client.raw)["receipts"]["fixture-replacement"]["output"]["turn"] != 2
        ):
            raise AssertionError("replacement identity or conversation continuity failed")
        policy = (
            runtime.policy_qualification.summary()
            if runtime.policy_qualification is not None
            else {"passed": False, "status": "not_requested"}
        )
        if runtime.policy_qualification is not None and len(policy["attempts"]) != 2:
            raise AssertionError("policy qualification did not inspect both created incarnations")
        return {
            "passed": True,
            "runtime": "OpenShell 0.1.8",
            "image": args.image,
            "effective_policy": policy,
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
                "atomic snapshot-to-exec policy binding",
                "continuous policy enforcement between observations",
                "provider credential state or provider-composed policy",
                *([] if args.prover else ["effective-policy boundary comparison (no prover supplied)"]),
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
    parser.add_argument(
        "--prover", help="optional pinned OpenShell 0.1.8 boundary checker for effective-policy evidence"
    )
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
