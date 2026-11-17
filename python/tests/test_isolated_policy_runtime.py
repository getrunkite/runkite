"""Qualification interception ordering, without starting an external runtime."""

import importlib.util
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "python"))
sys.path.insert(0, str(ROOT / "scripts"))
spec = importlib.util.spec_from_file_location("isolated_policy_runtime_fixture", ROOT / "scripts/qualify-isolated.py")
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


class QualificationOrderingTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        policy = Path(self.directory.name) / "policy.yaml"
        policy.write_text("version: 1\n")
        self.runtime = fixture.ObservedRuntime(
            "openshell",
            "synthetic@sha256:" + "a" * 64,
            str(policy),
            ["python3", "/app/workload.py"],
            gateway_endpoint="https://runtime.example.test",
        )
        self.order = []
        self.name = "rk-" + "a" * 16

        async def before(*_args):
            self.order.append("before")

        async def after(*_args):
            self.order.append("after")

        self.guard = SimpleNamespace(before_exec=AsyncMock(side_effect=before), after_exec=AsyncMock(side_effect=after))
        self.runtime.policy_qualification = self.guard

    async def control(self, _runtime, *args):
        self.order.append(args[1])
        return b'{"sandboxes":[]}' if args[:2] == ("sandbox", "list") else b""

    async def execute(self, *_args, **_kwargs):
        self.order.append("exec")
        return object()

    def management(self):
        async def control(runtime, *args):
            return await self.control(runtime, *args)

        return patch.object(fixture.OpenShellRuntime, "_control", new=control)

    async def test_capture_is_between_create_and_exec_and_post_inspection_precedes_delete(self):
        with self.management(), patch.object(fixture.asyncio, "create_subprocess_exec", side_effect=self.execute):
            await self.runtime.launch(self.name)
            await self.runtime.cleanup(self.name)
            await self.runtime.cleanup(self.name)  # Receipt replay can repeat owned deletion.
        self.assertEqual(self.order, ["create", "before", "exec", "after", "delete", "list", "delete", "list"])
        self.guard.after_exec.assert_awaited_once()

    async def test_failed_precheck_never_executes_and_owned_deletion_still_works(self):
        self.guard.before_exec.side_effect = RuntimeError("synthetic inspection failure")
        with self.management(), patch.object(fixture.asyncio, "create_subprocess_exec", new=AsyncMock()) as execute:
            with self.assertRaises(RuntimeError):
                await self.runtime.launch(self.name)
            await self.runtime.cleanup(self.name)
        execute.assert_not_awaited()
        self.guard.after_exec.assert_not_awaited()
        self.assertEqual(self.order, ["create", "delete", "list"])

    async def test_failed_postcheck_does_not_prevent_delete_or_get_retried_as_success(self):
        self.guard.after_exec.side_effect = RuntimeError("synthetic drift")
        with self.management(), patch.object(fixture.asyncio, "create_subprocess_exec", side_effect=self.execute):
            await self.runtime.launch(self.name)
            with self.assertRaisesRegex(RuntimeError, "synthetic drift"):
                await self.runtime.cleanup(self.name)
            await self.runtime.cleanup(self.name)
        self.assertEqual(self.order, ["create", "before", "exec", "delete", "list", "delete", "list"])
        self.guard.after_exec.assert_awaited_once()

    async def test_without_checker_legacy_capture_adds_no_policy_or_prover_calls(self):
        self.runtime.policy_qualification = None
        with self.management(), patch.object(fixture.asyncio, "create_subprocess_exec", side_effect=self.execute):
            await self.runtime.launch(self.name)
            await self.runtime.cleanup(self.name)
        self.assertEqual(self.order, ["create", "exec", "delete", "list"])

    def test_qualification_refuses_explicit_providers_before_creating_a_sandbox(self):
        policy = Path(self.directory.name) / "policy.yaml"
        with self.assertRaisesRegex(ValueError, "provider-free"):
            fixture.ObservedRuntime(
                "openshell",
                "synthetic@sha256:" + "a" * 64,
                str(policy),
                ["python3", "/app/workload.py"],
                providers=["synthetic-provider"],
                gateway_endpoint="https://runtime.example.test",
                prover="unused",
            )


if __name__ == "__main__":
    unittest.main()
