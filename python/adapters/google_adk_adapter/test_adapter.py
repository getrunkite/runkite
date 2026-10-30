"""Managed turns exercised through the actual offline Google ADK model loop."""

from __future__ import annotations

import asyncio
import copy
import json
import sys
import unittest
from dataclasses import FrozenInstanceError
from pathlib import Path
from typing import Any
from unittest.mock import AsyncMock, Mock, patch

import httpx
from google.adk.agents import LlmAgent, SequentialAgent
from google.adk.models import BaseLlm, LlmResponse
from google.adk.tools import FunctionTool
from google.adk.tools.tool_context import ToolContext
from google.genai import types
from pydantic import Field

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "adapters"))

from runkite_runner.completion import CompletionDeliveryError  # noqa: E402
from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402

from google_adk_adapter import GoogleAdkAdapter  # noqa: E402
from google_adk_adapter import adapter as managed  # noqa: E402
from google_adk_adapter.state import FRAMEWORK  # noqa: E402


class Plane:
    def __init__(self):
        self.blobs, self.versions, self.puts = {}, {}, 0
        self.fail_get, self.fail_put = False, False

    def transport(self):
        def handler(request):
            key = (request.headers["X-Runkite-Tenant-Id"], request.url.path)
            if request.method == "GET":
                if self.fail_get:
                    return httpx.Response(503)
                if key not in self.blobs:
                    return httpx.Response(404)
                return httpx.Response(
                    200,
                    content=self.blobs[key],
                    headers={
                        "ETag": f'"{self.versions[key]}"',
                        "X-Runkite-Checkpoint-Framework": FRAMEWORK,
                    },
                )
            self.puts += 1
            if self.fail_put:
                return httpx.Response(503)
            if request.headers.get("If-None-Match") == "*" and key in self.blobs:
                return httpx.Response(412)
            if request.headers.get("If-Match") and request.headers["If-Match"] != f'"{self.versions.get(key)}"':
                return httpx.Response(412)
            self.blobs[key] = request.content
            self.versions[key] = self.versions.get(key, 0) + 1
            return httpx.Response(204, headers={"ETag": f'"{self.versions[key]}"'})

        return httpx.MockTransport(handler)


class NativeModel(BaseLlm):
    steps: Any
    calls: list[Any] = Field(default_factory=list)
    closed_streams: int = 0

    async def generate_content_async(self, llm_request, stream=False):
        step = self.steps[len(self.calls)]
        self.calls.append(copy.deepcopy(llm_request))
        try:
            async for value in step(llm_request):
                yield value
        finally:
            self.closed_streams += 1


def response(text="answer", *, prompt=10, completion=2, thoughts=0, tools=0, model="offline-adk", **kwargs):
    return LlmResponse(
        model_version=model,
        content=types.Content(role="model", parts=[types.Part(text=text)]),
        usage_metadata=types.GenerateContentResponseUsageMetadata(
            prompt_token_count=prompt,
            candidates_token_count=completion,
            thoughts_token_count=thoughts,
            tool_use_prompt_token_count=tools,
            total_token_count=prompt + completion + thoughts + tools,
        ),
        partial=False,
        turn_complete=True,
        **kwargs,
    )


async def text(request):
    yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text="an")]), partial=True)
    yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text="swer")]), partial=True)
    yield response()


async def tool_call(request):
    value = response()
    value.content.parts = [
        types.Part(function_call=types.FunctionCall(id="call-1", name="effect", args={"value": "hello"}))
    ]
    yield value


class GoogleAdkAdapterTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.plane, self.stores = Plane(), []
        self.adapter = GoogleAdkAdapter()
        self.adapter.configure(http_address="http://plane", runner_token="token")
        original = self.adapter._state

        def state():
            store = original()
            store._client._transport = self.plane.transport()
            self.stores.append(store)
            return store

        self.adapter._state = state
        self.model = NativeModel(model="offline-adk", steps=[text])
        self.adapter.factories["agent"] = lambda context: LlmAgent(name="test_agent", model=self.model)
        logging = patch.object(managed.logger, "exception")
        logging.start()
        self.addCleanup(logging.stop)

    async def asyncTearDown(self):
        for store in self.stores:
            self.assertIsNone(store._client._client)

    async def execute(
        self,
        run_id="run-1",
        *,
        generation=1,
        cancel=None,
        extra=None,
        send_hook=None,
        tenant="default",
        thread="thread-1",
    ):
        events = []

        async def send(event):
            events.append(event)
            if send_hook:
                await send_hook(event)

        bound_tenant, bound_run = bind_tenant(tenant), bind_run(run_id, generation)
        try:
            status = await self.adapter.execute(
                {
                    "run_id": run_id,
                    "thread_id": thread,
                    "graph_id": "agent",
                    "tenant_id": tenant,
                    "generation": generation,
                    "input": {"messages": [{"role": "user", "content": "question"}]},
                    **(extra or {}),
                },
                send,
                cancel if cancel is not None else asyncio.Event(),
            )
        finally:
            reset_run(bound_run)
            reset_tenant(bound_tenant)
        return status, events

    def output(self, events):
        return [event["data"] for event in events if event["method"] == "values"][-1]

    def saved(self):
        return json.loads(next(iter(self.plane.blobs.values())))

    def tool_agent(self, function, *, confirmation=False):
        self.model = NativeModel(model="offline-adk", steps=[tool_call, text])
        self.adapter.factories["agent"] = lambda context: LlmAgent(
            name="test_agent", model=self.model, tools=[FunctionTool(function, require_confirmation=confirmation)]
        )

    async def test_partial_text_repeats_final_aggregate_once_after_durable_save(self):
        async def send(event):
            if event["method"] == "values" and "messages" in event["data"]:
                self.assertEqual(self.plane.puts, 1)

        status, events = await self.execute(send_hook=send)
        self.assertEqual(status, "success", events)
        self.assertEqual(
            "".join(event["data"][0]["content"] for event in events if event["method"] == "messages"), "answer"
        )
        self.assertEqual(
            self.output(events)["usage"],
            {"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12, "model": "offline-adk"},
        )
        self.assertEqual(self.model.closed_streams, 1)
        self.assertEqual(self.saved()["completed"]["text_events"], ["an", "swer"])

    async def test_usage_is_delivered_before_allowlisted_tool_and_counts_thoughts(self):
        delivered, effects = [], []

        async def first(request):
            value = response(thoughts=3, tools=2)
            value.content.parts = [
                types.Part(function_call=types.FunctionCall(id="call-1", name="effect", args={"value": "hello"}))
            ]
            yield value

        def effect(value: str):
            effects.append(value)
            self.assertEqual(delivered[-1]["usage"]["completion_tokens"], 5)
            self.assertEqual(delivered[-1]["usage"]["prompt_tokens"], 12)
            return {"ok": True}

        self.tool_agent(effect)
        self.model.steps = [first, text]

        async def send(event):
            if event["method"] == "values":
                delivered.append(event["data"])

        status, events = await self.execute(send_hook=send, extra={"allowed_tools": ["effect"]})
        self.assertEqual(status, "success", events)
        self.assertEqual(effects, ["hello"])
        self.assertEqual(self.output(events)["usage"]["total_tokens"], 29)

    async def test_empty_and_nonmatching_allowlists_deny_before_body(self):
        effects = []

        def effect(value: str):
            effects.append(value)
            return {"ok": True}

        for allowed in ([], ["different"]):
            self.tool_agent(effect)
            status, events = await self.execute(extra={"allowed_tools": allowed})
            self.assertEqual(status, "error", events)
            self.assertEqual(effects, [])
            self.assertEqual(self.plane.puts, 0)
            self.assertTrue(
                any(
                    event["method"] == "tool_auth" and event["data"]["reason_code"] == "tool_not_allowed"
                    for event in events
                )
            )
            self.assertEqual(self.output(events)["usage"]["total_tokens"], 12)

    async def test_native_confirmation_checks_real_args_without_body_or_commit(self):
        effects, proposals = [], []

        def effect(value: str):
            effects.append(value)
            return {"ok": True}

        def confirmation(value: str):
            proposals.append(value)
            return True

        self.tool_agent(effect, confirmation=confirmation)
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(effects, [])
        self.assertEqual(proposals, ["hello"])
        self.assertEqual(len(self.model.calls), 1)
        self.assertEqual(self.plane.puts, 0)

    async def test_receipt_replay_before_factory_and_retags_generation(self):
        self.assertEqual((await self.execute())[0], "success")
        self.adapter.factories["agent"] = lambda context: self.fail("receipt replay called factory")
        status, events = await self.execute(generation=2)
        self.assertEqual(status, "success", events)
        self.assertEqual(self.output(events)["google_adk"]["generation"], 2)
        self.assertEqual(self.output(events)["usage"]["total_tokens"], 12)
        self.assertEqual(self.plane.puts, 1)

    async def test_successful_native_state_and_history_continue_after_worker_restart(self):
        self.assertEqual((await self.execute())[0], "success")
        self.model = NativeModel(model="offline-adk", steps=[text])
        status, events = await self.execute("run-2")
        self.assertEqual(status, "success", events)
        self.assertEqual(len(self.model.calls[0].contents), 3)
        self.assertEqual(self.output(events)["usage"]["total_tokens"], 12)
        self.assertEqual(len(self.output(events)["messages"]), 4)

    async def test_state_load_failure_never_runs_factory_and_closes_client(self):
        self.plane.fail_get = True
        self.adapter.factories["agent"] = lambda context: self.fail("factory ran after load failed")
        with self.assertRaises(CompletionDeliveryError):
            await self.execute()

    async def test_failed_commit_withholds_terminal_and_recoverable_cas_conflict(self):
        self.plane.fail_put = True
        with self.assertRaises(CompletionDeliveryError):
            await self.execute()
        self.assertEqual(self.model.closed_streams, 1)

    async def test_presets_cancel_before_state_factory_or_model(self):
        cancel = asyncio.Event()
        cancel.set()
        self.adapter.factories["agent"] = lambda context: self.fail("factory ran canceled")
        status, events = await self.execute(cancel=cancel)
        self.assertEqual(status, "interrupted", events)
        self.assertEqual(self.stores, [])

    async def test_cancel_in_observed_usage_prevents_tool_and_retains_known_tokens(self):
        cancel, effects = asyncio.Event(), []

        def effect(value: str):
            effects.append(value)

        self.tool_agent(effect)

        async def send(event):
            if event["method"] == "values":
                cancel.set()

        status, events = await self.execute(cancel=cancel, send_hook=send)
        self.assertEqual(status, "interrupted", events)
        self.assertEqual(effects, [])
        self.assertEqual(self.output(events)["usage"]["model"], "offline-adk")
        self.assertEqual(self.plane.puts, 0)

    async def test_error_after_charged_tool_preserves_prior_checkpoint_and_usage(self):
        self.assertEqual((await self.execute())[0], "success")
        prior = dict(self.plane.blobs)

        async def failure(request):
            raise RuntimeError("provider failed")
            yield

        def effect(value: str):
            return {"ok": True}

        self.tool_agent(effect)
        self.model.steps = [tool_call, failure]
        status, events = await self.execute("run-2")
        self.assertEqual(status, "error", events)
        self.assertEqual(self.plane.blobs, prior)
        self.assertEqual(self.output(events)["usage"]["total_tokens"], 12)
        self.assertEqual(self.output(events)["usage"]["model"], "offline-adk")

    async def test_nonfinal_errors_refusal_or_incomplete_response_never_success(self):
        for kwargs in (
            {"error_code": "blocked"},
            {"finish_reason": types.FinishReason.MAX_TOKENS},
            {"interrupted": True},
        ):

            async def invalid(request, kwargs=kwargs):
                yield response(**kwargs)

            self.model = NativeModel(model="offline-adk", steps=[invalid])
            status, events = await self.execute()
            self.assertEqual(status, "error", events)
            self.assertEqual(self.plane.puts, 0)

    async def test_explicit_unfinished_native_turn_is_not_a_successful_final_response(self):
        async def unfinished(request):
            value = response()
            value.turn_complete = False
            yield value

        self.model.steps = [unfinished]
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(self.plane.puts, 0)

    async def test_multimodal_static_instruction_rejects_before_model(self):
        self.adapter.factories["agent"] = lambda context: LlmAgent(
            name="agent",
            model=self.model,
            static_instruction=types.Part(inline_data=types.Blob(data=b"synthetic", mime_type="image/png")),
        )
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(self.model.calls, [])

    async def test_invalid_roles_batches_resume_and_binary_reject_before_factory(self):
        self.adapter.factories["agent"] = lambda context: self.fail("factory called for invalidinput")
        cases = [
            {"input": {"messages": [{"role": "system", "content": "instruction"}]}},
            {"input": {"messages": [{"role": "user", "content": "one"}, {"role": "user", "content": "two"}]}},
            {"input": {"messages": [{"role": "user", "content": [{"type": "image", "url": "fake"}]}]}},
            {"resume_command": {}},
        ]
        for extra in cases:
            self.assertEqual((await self.execute(extra=extra))[0], "error")
        self.assertEqual(self.stores, [])

    async def test_frozen_factory_context_is_run_and_tenant_specific(self):
        contexts = []

        def factory(context):
            contexts.append(context)
            with self.assertRaises(FrozenInstanceError):
                context.run_id = "changed"
            return LlmAgent(name="agent", model=NativeModel(model="offline-adk", steps=[text]))

        self.adapter.factories["agent"] = factory
        await asyncio.gather(self.execute("run-a", tenant="a"), self.execute("run-b", tenant="b"))
        self.assertEqual({(ctx.run_id, ctx.tenant_id) for ctx in contexts}, {("run-a", "a"), ("run-b", "b")})
        self.assertEqual(len(self.plane.blobs), 2)

    async def test_workflows_native_callbacks_and_streaming_tools_reject_before_model(self):
        factories = [
            lambda context: SequentialAgent(name="workflow", sub_agents=[LlmAgent(name="child", model=self.model)]),
            lambda context: LlmAgent(name="agent", model=self.model, before_model_callback=lambda **kwargs: None),
        ]

        async def streaming(value: str):
            yield "unsupported"

        factories.append(lambda context: LlmAgent(name="agent", model=self.model, tools=[streaming]))
        for factory in factories:
            self.adapter.factories["agent"] = factory
            self.assertEqual((await self.execute())[0], "error")
        self.assertEqual(self.model.calls, [])

    async def test_cumulative_stream_usage_is_replaced_not_summed(self):
        async def cumulative(request):
            value = response("an", prompt=5, completion=1)
            value.partial = True
            yield value
            yield response("answer", prompt=10, completion=2)

        self.model.steps = [cumulative]
        status, events = await self.execute()
        self.assertEqual(status, "success", events)
        self.assertEqual(self.output(events)["usage"]["total_tokens"], 12)

    async def test_same_model_mixed_model_and_unknown_request_attribution(self):
        def effect(value: str):
            return {"ok": True}

        for identity in ("offline-adk", "another-model", None):

            async def final(request, identity=identity):
                yield response(model=identity)

            self.tool_agent(effect)
            self.model.steps = [tool_call, final]
            status, events = await self.execute(run_id=f"run-{identity}", thread=f"thread-{identity}")
            self.assertEqual(status, "success", events)
            self.assertEqual(self.output(events)["usage"]["total_tokens"], 24)
            self.assertEqual(
                self.output(events)["usage"].get("model"), "offline-adk" if identity == "offline-adk" else None
            )

    async def test_missing_usage_stays_unmetered_without_fabricated_model(self):
        async def missing(request):
            yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text="answer")]))

        self.model.steps = [missing]
        status, events = await self.execute()
        self.assertEqual(status, "success", events)
        self.assertTrue(self.output(events)["usage"]["unmetered"])
        self.assertNotIn("model", self.output(events)["usage"])

    async def test_partial_only_and_text_rewrite_fail_without_checkpoint(self):
        async def partial(request):
            yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text="before")]), partial=True)

        async def rewrite(request):
            async for value in partial(request):
                yield value
            yield response("different")

        for step in (partial, rewrite):
            self.model = NativeModel(model="offline-adk", steps=[step])
            self.assertEqual((await self.execute())[0], "error")
            self.assertEqual(self.plane.puts, 0)

    async def test_cancel_stalled_second_request_closes_native_generators(self):
        cancel, stalled = asyncio.Event(), asyncio.Event()

        def effect(value: str):
            return {"ok": True}

        async def stall(request):
            stalled.set()
            yield LlmResponse(content=types.Content(role="model", parts=[types.Part(text="waiting")]), partial=True)
            await asyncio.Event().wait()

        self.tool_agent(effect)
        self.model.steps = [tool_call, stall]
        task = asyncio.create_task(self.execute(cancel=cancel))
        await asyncio.wait_for(stalled.wait(), 2)
        cancel.set()
        status, events = await asyncio.wait_for(task, 2)
        self.assertEqual(status, "interrupted", events)
        self.assertEqual(self.output(events)["usage"]["model"], "offline-adk")
        self.assertEqual(self.model.closed_streams, 2)
        self.assertEqual(self.plane.puts, 0)

    async def test_partial_function_arguments_never_execute_before_completed_proposal(self):
        effects = []

        def effect(value: str):
            effects.append(value)
            return {"ok": True}

        async def fragments(request):
            value = response()
            value.partial = True
            value.content.parts = [
                types.Part(function_call=types.FunctionCall(name="effect", args={"value": "incomplete"}))
            ]
            yield value

        self.tool_agent(effect)
        self.model.steps = [fragments]
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(effects, [])
        self.assertEqual(self.plane.puts, 0)

    async def test_delivery_error_wrapped_by_native_plugin_withholds_terminal(self):
        async def send(event):
            if event["method"] == "messages":
                raise CompletionDeliveryError("synthetic stream delivery failure")

        with self.assertRaisesRegex(CompletionDeliveryError, "synthetic stream delivery"):
            await self.execute(send_hook=send)
        self.assertEqual(self.plane.puts, 0)
        self.assertEqual(self.model.closed_streams, 1)

    async def test_tool_session_state_commits_only_successful_turns_and_scopes_reject(self):
        def effect(value: str, tool_context: ToolContext):
            tool_context.state["counter"] = tool_context.state.get("counter", 0) + 1
            tool_context.state["temp:ephemeral"] = "not durable"
            return {"ok": True}

        self.tool_agent(effect)
        status, events = await self.execute()
        self.assertEqual(status, "success", events)
        self.assertEqual(self.saved()["history"]["state"], {"counter": 1})
        prior = dict(self.plane.blobs)

        def scoped(value: str, tool_context: ToolContext):
            tool_context.state["app:shared"] = "unsupported"
            return {"ok": True}

        self.tool_agent(scoped)
        # Keep native tool name matching the model's ordinary proposal.
        self.adapter.factories["agent"] = lambda context: LlmAgent(
            name="test_agent", model=self.model, tools=[FunctionTool(scoped)]
        )

        async def scoped_call(request):
            value = response()
            value.content.parts = [types.Part(function_call=types.FunctionCall(name="scoped", args={"value": "hello"}))]
            yield value

        self.model.steps = [scoped_call, text]
        self.assertEqual((await self.execute("run-2"))[0], "error")
        self.assertEqual(self.plane.blobs, prior)

    async def test_giant_native_delta_rejected_before_messages_wire_and_commit(self):
        async def giant(request):
            yield response("x" * (3 << 20))

        self.model.steps = [giant]
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertFalse(any(event["method"] == "messages" for event in events))
        self.assertEqual(self.plane.puts, 0)

    async def test_giant_tool_args_rejected_before_body(self):
        effects = []

        def effect(value: str):
            effects.append(value)
            return {"ok": True}

        async def giant(request):
            value = response()
            value.content.parts = [
                types.Part(function_call=types.FunctionCall(name="effect", args={"value": "x" * (3 << 20)}))
            ]
            yield value

        self.tool_agent(effect)
        self.model.steps = [giant, text]
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(effects, [])
        self.assertEqual(self.plane.puts, 0)

    async def test_giant_tool_result_rejected_before_next_model(self):
        def effect(value: str):
            return {"result": "x" * (3 << 20)}

        self.tool_agent(effect)
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(len(self.model.calls), 1)
        self.assertEqual(self.plane.puts, 0)

    async def test_text_receipt_cap_rejects_before_success(self):
        with patch.object(managed, "MAX_RECEIPT_BYTES", 5):
            status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(self.plane.puts, 0)

    async def test_native_binary_response_rejected_with_observed_usage(self):
        async def binary(request):
            value = response()
            value.content.parts = [types.Part(inline_data=types.Blob(data=b"synthetic", mime_type="image/png"))]
            yield value

        self.model.steps = [binary]
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(self.output(events)["usage"]["total_tokens"], 12)
        self.assertEqual(self.plane.puts, 0)

    async def test_cleanup_failure_does_not_rewrite_committed_success(self):
        from google.adk import Runner

        original = Runner.close

        async def failed_close(runner):
            await original(runner)
            raise RuntimeError("synthetic cleanup failure")

        with patch.object(Runner, "close", failed_close):
            status, events = await self.execute()
        self.assertEqual(status, "success", events)
        self.assertEqual(self.plane.puts, 1)

    async def test_explicit_native_client_closes_even_on_preflight_rejection(self):
        from google import genai
        from google.adk.models.google_llm import Gemini

        client = genai.Client(api_key="synthetic-unused")
        model = Gemini(model="synthetic", client=client)
        self.adapter.factories["agent"] = lambda context: LlmAgent(
            name="agent",
            model=model,
            before_model_callback=lambda **kwargs: None,
        )
        with (
            patch.object(client.aio, "aclose", new_callable=AsyncMock) as async_close,
            patch.object(client, "close", new_callable=Mock) as close,
        ):
            status, events = await self.execute()
        self.assertEqual(status, "error", events)
        async_close.assert_awaited_once()
        close.assert_called_once()
        await client.aio.aclose()
        client.close()

    async def test_malformed_native_usage_total_rejects_before_tool_effect(self):
        effects = []

        def effect(value: str):
            effects.append(value)
            return {"ok": True}

        async def malformed(request):
            async for value in tool_call(request):
                value.usage_metadata.total_token_count = 999
                yield value

        self.tool_agent(effect)
        self.model.steps = [malformed, text]
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(effects, [])
        self.assertEqual(self.plane.puts, 0)

    async def test_nonpartial_incomplete_function_arguments_reject_before_body(self):
        effects = []

        def effect(value: str):
            effects.append(value)
            return {"ok": True}

        async def incomplete(request):
            async for value in tool_call(request):
                value.content.parts[0].function_call.will_continue = True
                yield value

        self.tool_agent(effect)
        self.model.steps = [incomplete, text]
        status, events = await self.execute()
        self.assertEqual(status, "error", events)
        self.assertEqual(effects, [])
        self.assertEqual(self.plane.puts, 0)

    async def test_response_observed_after_cancel_retains_usage_without_text_or_effects(self):
        cancel = asyncio.Event()

        async def completed(request):
            cancel.set()
            yield response()

        self.model.steps = [completed]
        status, events = await self.execute(cancel=cancel)
        self.assertEqual(status, "interrupted", events)
        self.assertEqual(
            self.output(events)["usage"],
            {
                "prompt_tokens": 10,
                "completion_tokens": 2,
                "total_tokens": 12,
                "model": "offline-adk",
            },
        )
        self.assertFalse(any(event["method"] == "messages" for event in events))
        self.assertEqual(self.plane.puts, 0)


if __name__ == "__main__":
    unittest.main()
