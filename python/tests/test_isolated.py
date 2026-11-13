"""Bridge invariants with fake runtime processes, not sandbox qualification."""

import asyncio
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import AsyncMock, patch

from runkite_runner.completion import CompletionDeliveryError
from runkite_runner.isolated import IsolatedAdapter, OpenShellRuntime, decode_frame, encode_frame, workload_input


class Checkpoints:
    def __init__(self):
        self.raw = None
        self.version = 0
        self.fail_committed_result = False

    async def get_versioned(self, _thread):
        return self.raw, f'"{self.version}"' if self.version else None

    async def put_cas(self, _thread, value, *, etag, create_only=False):
        expected = f'"{self.version}"' if self.version else None
        if etag != expected or create_only != (self.version == 0):
            raise RuntimeError("CAS conflict")
        self.raw = value
        self.version += 1
        if self.fail_committed_result and "intent" not in decode_frame(value):
            self.fail_committed_result = False
            raise RuntimeError("lost commit acknowledgement")


class Runtime:
    descriptor = "fixture-descriptor"

    def __init__(self, script=None):
        self.starts = []
        self.ready = asyncio.Event()
        self.cleanups = []
        self.script = (
            script
            or """
import json,os,sys
request=json.loads(sys.stdin.readline())
state=(request['state'] or 0)+1
print(json.dumps({'type':'result','status':'success','state':state,'output':{'count':state,'keys':sorted(request),'token_present':'RUNNER_TOKEN' in os.environ}}),flush=True)
"""
        )

    async def verify(self):
        pass

    async def launch(self, name):
        self.starts.append(name)
        process = await asyncio.create_subprocess_exec(
            sys.executable,
            "-c",
            self.script,
            env={"PATH": os.environ.get("PATH", "")},
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
        )
        self.ready.set()
        return process

    async def cleanup(self, name):
        self.cleanups.append(name)
        return True


