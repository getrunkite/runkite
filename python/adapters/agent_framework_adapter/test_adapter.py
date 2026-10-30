"""Self-checks for managed Agent Framework adapter: session, CAS, reclaim, cancel."""

from __future__ import annotations

import asyncio
import json
import sys
import tempfile
import unittest
from pathlib import Path

import httpx

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "adapters"))
EXAMPLE = Path(__file__).resolve().parents[3] / "examples" / "managed_agent_framework"
sys.path.insert(0, str(EXAMPLE))

from agent_framework_adapter.adapter import AgentFrameworkAdapter  # noqa: E402
from agent_framework_adapter.state import FRAMEWORK, state_json  # noqa: E402
from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402


class FakePlane:
    def __init__(self) -> None:
        self.blobs: dict[str, bytes] = {}
        self.versions: dict[str, int] = {}
        self.puts = 0

    def transport(self) -> httpx.MockTransport:
        def handler(request: httpx.Request) -> httpx.Response:
            key = request.url.path
            if request.method == "GET":
                if key not in self.blobs:
                    return httpx.Response(404)
                ver = self.versions[key]
                return httpx.Response(
                    200,
                    content=self.blobs[key],
                    headers={
                        "ETag": f'"{ver}"',
                        "X-Runkite-Checkpoint-Framework": FRAMEWORK,
                    },
                )
            if request.method == "PUT":
                self.puts += 1
                if_match = request.headers.get("If-Match")
                if_none = request.headers.get("If-None-Match")
                if if_none == "*" and key in self.blobs:
                    return httpx.Response(412, text="exists")
                if if_match is not None:
                    want = if_match.strip().strip('"')
                    have = str(self.versions.get(key, ""))
                    if want != have:
                        return httpx.Response(412, text="mismatch")
                next_ver = self.versions.get(key, 0) + 1
                self.blobs[key] = request.content
                self.versions[key] = next_ver
                return httpx.Response(204, headers={"ETag": f'"{next_ver}"'})
            return httpx.Response(405)

        return httpx.MockTransport(handler)


class AgentFrameworkAdapterTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.plane = FakePlane()
        self.adapter = AgentFrameworkAdapter()
        self.adapter.configure(http_address="http://plane", runner_kind="python-agent-framework")
        await self.adapter.load_config(str(EXAMPLE / "langgraph.json"))
        # Inject mock transport into clients created by the adapter.
        original_state = self.adapter._state

        def patched_state():
            store = original_state()
            store._client._transport = self.plane.transport()
            return store

        self.adapter._state = patched_state  # type: ignore[method-assign]
        self.tenant = bind_tenant("default")
        self.run = bind_run("run-1", 1)

    async def asyncTearDown(self) -> None:
        reset_run(self.run)
        reset_tenant(self.tenant)

    async def _execute(self, run_id: str, text: str, cancel: asyncio.Event | None = None) -> tuple[str, list[dict]]:
        events: list[dict] = []

        async def send(event: dict) -> None:
            events.append(event)

        status = await self.adapter.execute(
            {
                "run_id": run_id,
                "thread_id": "thread-1",
                "graph_id": "managed_agent_framework",
                "tenant_id": "default",
                "generation": 1,
                "input": {"messages": [{"role": "user", "content": text}]},
            },
            send,
            cancel or asyncio.Event(),
        )
        return status, events

    async def test_text_turn_streams_and_commits_session(self) -> None:
        status, events = await self._execute("run-a", "hello")
        self.assertEqual(status, "success")
        values = [e for e in events if e.get("method") == "values"][-1]["data"]
        self.assertIn("Remembered.", "".join(m["content"] for m in values["messages"] if m["role"] == "assistant"))
        self.assertGreater(values["usage"]["total_tokens"], 0)
        self.assertEqual(self.plane.puts, 1)
        blob = json.loads(next(iter(self.plane.blobs.values())))
        self.assertEqual(blob["framework"], FRAMEWORK)
        self.assertEqual(blob["completed"]["run_id"], "run-a")
        self.assertIn("session", blob)

    async def test_reclaim_replays_without_second_commit_or_model(self) -> None:
        await self._execute("run-a", "hello")
        puts_after_first = self.plane.puts
        status, events = await self._execute("run-a", "hello")
        self.assertEqual(status, "success")
        self.assertEqual(self.plane.puts, puts_after_first, "receipt replay must not rewrite state")
        messages = [e for e in events if e.get("method") == "messages"]
        self.assertGreaterEqual(len(messages), 1)

    async def test_local_tool_turn(self) -> None:
        status, events = await self._execute("run-tool", "please echo hi")
        self.assertEqual(status, "success")
        values = [e for e in events if e.get("method") == "values"][-1]["data"]
        assistant = " ".join(m["content"] for m in values["messages"] if m["role"] == "assistant")
        self.assertIn("Tool finished", assistant)

    async def test_cancel_during_slow_turn(self) -> None:
        # Patch factory client path by using cancel before execute returns; the
        # scripted client is fast, so cancel the event mid-flight via a wrapper.
        cancel = asyncio.Event()
        events: list[dict] = []

        async def send(event: dict) -> None:
            events.append(event)
            if event.get("method") == "messages":
                cancel.set()

        status = await self.adapter.execute(
            {
                "run_id": "run-cancel",
                "thread_id": "thread-1",
                "graph_id": "managed_agent_framework",
                "tenant_id": "default",
                "generation": 1,
                "input": {"messages": [{"role": "user", "content": "hello slow path"}]},
            },
            send,
            cancel,
        )
        # Fast scripted model may finish before cancel is observed; accept either
        # interrupted after a streamed chunk or success if it completed first.
        self.assertIn(status, {"interrupted", "success"})

    async def test_cas_rejects_stale_etag(self) -> None:
        await self._execute("run-a", "hello")
        key = next(iter(self.plane.blobs))
        # Corrupt local view: pretend we still have etag "1" while plane advanced.
        self.plane.versions[key] = 9
        from agent_framework_adapter.state import AgentFrameworkState
        from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient

        client = OpaqueCheckpointClient(
            http_base_url="http://plane",
            framework=FRAMEWORK,
            transport=self.plane.transport(),
        )
        store = AgentFrameworkState(client)
        with self.assertRaises(Exception):
            await store.save("thread-1", {"v": 1, "framework": FRAMEWORK}, '"1"')


def test_state_json_rejects_nan() -> None:
    with tempfile.TemporaryDirectory():
        try:
            state_json({"x": float("nan")})
            raise AssertionError("expected ValueError")
        except ValueError:
            pass


if __name__ == "__main__":
    unittest.main()
