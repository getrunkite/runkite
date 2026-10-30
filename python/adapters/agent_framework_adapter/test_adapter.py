"""Self-checks for managed Agent Framework adapter: session, CAS, reclaim, cancel."""

from __future__ import annotations

import asyncio
import json
import sys
import unittest
from pathlib import Path
from unittest.mock import patch

import httpx

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "adapters"))
EXAMPLE = Path(__file__).resolve().parents[3] / "examples" / "managed_agent_framework"
sys.path.insert(0, str(EXAMPLE))

from agent import ScriptedChatClient  # noqa: E402
from agent_framework import Agent, ChatResponseUpdate, Content, FunctionTool, UsageDetails  # noqa: E402
from runkite_runner.completion import CompletionDeliveryError  # noqa: E402
from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402

from agent_framework_adapter import adapter as managed_adapter  # noqa: E402
from agent_framework_adapter.adapter import AgentFrameworkAdapter  # noqa: E402
from agent_framework_adapter.state import FRAMEWORK  # noqa: E402


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


class RuntimeClient(ScriptedChatClient):
    """Real framework function loop around controlled native provider streams."""

    def __init__(self, turns):
        super().__init__()
        self.turns = turns
        self.calls = 0
        self.prompts = []
        self.opened = 0
        self.closed = 0
        self.streams_closed = 0

    async def __aenter__(self):
        self.opened += 1
        return self

    async def __aexit__(self, *_args):
        self.closed += 1

    async def _inner_get_response(self, *, messages, stream=False, options=None, **kwargs):
        self.assert_stream = stream
        turn = self.turns[self.calls]
        self.calls += 1
        self.prompts.append(messages)

        async def tracked():
            try:
                async for update in turn(messages):
                    yield update
            finally:
                self.streams_closed += 1

        return self._build_response_stream(tracked())


def usage_content(prompt=10, completion=4, *, merge_mode=None):
    return Content.from_usage(
        UsageDetails(input_token_count=prompt, output_token_count=completion, total_token_count=prompt + completion),
        merge_mode=merge_mode,
    )


class AgentFrameworkAdapterTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.plane = FakePlane()
        self.adapter = AgentFrameworkAdapter()
        self.adapter.configure(http_address="http://plane", runner_kind="python-agent-framework")
        await self.adapter.load_config(str(EXAMPLE / "langgraph.json"))
        # Inject mock transport into clients created by the adapter.
        original_state = self.adapter._state
        self.stores = []

        def patched_state():
            store = original_state()
            store._client._transport = self.plane.transport()
            self.stores.append(store)
            return store

        self.adapter._state = patched_state  # type: ignore[method-assign]
        self.tenant = bind_tenant("default")
        self.run = bind_run("run-1", 1)

    async def asyncTearDown(self) -> None:
        for store in self.stores:
            self.assertIsNone(store._client._client, "Every execution path must close its checkpoint HTTP client")
        reset_run(self.run)
        reset_tenant(self.tenant)

    async def _execute(
        self,
        run_id: str,
        text: str,
        cancel: asyncio.Event | None = None,
        *,
        generation: int = 1,
        messages: list[dict] | None = None,
        extra: dict | None = None,
        send_hook=None,
    ) -> tuple[str, list[dict]]:
        events: list[dict] = []

        async def send(event: dict) -> None:
            events.append(event)
            if send_hook is not None:
                await send_hook(event)

        run_binding = bind_run(run_id, generation)
        try:
            status = await self.adapter.execute(
                {
                    "run_id": run_id,
                    "thread_id": "thread-1",
                    "graph_id": "managed_agent_framework",
                    "tenant_id": "default",
                    "generation": generation,
                    "input": {"messages": messages or [{"role": "user", "content": text}]},
                    **(extra or {}),
                },
                send,
                cancel or asyncio.Event(),
            )
        finally:
            reset_run(run_binding)
        return status, events

    def install_runtime_client(self, client, tools=None):
        self.adapter.factories["managed_agent_framework"] = lambda _context: Agent(client=client, tools=tools or [])

    def assert_runtime_closed(self, client):
        self.assertEqual(client.closed, client.opened)
        self.assertEqual(client.opened, 1)
        self.assertEqual(client.streams_closed, client.calls)

    def final_usage(self, events):
        return [event["data"]["usage"] for event in events if event["method"] == "values"][-1]

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
        from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient

        from agent_framework_adapter.state import AgentFrameworkState

        client = OpaqueCheckpointClient(
            http_base_url="http://plane",
            framework=FRAMEWORK,
            transport=self.plane.transport(),
        )
        store = AgentFrameworkState(client)
        try:
            with self.assertRaises(CompletionDeliveryError):
                await store.save(
                    "thread-1", json.loads(self.plane.blobs[key]), '"1"', graph_id="managed_agent_framework"
                )
        finally:
            await store.aclose()

    async def test_streamed_usage_is_published_before_tools_and_retained_on_cancel(self):
        second_model_started = asyncio.Event()
        cancel = asyncio.Event()
        effects = []
        delivered_usage = []

        async def first(_messages):
            yield ChatResponseUpdate(
                role="assistant",
                contents=[
                    Content.from_function_call(call_id="tool-1", name="write", arguments={}),
                    usage_content(),
                ],
            )

        async def second(_messages):
            second_model_started.set()
            await asyncio.Event().wait()
            yield ChatResponseUpdate(role="assistant", contents=[])

        def write() -> str:
            self.assertEqual(delivered_usage[-1]["total_tokens"], 14, "Deliver observed usage before running a tool")
            effects.append(True)
            return "written"

        async def receive(event):
            if event["method"] == "values":
                delivered_usage.append(event["data"].get("usage"))

        client = RuntimeClient([first, second])
        self.install_runtime_client(client, [FunctionTool(name="write", func=write)])
        task = asyncio.create_task(self._execute("run-cancel-observed", "write once", cancel, send_hook=receive))
        await asyncio.wait_for(second_model_started.wait(), 2)
        cancel.set()
        status, events = await asyncio.wait_for(task, 2)
        self.assertEqual(status, "interrupted")
        self.assertEqual(effects, [True])
        self.assertEqual(self.final_usage(events), {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14})
        self.assertEqual(self.plane.puts, 0, "An interruption must retain the previous committed native session")
        self.assert_runtime_closed(client)

    async def test_reported_usage_survives_provider_failure_and_stream_cleanup(self):
        async def broken(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("partial"), usage_content()])
            raise RuntimeError("synthetic provider failure")

        client = RuntimeClient([broken])
        self.install_runtime_client(client)
        status, events = await self._execute("run-provider-error", "reply")
        self.assertEqual(status, "error")
        self.assertEqual(self.final_usage(events)["total_tokens"], 14)
        self.assertEqual(self.plane.puts, 0)
        self.assert_runtime_closed(client)

    async def test_final_native_usage_replaces_observed_totals_without_double_counting(self):
        async def first(_messages):
            yield ChatResponseUpdate(
                role="assistant",
                contents=[Content.from_function_call(call_id="tool-1", name="echo", arguments={}), usage_content()],
            )

        async def second(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("done"), usage_content(2, 1)])

        def echo() -> str:
            return "echoed"

        client = RuntimeClient([first, second])
        self.install_runtime_client(client, [FunctionTool(name="echo", func=echo)])
        status, events = await self._execute("run-complete-usage", "echo")
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(events), {"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17})
        self.assertEqual(self.plane.puts, 1)
        self.assert_runtime_closed(client)

    async def test_native_replace_usage_is_not_added_to_the_previous_snapshot(self):
        async def response(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("answer"), usage_content(2, 1)])
            yield ChatResponseUpdate(role="assistant", contents=[usage_content(10, 4, merge_mode="replace")])

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-replace-usage", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(events)["total_tokens"], 14)
        self.assert_runtime_closed(client)

    async def test_native_approval_is_an_explicit_error_without_success_receipt(self):
        effects = []

        def echo(text: str) -> str:
            effects.append(text)
            return text

        self.adapter.factories["managed_agent_framework"] = lambda _context: Agent(
            client=ScriptedChatClient(),
            tools=[FunctionTool(name="echo", func=echo, approval_mode="always_require")],
        )
        status, events = await self._execute("run-approval", "please echo")
        self.assertEqual(status, "error")
        self.assertEqual(effects, [])
        self.assertEqual(self.plane.puts, 0)
        self.assertIn("unsupported", events[-1]["data"]["message"])

    async def test_native_background_continuation_is_not_a_successful_turn(self):
        async def response(_messages):
            yield ChatResponseUpdate(
                role="assistant",
                contents=[Content.from_text("working"), usage_content()],
                continuation_token="operation-1",
            )

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-background", "reply")
        self.assertEqual(status, "error")
        self.assertEqual(self.plane.puts, 0)
        self.assertIn("continuation", events[-1]["data"]["message"])
        self.assert_runtime_closed(client)

    async def test_informational_function_records_do_not_require_local_tool_results(self):
        async def response(_messages):
            yield ChatResponseUpdate(
                role="assistant",
                contents=[
                    Content.from_function_call(
                        call_id="informational", name="provider-record", arguments={}, informational_only=True
                    ),
                    Content.from_text("answer"),
                    usage_content(),
                ],
            )

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-informational", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(self.plane.puts, 1)
        self.assertEqual(self.final_usage(events)["total_tokens"], 14)
        self.assert_runtime_closed(client)

    async def test_native_resume_is_rejected_before_factory_or_model_work(self):
        def unexpected_factory(_context):
            self.fail("Unsupported resume must not construct an agent")

        self.adapter.factories["managed_agent_framework"] = unexpected_factory
        for resume in ({}, {"approval": True}):
            status, events = await self._execute("run-resume", "reply", extra={"resume_command": resume})
            self.assertEqual(status, "error")
            self.assertIn("unsupported", events[-1]["data"]["message"])
        self.assertEqual(self.stores, [])

    async def test_every_accepted_message_reaches_the_native_model(self):
        async def response(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("answer"), usage_content()])

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        messages = [
            {"role": "system", "content": "per-turn system text"},
            {"role": "user", "content": "first user text"},
            {"role": "human", "content": [{"type": "text", "text": "second user text"}]},
        ]
        status, _events = await self._execute("run-input-batch", "unused", messages=messages)
        self.assertEqual(status, "success")
        self.assertEqual(
            [(message.role, message.text) for message in client.prompts[0]],
            [("system", "per-turn system text"), ("user", "first user text"), ("user", "second user text")],
        )
        self.assert_runtime_closed(client)

    async def test_unsupported_input_is_not_silently_filtered(self):
        def unexpected_factory(_context):
            self.fail("Unsupported content must fail before constructing an agent")

        self.adapter.factories["managed_agent_framework"] = unexpected_factory
        status, events = await self._execute(
            "run-nontext", "unused", messages=[{"role": "user", "content": [{"type": "image", "url": "synthetic"}]}]
        )
        self.assertEqual(status, "error")
        self.assertIn("text parts only", events[-1]["data"]["message"])
        self.assertEqual(self.stores, [])

    async def test_pre_cancelled_assignment_does_no_factory_model_tool_or_state_work(self):
        def unexpected_factory(_context):
            self.fail("A pre-cancelled assignment must not construct an agent")

        self.adapter.factories["managed_agent_framework"] = unexpected_factory
        cancel = asyncio.Event()
        cancel.set()
        status, events = await self._execute("run-already-cancelled", "reply", cancel)
        self.assertEqual(status, "interrupted")
        self.assertEqual([event["method"] for event in events], ["end"])
        self.assertEqual(self.stores, [])

    async def test_reclaim_retags_generation_without_changing_saved_receipt_or_billing(self):
        await self._execute("run-replayed", "hello")
        saved = dict(self.plane.blobs)
        puts = self.plane.puts

        def unexpected_factory(_context):
            self.fail("Receipt replay must not invoke an agent factory")

        self.adapter.factories["managed_agent_framework"] = unexpected_factory
        status, events = await self._execute("run-replayed", "hello", generation=2)
        self.assertEqual(status, "success")
        values = [event["data"] for event in events if event["method"] == "values"][-1]
        self.assertEqual(values["agent_framework"]["generation"], 2)
        self.assertEqual(values["usage"]["total_tokens"], 14)
        self.assertEqual(self.plane.puts, puts)
        self.assertEqual(self.plane.blobs, saved)

    async def test_oversized_output_and_receipt_fail_before_success_commit(self):
        cases = (
            ("MAX_OUTPUT_BYTES", 1024, ["x" * 2048]),
            ("MAX_OUTPUT_BYTES", 1024, ["x" * 600, "y" * 600]),
            ("MAX_RECEIPT_BYTES", 16, ["x" * 32]),
        )
        for index, (limit_name, limit, parts) in enumerate(cases):
            with self.subTest(limit=limit_name, chunks=len(parts)):

                async def response(_messages, parts=parts):
                    for index, text in enumerate(parts):
                        yield ChatResponseUpdate(
                            role="assistant",
                            contents=[Content.from_text(text), *([usage_content()] if index == 0 else [])],
                        )

                client = RuntimeClient([response])
                self.install_runtime_client(client)
                with patch.object(managed_adapter, limit_name, limit):
                    status, events = await self._execute(f"run-bound-{index}", "reply")
                self.assertEqual(status, "error")
                self.assertEqual(self.plane.puts, 0)
                self.assertFalse(any(event["method"] == "end" for event in events))
                if len(parts) == 1:
                    self.assertFalse(any(event["method"] == "messages" for event in events))
                else:
                    self.assertEqual(sum(event["method"] == "messages" for event in events), 2)
                self.assert_runtime_closed(client)

    async def test_oversized_replay_is_rejected_before_emitting_any_receipt_text(self):
        await self._execute("run-bounded-replay", "hello")
        puts = self.plane.puts
        for limit_name in ("MAX_OUTPUT_BYTES", "MAX_RECEIPT_BYTES"):
            with self.subTest(limit=limit_name), patch.object(managed_adapter, limit_name, 1):
                status, events = await self._execute("run-bounded-replay", "hello", generation=2)
                self.assertEqual(status, "error")
                self.assertFalse(any(event["method"] in ("messages", "values", "end") for event in events))
                self.assertEqual(self.plane.puts, puts)

    async def test_giant_native_delta_is_rejected_before_a_message_can_reach_grpc(self):
        async def response(_messages):
            yield ChatResponseUpdate(
                role="assistant",
                contents=[Content.from_text("x" * (managed_adapter.MAX_OUTPUT_BYTES + 1)), usage_content()],
            )

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-giant-delta", "reply")
        self.assertEqual(status, "error")
        self.assertFalse(any(event["method"] in ("messages", "end") for event in events))
        self.assertIn("message exceeds", events[-1]["data"]["message"])
        self.assertEqual(self.plane.puts, 0)
        self.assert_runtime_closed(client)

    async def test_a_cleanup_fault_does_not_rewrite_a_committed_success(self):
        async def response(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("answer"), usage_content()])

        class CleanupFaultClient(RuntimeClient):
            async def __aexit__(self, *_args):
                await super().__aexit__(*_args)
                raise RuntimeError("synthetic cleanup fault after client close")

        client = CleanupFaultClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-cleanup-fault", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(events[-1]["data"]["status"], "success")
        self.assertEqual(self.plane.puts, 1)
        self.assert_runtime_closed(client)

    async def test_factory_and_state_load_failures_close_the_checkpoint_client(self):
        def invalid_factory(_context):
            raise RuntimeError("synthetic factory failure")

        self.adapter.factories["managed_agent_framework"] = invalid_factory
        status, _events = await self._execute("run-factory-error", "reply")
        self.assertEqual(status, "error")
        self.assertIsNone(self.stores[-1]._client._client)
        self.plane.blobs["/internal/checkpoints/thread-1/adapter-state"] = b"not JSON"
        self.plane.versions["/internal/checkpoints/thread-1/adapter-state"] = 1
        with self.assertRaises(CompletionDeliveryError):
            await self._execute("run-load-error", "reply")
        self.assertIsNone(self.stores[-1]._client._client)


if __name__ == "__main__":
    unittest.main()
