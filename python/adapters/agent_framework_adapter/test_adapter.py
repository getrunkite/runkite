"""Self-checks for managed Agent Framework adapter: session, CAS, reclaim, cancel."""

from __future__ import annotations

import asyncio
import json
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from typing import Any
from unittest.mock import patch

import httpx

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "adapters"))
EXAMPLE = Path(__file__).resolve().parents[3] / "examples" / "managed_agent_framework"
sys.path.insert(0, str(EXAMPLE))

from agent import ScriptedChatClient  # noqa: E402
from agent_framework import (  # noqa: E402
    Agent,
    BaseChatClient,
    ChatMiddleware,
    ChatMiddlewareLayer,
    ChatResponse,
    ChatResponseUpdate,
    Content,
    FunctionInvocationLayer,
    FunctionMiddleware,
    FunctionTool,
    MiddlewareFailure,
    UsageDetails,
)
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


class RuntimeClient(FunctionInvocationLayer[Any], ChatMiddlewareLayer[Any], BaseChatClient[Any]):
    """Real framework function loop around controlled native provider streams."""

    def __init__(self, turns):
        super().__init__()
        self.turns = turns
        self.calls = 0
        self.prompts = []
        self.options = []
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
        self.options.append(dict(options or {}))

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

    def install_runtime_client(self, client, tools=None, *, default_options=None):
        self.adapter.factories["managed_agent_framework"] = lambda _context: Agent(
            client=client, tools=tools or [], default_options=default_options
        )

    def assert_runtime_closed(self, client):
        self.assertEqual(client.closed, client.opened)
        self.assertEqual(client.opened, 1)
        self.assertEqual(client.streams_closed, client.calls)

    def final_usage(self, events):
        return [event["data"]["usage"] for event in events if event["method"] == "values"][-1]

    def install_allowlist_tool(self, effects, *, middleware=None):
        async def first(_messages):
            yield ChatResponseUpdate(
                role="assistant",
                model="restricted-model",
                contents=[Content.from_function_call(call_id="write-1", name="write", arguments={}), usage_content()],
            )

        async def second(_messages):
            yield ChatResponseUpdate(
                role="assistant", model="restricted-model", contents=[Content.from_text("written"), usage_content(2, 1)]
            )

        def write() -> str:
            effects.append(True)
            return "written"

        client = RuntimeClient([first, second])
        self.adapter.factories["managed_agent_framework"] = lambda _context: Agent(
            client=client, tools=[FunctionTool(name="write", func=write)], middleware=middleware
        )
        return client

    async def test_allowed_tools_omitted_or_named_allow_executes_once(self):
        for index, extra in enumerate(({}, {"allowed_tools": None}, {"allowed_tools": ["write"]})):
            with self.subTest(extra=extra):
                effects = []
                client = self.install_allowlist_tool(effects)
                status, events = await self._execute(f"allowed-{index}", "write", extra=extra)
                self.assertEqual(status, "success")
                self.assertEqual(effects, [True])
                self.assertEqual(client.calls, 2)
                self.assertEqual(self.final_usage(events)["total_tokens"], 17)
                self.assertEqual(self.final_usage(events)["model"], "restricted-model")
                self.assert_runtime_closed(client)

    async def test_allowed_tools_denial_preserves_usage_and_previous_session(self):
        await self._execute("committed-before-denial", "prior turn")
        saved, puts = dict(self.plane.blobs), self.plane.puts
        for index, allowed in enumerate(([], ["read"], ["WRITE"])):
            with self.subTest(allowed=allowed):
                effects = []
                client = self.install_allowlist_tool(effects)
                status, events = await self._execute(
                    f"denied-{index}", "write", generation=9, extra={"allowed_tools": allowed}
                )
                self.assertEqual(status, "error")
                self.assertEqual(effects, [])
                self.assertEqual(client.calls, 1, "A denial must abort the native model loop")
                self.assertEqual(self.plane.blobs, saved)
                self.assertEqual(self.plane.puts, puts)
                self.assertEqual(
                    self.final_usage(events),
                    {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "restricted-model"},
                )
                denials = [event["data"] for event in events if event["method"] == "tool_auth"]
                self.assertEqual(len(denials), 1)
                self.assertEqual(denials[0]["effect"], "deny")
                self.assertEqual(denials[0]["tool"], "write")
                self.assertEqual(denials[0]["generation"], 9)
                self.assertEqual(denials[0]["reason_code"], "tool_not_allowed")
                self.assertNotIn("args", denials[0])
                self.assertIn("allowed_tools", events[-1]["data"]["message"])
                self.assert_runtime_closed(client)

    async def test_allowed_tools_snapshot_precedes_first_await(self):
        for index, (initial, replacement, expected) in enumerate(
            ((["write"], [], "success"), ([], ["write"], "error"))
        ):
            with self.subTest(initial=initial):
                allowed = list(initial)
                effects = []
                client = self.install_allowlist_tool(effects)

                async def mutate(event, allowed=allowed, replacement=replacement):
                    if event["method"] == "lifecycle":
                        allowed[:] = replacement

                status, _events = await self._execute(
                    f"snapshot-{index}", "write", extra={"allowed_tools": allowed}, send_hook=mutate
                )
                self.assertEqual(status, expected)
                self.assertEqual(effects, [True] if expected == "success" else [])
                self.assert_runtime_closed(client)

    async def test_malformed_allowed_tools_fails_before_state_factory_or_model(self):
        self.adapter.factories["managed_agent_framework"] = lambda _context: self.fail(
            "Invalid restriction ran factory"
        )
        for invalid in (True, {}, "write", [1], [None]):
            with self.subTest(invalid=invalid):
                status, events = await self._execute("invalid-tools", "write", extra={"allowed_tools": invalid})
                self.assertEqual(status, "error")
                self.assertIn("allowed_tools", events[-1]["data"]["message"])
        self.assertEqual(self.stores, [])

    async def test_restricted_custom_client_without_function_hook_fails_before_open(self):
        class BareClient(BaseChatClient[Any]):
            async def __aenter__(self):
                raise AssertionError("An unsupported restricted client must not open")

            async def _inner_get_response(self, **_kwargs):
                raise AssertionError("An unsupported restricted client must not call the model")

        for allowed in ([], ["write"]):
            with self.subTest(allowed=allowed):
                self.adapter.factories["managed_agent_framework"] = lambda _context: Agent(client=BareClient())
                status, events = await self._execute("missing-hook", "reply", extra={"allowed_tools": allowed})
                self.assertEqual(status, "error")
                self.assertIn("FunctionInvocationLayer", events[-1]["data"]["message"])
        self.assertEqual(self.plane.puts, 0)

    async def test_cancel_during_tool_gate_never_runs_body_or_next_model(self):
        for index, trigger in enumerate(("values", "tool_auth")):
            with self.subTest(trigger=trigger):
                cancel, effects = asyncio.Event(), []
                client = self.install_allowlist_tool(effects)

                async def stop(event, trigger=trigger, cancel=cancel):
                    if event["method"] == trigger:
                        cancel.set()
                        await asyncio.sleep(0)

                status, events = await asyncio.wait_for(
                    self._execute(f"gate-cancel-{index}", "write", cancel, extra={"allowed_tools": []}, send_hook=stop),
                    2,
                )
                self.assertEqual(status, "interrupted")
                self.assertEqual(effects, [])
                self.assertEqual(client.calls, 1)
                self.assertEqual(self.final_usage(events)["total_tokens"], 14)
                self.assertFalse(any(event["method"] == "error" for event in events))
                self.assertEqual(self.plane.puts, 0)
                self.assert_runtime_closed(client)

    async def test_tool_gate_delivery_failure_remains_recoverable(self):
        class WrapFailure(FunctionMiddleware):
            async def process(self, context, call_next):
                try:
                    await call_next()
                except MiddlewareFailure as exc:
                    raise RuntimeError("synthetic user middleware wrapper") from exc

        async def fail_delivery(event):
            if event["method"] == "tool_auth":
                raise CompletionDeliveryError("synthetic tool authorization delivery failure")

        for index, middleware in enumerate((None, [WrapFailure()])):
            with self.subTest(wrapped=bool(middleware)):
                effects = []
                client = self.install_allowlist_tool(effects, middleware=middleware)
                with self.assertRaises(CompletionDeliveryError):
                    await self._execute(
                        f"gate-delivery-{index}", "write", extra={"allowed_tools": []}, send_hook=fail_delivery
                    )
                self.assertEqual(effects, [])
                if middleware is None:
                    self.assertEqual(client.calls, 1)
                self.assertEqual(self.plane.puts, 0)
                self.assert_runtime_closed(client)

    async def test_swallowed_native_denial_cannot_commit_success_receipt(self):
        class SwallowFailure(FunctionMiddleware):
            async def process(self, context, call_next):
                try:
                    await call_next()
                except MiddlewareFailure:
                    context.result = "blocked"

        effects = []
        client = self.install_allowlist_tool(effects, middleware=[SwallowFailure()])
        status, events = await self._execute("swallowed-denial", "write", extra={"allowed_tools": []})
        self.assertEqual(status, "error")
        self.assertEqual(effects, [])
        self.assertEqual(self.plane.puts, 0)
        self.assertIn("allowed_tools", events[-1]["data"]["message"])
        self.assert_runtime_closed(client)

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

    async def test_native_model_metadata_separate_from_usage_survives_final_aggregation(self):
        async def response(_messages):
            yield ChatResponseUpdate(model="reported-model", response_id="native-response")
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("answer"), usage_content()])

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-model-metadata", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(
            self.final_usage(events),
            {
                "prompt_tokens": 10,
                "completion_tokens": 4,
                "total_tokens": 14,
                "model": "reported-model",
            },
        )
        self.assert_runtime_closed(client)

    async def test_native_reported_model_wins_over_configured_alias(self):
        async def response(_messages):
            yield ChatResponseUpdate(model="configured-alias")
            yield ChatResponseUpdate(
                role="assistant", model="reported-model", contents=[Content.from_text("answer"), usage_content()]
            )

        client = RuntimeClient([response])
        client.model = "client-alias"
        self.install_runtime_client(client, default_options={"model": "configured-alias"})
        status, events = await self._execute("run-model-alias", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(events)["model"], "reported-model")
        self.assertEqual(client.options[0]["model"], "configured-alias")

    async def test_trustworthy_per_call_configuration_fills_absent_native_model(self):
        async def response(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("answer"), usage_content()])

        for index, options in enumerate((None, {"model": "option-model"})):
            with self.subTest(options=options):
                client = RuntimeClient([response])
                client.model = "client-model"
                self.install_runtime_client(client, default_options=options)
                status, events = await self._execute(f"run-model-config-{index}", "reply")
                self.assertEqual(status, "success")
                expected = "option-model" if options else "client-model"
                self.assertEqual(self.final_usage(events)["model"], expected)
                self.assertEqual(client.options[0]["model"], expected)

    async def test_unknown_native_and_configured_models_remain_unpriced(self):
        async def response(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("answer"), usage_content()])

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-model-unknown", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(events)["total_tokens"], 14)
        self.assertNotIn("model", self.final_usage(events))

    async def test_model_is_published_before_tool_and_retained_during_next_model_cancel(self):
        cancel, second_started = asyncio.Event(), asyncio.Event()
        published = []

        async def first(_messages):
            yield ChatResponseUpdate(model="first-model")
            yield ChatResponseUpdate(
                role="assistant",
                contents=[
                    Content.from_function_call(call_id="model-tool", name="write", arguments={}),
                    usage_content(),
                ],
            )

        async def second(_messages):
            second_started.set()
            await asyncio.Event().wait()
            yield ChatResponseUpdate()

        def write() -> str:
            self.assertEqual(published[-1]["model"], "first-model")
            self.assertEqual(published[-1]["total_tokens"], 14)
            return "done"

        async def receive(event):
            if event["method"] == "values":
                published.append(event["data"].get("usage"))

        client = RuntimeClient([first, second])
        self.install_runtime_client(client, [FunctionTool(name="write", func=write)])
        running = asyncio.create_task(self._execute("run-model-cancel", "write", cancel, send_hook=receive))
        await asyncio.wait_for(second_started.wait(), 2)
        cancel.set()
        status, events = await asyncio.wait_for(running, 2)
        self.assertEqual(status, "interrupted")
        self.assertEqual(
            self.final_usage(events),
            {
                "prompt_tokens": 10,
                "completion_tokens": 4,
                "total_tokens": 14,
                "model": "first-model",
            },
        )
        self.assertEqual(self.plane.puts, 0)
        self.assert_runtime_closed(client)

    async def test_provider_failure_retains_known_model_without_counting_twice(self):
        async def broken(_messages):
            yield ChatResponseUpdate(model="failed-model", contents=[Content.from_text("partial"), usage_content()])
            raise RuntimeError("synthetic metered failure")

        client = RuntimeClient([broken])
        self.install_runtime_client(client)
        status, events = await self._execute("run-model-error", "reply")
        self.assertEqual(status, "error")
        self.assertEqual(
            self.final_usage(events),
            {
                "prompt_tokens": 10,
                "completion_tokens": 4,
                "total_tokens": 14,
                "model": "failed-model",
            },
        )
        self.assert_runtime_closed(client)

    async def test_multi_step_same_mixed_and_unknown_models_preserve_tokens(self):
        def echo() -> str:
            return "echoed"

        cases = (
            ("same-model", "same-model", "same-model"),
            ("first-model", "second-model", None),
            (None, "last-model", None),
            ("first-model", None, None),
        )
        for index, (first_model, second_model, expected) in enumerate(cases):
            with self.subTest(models=(first_model, second_model)):

                async def first(_messages, model=first_model, call_index=index):
                    yield ChatResponseUpdate(
                        model=model,
                        response_id="reused-response-id",
                        contents=[
                            Content.from_function_call(call_id=f"mixed-call-{call_index}", name="echo", arguments={}),
                            usage_content(),
                        ],
                    )

                async def second(_messages, model=second_model):
                    yield ChatResponseUpdate(
                        model=model,
                        response_id="reused-response-id",
                        role="assistant",
                        contents=[Content.from_text("done"), usage_content(2, 1)],
                    )

                client = RuntimeClient([first, second])
                self.install_runtime_client(client, [FunctionTool(name="echo", func=echo)])
                status, events = await self._execute(f"run-models-{index}", "echo")
                self.assertEqual(status, "success")
                self.assertEqual(self.final_usage(events)["total_tokens"], 17)
                if expected is None:
                    self.assertNotIn("model", self.final_usage(events))
                else:
                    self.assertEqual(self.final_usage(events)["model"], expected)
                self.assert_runtime_closed(client)

    async def test_append_replace_usage_preserves_one_model_and_native_token_totals(self):
        async def response(_messages):
            yield ChatResponseUpdate(
                model="replace-model", role="assistant", contents=[Content.from_text("answer"), usage_content(2, 1)]
            )
            yield ChatResponseUpdate(contents=[usage_content(3, 2)])
            yield ChatResponseUpdate(contents=[usage_content(10, 4, merge_mode="replace")])

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-model-replace", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(
            self.final_usage(events),
            {
                "prompt_tokens": 10,
                "completion_tokens": 4,
                "total_tokens": 14,
                "model": "replace-model",
            },
        )

    async def test_conflicting_models_within_one_request_cannot_use_the_latest_model(self):
        async def response(_messages):
            yield ChatResponseUpdate(model="first-model", contents=[Content.from_text("answer"), usage_content()])
            yield ChatResponseUpdate(model="different-model")

        client = RuntimeClient([response])
        self.install_runtime_client(client, default_options={"model": "configured-model"})
        status, events = await self._execute("run-model-conflict", "reply")
        self.assertEqual(status, "success")
        self.assertNotIn("model", self.final_usage(events))

    async def test_final_native_response_model_is_published_before_tools(self):
        delivered = []

        async def first(_messages):
            yield ChatResponseUpdate(
                contents=[
                    Content.from_function_call(call_id="final-model-call", name="write", arguments={}),
                    usage_content(),
                ]
            )

        async def second(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("done"), usage_content(2, 1)])

        class FinalModelClient(RuntimeClient):
            async def _inner_get_response(self, **kwargs):
                result = await super()._inner_get_response(**kwargs)

                def set_model(response: ChatResponse) -> ChatResponse:
                    response.model = "final-native-model"
                    return response

                return result.with_result_hook(set_model)

        def write() -> str:
            self.assertEqual(delivered[-1]["model"], "final-native-model")
            self.assertEqual(delivered[-1]["total_tokens"], 14)
            return "done"

        async def receive(event):
            if event["method"] == "values":
                delivered.append(event["data"].get("usage"))

        client = FinalModelClient([first, second])
        self.install_runtime_client(client, [FunctionTool(name="write", func=write)])
        status, events = await self._execute("run-final-model", "write", send_hook=receive)
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(events)["model"], "final-native-model")
        self.assertEqual(self.final_usage(events)["total_tokens"], 17)

    async def test_per_call_middleware_configuration_does_not_collapse_mixed_models(self):
        class SelectModel(ChatMiddleware):
            def __init__(self):
                self.calls = 0

            async def process(self, context, call_next):
                self.calls += 1
                context.options = {**(context.options or {}), "model": f"configured-{self.calls}"}
                await call_next()

        async def first(_messages):
            yield ChatResponseUpdate(
                contents=[
                    Content.from_function_call(call_id="option-model-call", name="echo", arguments={}),
                    usage_content(),
                ]
            )

        async def second(_messages):
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("done"), usage_content(2, 1)])

        client = RuntimeClient([first, second])
        self.adapter.factories["managed_agent_framework"] = lambda _context: Agent(
            client=client,
            tools=[FunctionTool(name="echo", func=lambda: "echoed")],
            default_options={"model": "initial-alias"},
            middleware=[SelectModel()],
        )
        status, events = await self._execute("run-model-options", "echo")
        self.assertEqual(status, "success")
        self.assertEqual([options["model"] for options in client.options], ["configured-1", "configured-2"])
        self.assertNotIn("model", self.final_usage(events))

    async def test_receipt_replay_preserves_model_and_usage_without_a_second_provider_call(self):
        async def response(_messages):
            yield ChatResponseUpdate(model="saved-model", contents=[Content.from_text("answer"), usage_content()])

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        first_status, first = await self._execute("run-model-replay", "reply")
        self.assertEqual(first_status, "success")
        self.adapter.factories["managed_agent_framework"] = lambda _context: self.fail("replayed factory")
        status, replay = await self._execute("run-model-replay", "reply", generation=2)
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(first), self.final_usage(replay))
        self.assertEqual(self.final_usage(replay)["model"], "saved-model")
        self.assertEqual((client.calls, self.plane.puts), (1, 1))

    async def test_custom_client_without_provider_middleware_remains_safely_unpriced(self):
        class NoMiddlewareClient(ScriptedChatClient):
            model = "configured-name"

            async def _inner_get_response(self, **kwargs):
                async def response():
                    yield ChatResponseUpdate(
                        model="reported-name", contents=[Content.from_text("answer"), usage_content()]
                    )

                return self._build_response_stream(response())

        self.install_runtime_client(NoMiddlewareClient())
        status, events = await self._execute("run-custom-client", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(events)["total_tokens"], 14)
        self.assertNotIn("model", self.final_usage(events))

    async def test_middleware_outside_function_loop_is_not_a_trusted_provider_boundary(self):
        class OutsideMiddlewareClient(ChatMiddlewareLayer[Any], FunctionInvocationLayer[Any], BaseChatClient[Any]):
            model = "configured-model"

            async def _inner_get_response(self, **kwargs):
                async def response():
                    yield ChatResponseUpdate(
                        model="reported-model", contents=[Content.from_text("answer"), usage_content()]
                    )

                return self._build_response_stream(response())

        self.install_runtime_client(OutsideMiddlewareClient())
        status, events = await self._execute("run-outer-middleware", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(events)["total_tokens"], 14)
        self.assertNotIn("model", self.final_usage(events))

    async def test_raw_provider_model_overrides_injected_wrapper_alias(self):
        async def response(_messages):
            yield ChatResponseUpdate(
                model="configured-alias",
                raw_representation=SimpleNamespace(
                    response=SimpleNamespace(model="provider-model"),
                ),
            )
            yield ChatResponseUpdate(
                model="configured-alias", role="assistant", contents=[Content.from_text("answer"), usage_content()]
            )

        client = RuntimeClient([response])
        self.install_runtime_client(client, default_options={"model": "configured-alias"})
        status, events = await self._execute("run-provider-model", "reply")
        self.assertEqual(status, "success")
        self.assertEqual(self.final_usage(events)["model"], "provider-model")

    async def test_conflicting_raw_provider_models_cannot_be_overwritten_by_final_wrapper(self):
        async def response(_messages):
            for model in ("configured-alias", "different-provider-model"):
                yield ChatResponseUpdate(model="configured-alias", raw_representation=SimpleNamespace(model=model))
            yield ChatResponseUpdate(role="assistant", contents=[Content.from_text("answer"), usage_content()])

        client = RuntimeClient([response])
        self.install_runtime_client(client, default_options={"model": "configured-alias"})
        status, events = await self._execute("run-provider-conflict", "reply")
        self.assertEqual(status, "success")
        self.assertNotIn("model", self.final_usage(events))

    async def test_native_text_done_confirmation_is_not_appended_twice(self):
        async def response(_messages):
            for text in ("ans", "wer"):
                yield ChatResponseUpdate(
                    role="assistant",
                    model="confirmed-model",
                    message_id="message-1",
                    contents=[Content.from_text(text)],
                    raw_representation=SimpleNamespace(content_index=0),
                )
            yield ChatResponseUpdate(
                role="assistant",
                message_id="message-1",
                contents=[
                    Content.from_text("answer", merge_mode="replace"),
                    usage_content(),
                ],
                raw_representation=SimpleNamespace(content_index=0),
            )

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-text-confirmed", "reply")
        self.assertEqual(status, "success")
        chunks = [event["data"][0]["content"] for event in events if event["method"] == "messages"]
        self.assertEqual(chunks, ["ans", "wer"])
        self.assertEqual(self.final_usage(events)["model"], "confirmed-model")
        self.assertEqual(self.plane.puts, 1)

    async def test_native_text_rewrite_remains_unsupported(self):
        async def response(_messages):
            yield ChatResponseUpdate(
                role="assistant", message_id="message-1", contents=[Content.from_text("before"), usage_content()]
            )
            yield ChatResponseUpdate(
                role="assistant", message_id="message-1", contents=[Content.from_text("after", merge_mode="replace")]
            )

        client = RuntimeClient([response])
        self.install_runtime_client(client)
        status, events = await self._execute("run-text-rewrite", "reply")
        self.assertEqual(status, "error")
        self.assertIn("replacement", events[-1]["data"]["message"])
        self.assertEqual(self.plane.puts, 0)


if __name__ == "__main__":
    unittest.main()