class IsolatedTests(unittest.IsolatedAsyncioTestCase):
    def adapter(self, runtime=None):
        client = Checkpoints()
        runtime = runtime or Runtime()
        adapter = IsolatedAdapter("http://fixture", "pool-test", None, runtime=runtime, client=client)
        adapter.graphs = {"echo"}
        return adapter, runtime, client

    def assignment(self, incarnation="first"):
        return {
            "run_id": "same-run-id",
            "run_incarnation_id": incarnation,
            "thread_id": "thread",
            "graph_id": "echo",
            "generation": 1,
            "tenant_id": "acme",
            "input": {"prompt": "synthetic"},
            "config": {"secret_config": "must not cross"},
            "user": {"permissions": ["admin"]},
            "run_manifest": {"credential": "must not cross"},
        }

    async def test_success_cas_receipt_replay_and_reused_run_id(self):
        adapter, runtime, client = self.adapter()
        events = []
        callback = AsyncMock(side_effect=lambda event: events.append(event))
        assignment = self.assignment()
        with patch.dict(os.environ, {"RUNNER_TOKEN": "synthetic parent marker"}):
            self.assertEqual(await adapter.execute(assignment, callback, asyncio.Event()), "success")
        receipt = decode_frame(client.raw)["receipts"]["first"]
        self.assertEqual(receipt["output"]["keys"], ["input", "run", "state", "type"])
        self.assertFalse(receipt["output"]["token_present"])
        assignment["generation"] = 2
        self.assertEqual(await adapter.execute(assignment, callback, asyncio.Event()), "success")
        self.assertEqual(len(runtime.starts), 1)
        self.assertEqual(await adapter.execute(self.assignment("replacement"), callback, asyncio.Event()), "success")
        self.assertEqual(len(runtime.starts), 2)
        self.assertEqual(decode_frame(client.raw)["receipts"]["replacement"]["output"]["count"], 2)
        cleanup = next(i for i, event in enumerate(events) if event["data"].get("event") == "execution_cleanup")
        terminal = next(i for i, event in enumerate(events) if event["method"] == "end")
        self.assertLess(cleanup, terminal)

    async def test_lost_commit_ack_replays_without_second_execution(self):
        adapter, runtime, client = self.adapter()
        client.fail_committed_result = True
        with self.assertRaises(CompletionDeliveryError):
            await adapter.execute(self.assignment(), AsyncMock(), asyncio.Event())
        self.assertEqual(await adapter.execute(self.assignment(), AsyncMock(), asyncio.Event()), "success")
        self.assertEqual(len(runtime.starts), 1)

    async def test_generated_sandbox_names_fit_runtime_limit_and_receipt_cleanup_binding(self):
        adapter, runtime, client = self.adapter()
        for incarnation in ("first", "replacement"):
            self.assertEqual(
                await adapter.execute(self.assignment(incarnation), AsyncMock(), asyncio.Event()), "success"
            )
            receipt = decode_frame(client.raw)["receipts"][incarnation]
            name = runtime.starts[-1]
            self.assertLessEqual(len(name), 19, "OpenShell workload names are at most 19 characters")
            self.assertRegex(name, r"^rk-[a-f0-9]{16}$")
            self.assertEqual(receipt["sandbox"], name)
            self.assertEqual(runtime.cleanups[-1], name)
        self.assertNotEqual(runtime.starts[0], runtime.starts[1])

    async def test_uncertain_launch_is_not_retried(self):
        adapter, runtime, client = self.adapter()
        runtime.launch = AsyncMock(side_effect=RuntimeError("runtime response lost"))
        with self.assertRaises(CompletionDeliveryError):
            await adapter.execute(self.assignment(), AsyncMock(), asyncio.Event())
        self.assertIn("intent", decode_frame(client.raw))
        with self.assertRaisesRegex(CompletionDeliveryError, "requires reconciliation"):
            await adapter.execute(self.assignment(), AsyncMock(), asyncio.Event())
        self.assertEqual(runtime.launch.await_count, 1)
        self.assertEqual(len(runtime.cleanups), 1)

    async def test_missing_incarnation_never_launches(self):
        adapter, runtime, client = self.adapter()
        assignment = self.assignment()
        del assignment["run_incarnation_id"]
        with self.assertRaises(CompletionDeliveryError):
            await adapter.execute(assignment, AsyncMock(), asyncio.Event())
        self.assertEqual(runtime.starts, [])
        self.assertIsNone(client.raw)

    async def test_duplicate_tool_id_cannot_replay_parent_proxy_call(self):
        script = """
import json,sys
sys.stdin.readline()
call={'type':'tool_call','id':'one','connector':'fixture','tool':'echo','arguments':{'value':'synthetic'}}
print(json.dumps(call),flush=True)
sys.stdin.readline()
print(json.dumps(call),flush=True)
"""
        adapter, runtime, client = self.adapter(Runtime(script))
        with patch("runkite_runner.isolated.proxy_connector_mcp", new=AsyncMock(return_value={"result": {}})) as proxy:
            with self.assertRaises(CompletionDeliveryError):
                await adapter.execute(self.assignment(), AsyncMock(), asyncio.Event())
            self.assertEqual(proxy.await_count, 1)
            self.assertEqual(proxy.call_args.args[0], {"configurable": {"run_id": "same-run-id", "generation": 1}})
        self.assertIn("intent", decode_frame(client.raw))
        self.assertEqual(len(runtime.cleanups), 1)

    async def test_cancellation_retains_uncertainty_and_cleans_only_owned_runtime(self):
        adapter, runtime, client = self.adapter(Runtime("import sys,time; sys.stdin.readline(); time.sleep(60)"))
        task = asyncio.create_task(adapter.execute(self.assignment(), AsyncMock(), asyncio.Event()))
        await runtime.ready.wait()
        task.cancel()
        with self.assertRaises(asyncio.CancelledError):
            await task
        self.assertEqual(runtime.cleanups, runtime.starts)
        self.assertIn("intent", decode_frame(client.raw))

    def test_protocol_rejects_ambiguous_or_oversize_frames(self):
        for raw in (
            b'{"type":"result","type":"tool_call"}',
            b'{"x":NaN}',
            b'{"x":1.234567890123456789}',
            b'{"x":1e309}',
            b"[]",
        ):
            with self.assertRaises(ValueError):
                decode_frame(raw)
        with self.assertRaises(ValueError):
            encode_frame({"value": "x" * (1 << 20)})
        self.assertEqual(set(workload_input(self.assignment(), None)), {"type", "input", "state", "run"})

    async def test_runtime_uses_pinned_cli_without_host_env_or_mount_arguments(self):
        with tempfile.TemporaryDirectory() as directory:
            policy = Path(directory) / "policy.yaml"
            policy.write_text("version: 1\n")
            runtime = OpenShellRuntime(
                "openshell",
                "fixture@sha256:" + "a" * 64,
                str(policy),
                ["python3", "/app/agent.py"],
                gateway_endpoint="https://runtime.example.test",
            )
            runtime._control = AsyncMock(return_value=b"openshell 0.1.8\n")
            await runtime.verify()
            with patch("asyncio.create_subprocess_exec", new=AsyncMock()) as execute:
                await runtime.launch("rk-fixture")
                args = runtime._control.call_args.args
                for forbidden in ("--env", "--env-from", "--upload", "--driver-config-json", "--auto-providers"):
                    self.assertNotIn(forbidden, args)
                self.assertIn("--no-auto-providers", args)
                self.assertIn("--no-login-shell", execute.call_args.args)
                self.assertIn("--no-tty", execute.call_args.args)

    async def test_policy_handoff_uses_private_immutable_snapshot_not_mutated_config(self):
        with tempfile.TemporaryDirectory() as directory:
            policy = Path(directory) / "policy.yaml"
            original = b"version: 1\nlandlock:\n  compatibility: hard_requirement\n"
            policy.write_bytes(original)
            runtime = OpenShellRuntime(
                "openshell",
                "fixture@sha256:" + "a" * 64,
                str(policy),
                ["python3", "/app/workload.py"],
                gateway_endpoint="https://runtime.example.test",
            )
            descriptor = runtime.descriptor
            policy.write_text("version: 1\nlandlock:\n  compatibility: best_effort\n")
            observed = []

            async def control(*args):
                snapshot = Path(args[args.index("--policy") + 1])
                observed.append(snapshot)
                self.assertNotEqual(snapshot, policy)
                self.assertEqual(snapshot.stat().st_mode & 0o777, 0o600)
                self.assertEqual(snapshot.parent.stat().st_mode & 0o777, 0o700)
                policy.write_text("version: 1\nfilesystem_policy: {}\n")
                self.assertEqual(snapshot.read_bytes(), original)
                self.assertEqual(runtime.descriptor, descriptor)
                return b""

            runtime._control = control
            with patch("asyncio.create_subprocess_exec", new=AsyncMock()):
                await runtime.launch("rk-" + "a" * 16)
            self.assertFalse(observed[0].exists())
            self.assertFalse(observed[0].parent.exists())
