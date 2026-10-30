"""Offline tests through the pinned native SDK's real streaming agent loop."""

from __future__ import annotations

import asyncio
import copy
import json
import sys
import unittest
from dataclasses import FrozenInstanceError, dataclass
from pathlib import Path
from unittest.mock import patch

import httpx
from agents import Agent, GuardrailFunctionOutput, Model, WebSearchTool, function_tool, output_guardrail
from agents.mcp import MCPServerStdio
from agents.models.interface import ModelTracing
from agents.tool import ToolOutputImage, ToolOutputText
from openai.types.responses import (
    Response,
    ResponseCompletedEvent,
    ResponseFunctionToolCall,
    ResponseOutputItemDoneEvent,
    ResponseOutputMessage,
    ResponseOutputRefusal,
    ResponseOutputText,
    ResponseReasoningItem,
    ResponseRefusalDeltaEvent,
    ResponseTextDeltaEvent,
    ResponseUsage,
)
from openai.types.responses.response_usage import InputTokensDetails, OutputTokensDetails

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "adapters"))

from runkite_runner.completion import CompletionDeliveryError  # noqa: E402
from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402

from openai_agents_adapter import adapter as managed  # noqa: E402
from openai_agents_adapter.adapter import OpenAIAgentsAdapter  # noqa: E402
from openai_agents_adapter.state import FRAMEWORK  # noqa: E402


def message(text="hello", *, identity="answer", status="completed", refusal=False):
    part = (
        ResponseOutputRefusal(type="refusal", refusal=text)
        if refusal
        else ResponseOutputText(type="output_text", text=text, annotations=[], logprobs=[])
    )
    return ResponseOutputMessage(id=identity, type="message", role="assistant", status=status, content=[part])


def tool_call(name="lookup", *, identity="call-1", arguments='{"query":"Runkite"}'):
    return ResponseFunctionToolCall(
        id=identity + "-item",
        call_id=identity,
        name=name,
        arguments=arguments,
        type="function_call",
        status="completed",
    )


@dataclass
class Step:
    output: list | None = None
    deltas: list[str] | None = None
    tokens: tuple[int, int] | None = (10, 2)
    entered: asyncio.Event | None = None
    release: asyncio.Event | None = None
    error: Exception | None = None
    model: str | None = "offline-model"
    response_id: str | None = None


class OfflineModel(Model):
    def __init__(self, steps):
        self.steps = steps
        self.calls = []
        self.closed = 0
        self.streams_closed = 0
        self.close_error = False

    async def get_response(self, *args, **kwargs):
        raise AssertionError("The managed adapter must use native streaming")

    async def close(self):
        self.closed += 1
        if self.close_error:
            raise RuntimeError("close failed")

    async def stream_response(
        self, system_instructions, input, model_settings, tools, output_schema, handoffs, tracing, **kwargs
    ):
        step = self.steps[len(self.calls)]
        self.calls.append({"input": copy.deepcopy(input), "tracing": tracing, "context": kwargs})
        if step.entered:
            step.entered.set()
        try:
            if step.release:
                await step.release.wait()
            if step.error:
                raise step.error
            output = step.output or []
            sequence = 0
            for item in output:
                if isinstance(item, ResponseOutputMessage):
                    for content in item.content:
                        deltas = (
                            step.deltas
                            if step.deltas is not None
                            else [content.refusal if content.type == "refusal" else content.text]
                        )
                        for delta in deltas:
                            values = dict(
                                item_id=item.id, output_index=0, content_index=0, delta=delta, sequence_number=sequence
                            )
                            if content.type == "refusal":
                                yield ResponseRefusalDeltaEvent(type="response.refusal.delta", **values)
                            else:
                                yield ResponseTextDeltaEvent(type="response.output_text.delta", logprobs=[], **values)
                            sequence += 1
            for index, item in enumerate(output):
                yield ResponseOutputItemDoneEvent(
                    type="response.output_item.done", item=item, output_index=index, sequence_number=sequence
                )
                sequence += 1
            usage = None
            if step.tokens is not None:
                prompt, completion = step.tokens
                usage = ResponseUsage(
                    input_tokens=prompt,
                    output_tokens=completion,
                    total_tokens=prompt + completion,
                    input_tokens_details=InputTokensDetails(cached_tokens=0, cache_write_tokens=0),
                    output_tokens_details=OutputTokensDetails(reasoning_tokens=0),
                )
            response = Response(
                id=step.response_id or f"response-{len(self.calls)}",
                object="response",
                created_at=0,
                model=step.model or "",
                output=output,
                tool_choice="auto",
                tools=[],
                parallel_tool_calls=False,
                usage=usage,
            )
            if step.model is None:
                response = Response.model_construct(**response.model_dump(exclude={"model"}))
            yield ResponseCompletedEvent(type="response.completed", sequence_number=sequence, response=response)
        finally:
            self.streams_closed += 1


class FakePlane:
    def __init__(self):
        self.blobs = {}
        self.versions = {}
        self.puts = 0
        self.reject_put = False
        self.reject_get = False
        self.requests = []

    def transport(self):
        def respond(request):
            self.requests.append(request)
            key = (request.headers["X-Runkite-Tenant-Id"], request.url.path)
            if request.method == "GET":
                if self.reject_get:
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
            if request.method == "PUT":
                self.puts += 1
                if self.reject_put:
                    return httpx.Response(503)
                if request.headers.get("If-None-Match") == "*" and key in self.blobs:
                    return httpx.Response(412)
                if "If-Match" in request.headers and request.headers["If-Match"] != f'"{self.versions.get(key)}"':
                    return httpx.Response(412)
                self.versions[key] = self.versions.get(key, 0) + 1
                self.blobs[key] = request.content
                return httpx.Response(204, headers={"ETag": f'"{self.versions[key]}"'})
            return httpx.Response(405)

        return httpx.MockTransport(respond)


class OpenAIAgentsAdapterTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.log_patch = patch.object(managed.logger, "exception")
        self.log_patch.start()
        self.addCleanup(self.log_patch.stop)
        self.plane = FakePlane()
        self.adapter = OpenAIAgentsAdapter()
        self.adapter.configure(http_address="http://plane", runner_token="runner-secret")
        self.stores = []
        state = self.adapter._state

        def make_state():
            store = state()
            store._client._transport = self.plane.transport()
            self.stores.append(store)
            return store

        self.adapter._state = make_state
        self.model = OfflineModel([Step([message()])])
        self.adapter.factories["agent"] = lambda context: Agent(name="offline", model=self.model)

    async def asyncTearDown(self):
        for state in self.stores:
            self.assertIsNone(state._client._client, "all state HTTP clients must close")

    async def execute(
        self,
        run="run-1",
        *,
        text="question",
        messages=None,
        tenant="default",
        thread="thread-1",
        generation=1,
        cancel=None,
        extra=None,
        send_hook=None,
    ):
        events = []

        async def send(event):
            events.append(event)
            if send_hook:
                await send_hook(event)

        tenant_binding = bind_tenant(tenant)
        run_binding = bind_run(run, generation)
        try:
            status = await self.adapter.execute(
                {
                    "run_id": run,
                    "thread_id": thread,
                    "graph_id": "agent",
                    "tenant_id": tenant,
                    "generation": generation,
                    "input": {"messages": messages if messages is not None else [{"role": "user", "content": text}]},
                    **(extra or {}),
                },
                send,
                cancel if cancel is not None else asyncio.Event(),
            )
        finally:
            reset_run(run_binding)
            reset_tenant(tenant_binding)
        return status, events

    def values(self, events):
        return [event["data"] for event in events if event["method"] == "values"][-1]

    def error(self, events):
        return [event["data"]["message"] for event in events if event["method"] == "error"][-1]

    def saved(self):
        return json.loads(next(iter(self.plane.blobs.values())))

    async def test_text_success_commits_before_final_values_and_closes_native_resources(self):
        async def before_final(event):
            if event["method"] == "values" and "messages" in event["data"]:
                self.assertEqual(self.plane.puts, 1)

        status, events = await self.execute(send_hook=before_final)
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["messages"][-1], {"role": "assistant", "content": "hello"})
        self.assertEqual(
            self.values(events)["usage"],
            {"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12, "model": "offline-model"},
        )
        self.assertEqual(self.model.calls[0]["tracing"], ModelTracing.DISABLED)
        self.assertEqual((self.model.closed, self.model.streams_closed), (1, 1))
        self.assertEqual(self.saved()["completed"]["text_events"], ["hello"])
        self.assertEqual(self.plane.requests[0].headers["X-Runner-Token"], "runner-secret")

    async def test_native_tool_body_observes_published_usage_before_execution(self):
        delivered = []
        body = []

        @function_tool
        async def lookup(query: str) -> str:
            self.assertEqual(delivered[-1]["prompt_tokens"], 10)
            self.assertEqual(delivered[-1]["model"], "offline-model")
            body.append(query)
            return "fact"

        self.model = OfflineModel([Step([tool_call()]), Step([message("answer")])])
        self.adapter.factories["agent"] = lambda context: Agent(name="tools", model=self.model, tools=[lookup])

        async def send(event):
            if event["method"] == "values" and "usage" in event["data"]:
                delivered.append(event["data"]["usage"])

        status, events = await self.execute(send_hook=send)
        self.assertEqual(status, "success")
        self.assertEqual(body, ["Runkite"])
        self.assertEqual(self.values(events)["usage"]["prompt_tokens"], 20)
        self.assertEqual(self.values(events)["usage"]["completion_tokens"], 4)
        self.assertEqual(
            [item["type"] for item in self.saved()["history"][1:]], ["function_call", "function_call_output", "message"]
        )

    async def test_preamble_tool_and_final_text_preserve_all_native_assistant_messages(self):
        @function_tool
        async def lookup(query: str) -> str:
            return "fact"

        self.model = OfflineModel(
            [Step([message("Checking. ", identity="preamble"), tool_call()]), Step([message("Answer.")])]
        )
        self.adapter.factories["agent"] = lambda context: Agent(name="preamble", model=self.model, tools=[lookup])
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["messages"][-1]["content"], "Checking. Answer.")
        self.assertEqual(self.saved()["completed"]["text_events"], ["Checking. ", "Answer."])

    async def test_next_turn_loads_native_history_and_passes_every_new_text_message(self):
        self.assertEqual((await self.execute())[0], "success")
        prior = self.saved()["history"]
        self.model = OfflineModel([Step([message("second", identity="second")])])
        incoming = [
            {"role": "system", "content": "new rules"},
            {"role": "human", "content": "one"},
            {"role": "user", "content": [{"type": "text", "text": "two"}]},
        ]
        status, events = await self.execute("run-2", messages=incoming)
        self.assertEqual(status, "success")
        self.assertEqual(
            self.model.calls[0]["input"],
            prior
            + [
                {"role": "system", "content": "new rules"},
                {"role": "user", "content": "one"},
                {"role": "user", "content": "two"},
            ],
        )
        self.assertEqual(len(self.values(events)["messages"]), 6)

    async def test_receipt_replay_skips_factory_and_retags_current_generation(self):
        self.assertEqual((await self.execute())[0], "success")

        def must_not_run(context):
            raise AssertionError("committed replay must not construct an agent")

        self.adapter.factories["agent"] = must_not_run
        status, events = await self.execute(generation=2)
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["openai_agents"]["generation"], 2)
        self.assertEqual(self.plane.puts, 1)
        self.assertEqual(len(self.model.calls), 1)

    async def test_receipt_replay_refuses_different_input_before_saved_text(self):
        await self.execute()
        status, events = await self.execute(text="different", generation=2)
        self.assertEqual(status, "error")
        self.assertIn("input differs", self.error(events))
        self.assertFalse(any(event["method"] == "messages" for event in events))

    async def test_missing_provider_usage_is_explicitly_unmetered(self):
        self.model = OfflineModel([Step([message()], tokens=None)])
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertTrue(self.values(events)["usage"]["unmetered"])
        self.assertEqual(self.values(events)["usage"]["model"], "offline-model")

    async def test_model_identity_prefers_provider_report_over_configured_alias(self):
        self.model.model = "configured-alias"
        self.model.steps[0].model = "reported-model"
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["usage"]["model"], "reported-model")
        self.assertEqual(self.saved()["completed"]["output"]["usage"]["model"], "reported-model")

    async def test_explicit_model_public_name_fills_missing_provider_identity(self):
        self.model.model = "configured-model"
        self.model.steps[0].model = ""
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["usage"]["model"], "configured-model")

    async def test_agent_model_string_fills_missing_provider_identity_without_network(self):
        self.model.steps[0].model = ""
        self.adapter.factories["agent"] = lambda context: Agent(name="configured", model="configured-model")
        with patch("agents.models.multi_provider.MultiProvider.get_model", return_value=self.model):
            status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["usage"]["model"], "configured-model")

    async def test_sdk_default_model_learns_reported_identity_and_replay_preserves_it(self):
        self.adapter.factories["agent"] = lambda context: Agent(name="SDK default")
        with patch("agents.models.multi_provider.MultiProvider.get_model", return_value=self.model):
            status, events = await self.execute()
        self.assertEqual(status, "success")
        original = self.values(events)["usage"]
        self.assertEqual(original["model"], "offline-model")
        self.adapter.factories["agent"] = lambda context: self.fail("replay constructed an agent")
        status, replay = await self.execute(generation=2)
        self.assertEqual(status, "success")
        self.assertEqual(self.values(replay)["usage"], original)

    async def test_unknown_model_is_not_invented_for_positive_usage(self):
        self.model.steps[0].model = ""
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["usage"]["total_tokens"], 12)
        self.assertNotIn("model", self.values(events)["usage"])

    async def test_missing_raw_model_metadata_still_settles_without_a_name(self):
        self.model.steps[0].model = None
        status, events = await asyncio.wait_for(self.execute(), 1)
        self.assertEqual(status, "success")
        self.assertNotIn("model", self.values(events)["usage"])

    async def test_mixed_or_partly_unknown_models_leave_aggregate_unpriced(self):
        @function_tool
        async def lookup(query: str) -> str:
            return "fact"

        for index, identities in enumerate((("first-model", "second-model"), ("", "second-model"))):
            with self.subTest(identities=identities):
                self.model = OfflineModel(
                    [Step([tool_call()], model=identities[0]), Step([message()], model=identities[1])]
                )
                self.adapter.factories["agent"] = lambda context: Agent(name="models", model=self.model, tools=[lookup])
                status, events = await self.execute(thread=f"thread-{index}")
                self.assertEqual(status, "success")
                self.assertEqual(self.values(events)["usage"]["prompt_tokens"], 20)
                self.assertNotIn("model", self.values(events)["usage"])

    async def test_repeated_provider_response_ids_keep_each_call_model_identity(self):
        @function_tool
        async def lookup(query: str) -> str:
            return "fact"

        for index, identities in enumerate((("same-model", "same-model"), ("first-model", "second-model"))):
            with self.subTest(identities=identities):
                self.model = OfflineModel(
                    [
                        Step([tool_call()], model=identities[0], response_id="fake-response-id"),
                        Step([message()], model=identities[1], response_id="fake-response-id"),
                    ]
                )
                self.adapter.factories["agent"] = lambda context: Agent(
                    name="same ID", model=self.model, tools=[lookup]
                )
                status, events = await self.execute(thread=f"thread-{index}")
                self.assertEqual(status, "success")
                usage = self.values(events)["usage"]
                self.assertEqual(usage["total_tokens"], 24)
                if identities[0] == identities[1]:
                    self.assertEqual(usage["model"], identities[0])
                else:
                    self.assertNotIn("model", usage)

    async def test_unaligned_metadata_has_bounded_wait_and_does_not_guess_a_model(self):
        with patch.object(managed, "MODEL_METADATA_WAIT_SECONDS", 0):
            status, events = await asyncio.wait_for(self.execute(), 1)
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["usage"]["prompt_tokens"], 10)
        self.assertNotIn("model", self.values(events)["usage"])

    async def test_provider_error_retains_current_usage_and_last_committed_history(self):
        await self.execute()
        prior = dict(self.plane.blobs)

        @function_tool
        async def lookup(query: str) -> str:
            return "fact"

        self.model = OfflineModel([Step([tool_call()], tokens=(10, 4)), Step(error=RuntimeError("provider failed"))])
        self.adapter.factories["agent"] = lambda context: Agent(name="failure", model=self.model, tools=[lookup])
        status, events = await self.execute("run-2")
        self.assertEqual(status, "error")
        self.assertEqual(self.plane.blobs, prior)
        self.assertEqual(
            self.values(events)["usage"],
            {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "offline-model"},
        )
        self.assertEqual((self.model.closed, self.model.streams_closed), (1, 2))

    async def test_cancel_during_second_model_step_preserves_observed_usage_and_closes_stream(self):
        entered, cancel = asyncio.Event(), asyncio.Event()

        @function_tool
        async def lookup(query: str) -> str:
            return "fact"

        self.model = OfflineModel([Step([tool_call()], tokens=(10, 4)), Step(entered=entered, release=asyncio.Event())])
        self.adapter.factories["agent"] = lambda context: Agent(name="cancel", model=self.model, tools=[lookup])
        task = asyncio.create_task(self.execute(cancel=cancel))
        await asyncio.wait_for(entered.wait(), 3)
        cancel.set()
        status, events = await asyncio.wait_for(task, 3)
        self.assertEqual(status, "interrupted")
        self.assertEqual(self.values(events)["usage"]["total_tokens"], 14)
        self.assertFalse(self.plane.blobs)
        self.assertEqual((self.model.closed, self.model.streams_closed), (1, 2))

    async def test_cancel_from_usage_hook_prevents_the_tool_body(self):
        cancel, body = asyncio.Event(), []

        @function_tool
        async def lookup(query: str) -> str:
            body.append(query)
            return "fact"

        self.model = OfflineModel([Step([tool_call()])])
        self.adapter.factories["agent"] = lambda context: Agent(name="cancel", model=self.model, tools=[lookup])

        async def send(event):
            if event["method"] == "values" and event["data"].get("usage", {}).get("prompt_tokens") == 10:
                cancel.set()

        status, events = await self.execute(cancel=cancel, send_hook=send)
        self.assertEqual(status, "interrupted")
        self.assertEqual(body, [])
        self.assertEqual(self.plane.puts, 0)

    async def test_preset_cancel_does_not_load_state_build_agent_or_run_model(self):
        cancel = asyncio.Event()
        cancel.set()
        self.adapter.factories["agent"] = lambda context: self.fail("factory ran")
        status, _ = await self.execute(cancel=cancel)
        self.assertEqual(status, "interrupted")
        self.assertEqual(self.plane.requests, [])

    async def test_allowlist_denies_unlisted_and_empty_without_tool_effects(self):
        def make_tool(body):
            @function_tool
            async def lookup(query: str) -> str:
                body.append(query)
                return "fact"

            return lookup

        for allowed in ([], ["other"], ["Lookup"]):
            with self.subTest(allowed=allowed):
                body = []
                lookup = make_tool(body)
                self.model = OfflineModel([Step([tool_call()])])
                self.adapter.factories["agent"] = lambda context, lookup=lookup: Agent(
                    name="deny", model=self.model, tools=[lookup]
                )
                status, events = await self.execute(extra={"allowed_tools": allowed})
                self.assertEqual(status, "error")
                self.assertEqual(body, [])
                methods = [event["method"] for event in events]
                self.assertLess(methods.index("tool_call"), methods.index("tool_auth"))
                denial = next(event["data"] for event in events if event["method"] == "tool_auth")
                self.assertEqual((denial["reason_code"], denial["stage"]), ("tool_not_allowed", "tool.call"))
                self.assertEqual(events[-1]["data"], {"status": "error"})

    async def test_allowlist_permits_exact_tool_name(self):
        @function_tool
        async def lookup(query: str) -> str:
            return "fact"

        self.model = OfflineModel([Step([tool_call()]), Step([message()])])
        self.adapter.factories["agent"] = lambda context: Agent(name="allow", model=self.model, tools=[lookup])
        self.assertEqual((await self.execute(extra={"allowed_tools": ["lookup"]}))[0], "success")

    async def test_giant_tool_arguments_fail_before_publish_or_body(self):
        body = []

        @function_tool
        async def lookup(query: str) -> str:
            body.append(query)
            return "fact"

        self.model = OfflineModel([Step([tool_call(arguments=json.dumps({"query": "x" * (3 << 20)}))])])
        self.adapter.factories["agent"] = lambda context: Agent(name="large args", model=self.model, tools=[lookup])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertFalse(body)
        self.assertFalse(any(event["method"] == "tool_call" for event in events))
        self.assertFalse(self.plane.blobs)
        self.assertIn("3 MiB", self.error(events))

    async def test_generator_tool_registration_fails_before_model(self):
        async def stream(query: str):
            yield query

        self.adapter.factories["agent"] = lambda context: Agent(
            name="stream", model=self.model, tools=[function_tool(stream)]
        )
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertIn("streaming", self.error(events))
        self.assertEqual(self.model.calls, [])

    async def test_custom_iterator_tool_result_never_becomes_a_phantom_success(self):
        class AsyncOutput:
            def __init__(self):
                self.closed = False
                self.consumed = False

            def __aiter__(self):
                return self

            async def __anext__(self):
                self.consumed = True
                raise StopAsyncIteration

            async def aclose(self):
                self.closed = True

        output = AsyncOutput()

        @function_tool
        async def lookup(query: str):
            return output

        self.model = OfflineModel([Step([tool_call()]), Step([message("phantom")])])
        self.adapter.factories["agent"] = lambda context: Agent(name="iterable", model=self.model, tools=[lookup])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertIn("streaming", self.error(events))
        self.assertEqual(len(self.model.calls), 1)
        self.assertTrue(output.closed)
        self.assertFalse(output.consumed)
        self.assertFalse(self.plane.blobs)

    async def test_cancel_at_tool_start_does_not_become_a_wrapped_sdk_error(self):
        cancel, body = asyncio.Event(), []

        @function_tool
        async def lookup(query: str) -> str:
            body.append(query)
            return "fact"

        self.model = OfflineModel([Step([tool_call()])])
        self.adapter.factories["agent"] = lambda context: Agent(name="cancel tool", model=self.model, tools=[lookup])

        async def send(event):
            if event["method"] == "tool_call":
                cancel.set()

        status, events = await self.execute(cancel=cancel, send_hook=send)
        self.assertEqual(status, "interrupted")
        self.assertFalse(body)
        self.assertFalse(any(event["method"] == "error" for event in events))

    async def test_concurrent_same_thread_cas_loser_withholds_success(self):
        ready, count = asyncio.Event(), 0
        models = []

        async def factory(context):
            nonlocal count
            count += 1
            if count == 2:
                ready.set()
            await ready.wait()
            model = OfflineModel([Step([message(context.run_id, identity=context.run_id)])])
            models.append(model)
            return Agent(name="CAS race", model=model)

        self.adapter.factories["agent"] = factory
        results = await asyncio.gather(self.execute("run-a"), self.execute("run-b"), return_exceptions=True)
        self.assertEqual(sum(isinstance(result, CompletionDeliveryError) for result in results), 1)
        self.assertEqual(sum(isinstance(result, tuple) and result[0] == "success" for result in results), 1)
        self.assertEqual(len(self.plane.blobs), 1)
        self.assertTrue(all(model.closed == 1 for model in models))

    async def test_empty_delta_provider_falls_back_to_completed_native_text(self):
        self.model = OfflineModel([Step([message("native final")], deltas=[])])
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(self.values(events)["messages"][-1]["content"], "native final")

    async def test_different_stream_and_completed_text_cannot_commit(self):
        self.model = OfflineModel([Step([message("native final")], deltas=["different"])])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertIn("streamed text differs", self.error(events))
        self.assertFalse(self.plane.blobs)

    async def test_oversized_provider_error_remains_deliverable(self):
        self.model = OfflineModel([Step(error=RuntimeError("x" * (3 << 20)))])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertLess(len(self.error(events).encode()), 66 << 10)
        self.assertTrue(self.error(events).endswith("[truncated]"))

    async def test_unsupported_agent_features_fail_before_model_or_tool_execution(self):
        @function_tool(needs_approval=True)
        async def approval() -> str:
            self.fail("approval tool ran")

        @function_tool(defer_loading=True)
        async def deferred() -> str:
            self.fail("deferred tool ran")

        @function_tool(allowed_callers=["direct"])
        async def caller() -> str:
            self.fail("programmatic tool ran")

        variants = [
            {"tools": [WebSearchTool()]},
            {"tools": [approval]},
            {"tools": [deferred]},
            {"tools": [caller]},
            {"tools": [Agent(name="nested", model=self.model).as_tool(tool_name="nested", tool_description="nested")]},
            {"handoffs": [Agent(name="nested", model=self.model)]},
            {"mcp_servers": [MCPServerStdio(params={"command": "not-started"})]},
            {"prompt": {"id": "remote-prompt"}},
            {"output_type": dict},
            {"tool_use_behavior": "stop_on_first_tool"},
        ]
        for variant in variants:
            with self.subTest(variant=variant):
                self.adapter.factories["agent"] = lambda context, variant=variant: Agent(
                    name="unsupported", model=self.model, **variant
                )
                status, _ = await self.execute()
                self.assertEqual(status, "error")
                self.assertEqual(self.model.calls, [])
                self.assertFalse(self.plane.blobs)

    async def test_native_resume_nontext_and_caller_history_reject_before_factory(self):
        cases = [
            {"resume_command": {}},
            {"checkpoint_ref": "old"},
            {"input": {"messages": [{"role": "assistant", "content": "history"}]}},
            {"input": {"messages": [{"role": "user", "content": [{"type": "image", "url": "x"}]}]}},
            {"input": {"messages": [{"role": "user", "content": "hello"}], "state": {}}},
        ]
        self.adapter.factories["agent"] = lambda context: self.fail("invalid input reached factory")
        for extra in cases:
            with self.subTest(extra=extra):
                self.assertEqual((await self.execute(extra=extra))[0], "error")
        self.assertEqual(self.plane.requests, [])

    async def test_frozen_factory_context_has_trusted_attempt_and_tenant(self):
        contexts = []

        async def factory(context):
            contexts.append(context)
            with self.assertRaises(FrozenInstanceError):
                context.generation = 999
            return Agent(name="context", model=self.model)

        self.adapter.factories["agent"] = factory
        self.assertEqual((await self.execute(tenant="tenant-a", generation=7))[0], "success")
        self.assertEqual((contexts[0].tenant_id, contexts[0].generation, contexts[0].run_id), ("tenant-a", 7, "run-1"))

    async def test_concurrent_tenants_use_separate_native_histories_and_contexts(self):
        seen = []

        async def factory(context):
            seen.append((context.run_id, context.tenant_id, context.generation))
            await asyncio.sleep(0)
            return Agent(name="isolated", model=OfflineModel([Step([message(context.tenant_id)])]))

        self.adapter.factories["agent"] = factory
        results = await asyncio.gather(
            self.execute("same-run", tenant="a", generation=1), self.execute("same-run", tenant="b", generation=2)
        )
        self.assertTrue(all(status == "success" for status, _ in results))
        self.assertEqual({key[0] for key in self.plane.blobs}, {"a", "b"})
        self.assertEqual(set(seen), {("same-run", "a", 1), ("same-run", "b", 2)})

    async def test_read_and_write_failures_withhold_completion(self):
        self.plane.reject_get = True
        with self.assertRaises(CompletionDeliveryError):
            await self.execute()
        self.assertEqual(self.model.calls, [])
        self.plane.reject_get = False
        self.plane.reject_put = True
        with self.assertRaises(CompletionDeliveryError):
            await self.execute()
        self.assertFalse(self.plane.blobs)
        self.assertEqual(self.model.closed, 1)

    async def test_delivery_failure_after_commit_recovers_without_reexecuting_model(self):
        async def send(event):
            if event["method"] == "values" and "messages" in event["data"]:
                raise CompletionDeliveryError("lost final acknowledgement")

        with self.assertRaises(CompletionDeliveryError):
            await self.execute(send_hook=send)
        self.assertEqual(self.plane.puts, 1)
        self.adapter.factories["agent"] = lambda context: self.fail("replayed factory")
        self.assertEqual((await self.execute(generation=2))[0], "success")
        self.assertEqual(len(self.model.calls), 1)

    async def test_delivery_failure_in_native_tool_hook_remains_recoverable(self):
        body = []

        @function_tool
        async def lookup(query: str) -> str:
            body.append(query)
            return "fact"

        self.model = OfflineModel([Step([tool_call()])])
        self.adapter.factories["agent"] = lambda context: Agent(name="delivery", model=self.model, tools=[lookup])

        async def send(event):
            if event["method"] == "tool_call":
                raise CompletionDeliveryError("tool event delivery failed")

        with self.assertRaises(CompletionDeliveryError):
            await self.execute(send_hook=send)
        self.assertFalse(body)
        self.assertFalse(self.plane.blobs)
        self.assertEqual(self.model.closed, 1)

    async def test_refusal_fails_explicitly_with_observed_usage_and_no_success_receipt(self):
        self.model = OfflineModel([Step([message("I cannot help.", refusal=True)])])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertEqual(self.values(events)["usage"]["prompt_tokens"], 10)
        self.assertFalse(self.plane.blobs)
        self.assertTrue(
            any(event["data"].get("type") == "ModelRefusalError" for event in events if event["method"] == "error")
        )

    async def test_reasoning_history_survives_without_becoming_ui_text(self):
        reasoning = ResponseReasoningItem(
            id="reasoning-1", type="reasoning", summary=[], encrypted_content="opaque-reasoning", status="completed"
        )
        self.model = OfflineModel([Step([reasoning, message()])])
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(self.saved()["history"][1]["encrypted_content"], "opaque-reasoning")
        self.assertEqual(self.values(events)["messages"][-1]["content"], "hello")

    async def test_unfinished_native_message_never_commits(self):
        self.model = OfflineModel([Step([message(status="in_progress")])])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertIn("unfinished", self.error(events))
        self.assertFalse(self.plane.blobs)

    async def test_no_native_assistant_output_never_becomes_empty_success(self):
        self.model = OfflineModel([Step([])])
        status, _ = await self.execute()
        self.assertEqual(status, "error")
        self.assertFalse(self.plane.blobs)

    async def test_giant_native_delta_fails_before_publish_and_commit(self):
        self.model = OfflineModel([Step([message("x" * (3 << 20))])])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertIn("3 MiB", self.error(events))
        self.assertFalse(any(event["method"] == "messages" for event in events))
        self.assertFalse(self.plane.blobs)

    async def test_output_and_receipt_limits_fail_before_state_commit(self):
        with patch.object(managed, "MAX_OUTPUT_BYTES", 180):
            self.model = OfflineModel([Step([message("short")])])
            status, _ = await self.execute()
            self.assertEqual(status, "error")
            self.assertFalse(self.plane.blobs)
        with patch.object(managed, "MAX_RECEIPT_BYTES", 12):
            self.model = OfflineModel(
                [
                    Step(
                        [
                            message("abcdefghi"),
                        ],
                        deltas=["abc", "def", "ghi"],
                    )
                ]
            )
            status, events = await self.execute()
            self.assertEqual(status, "error")
            self.assertIn("receipt", self.error(events))
            self.assertFalse(self.plane.blobs)

    async def test_model_close_failure_does_not_rewrite_committed_success(self):
        self.model.close_error = True
        self.assertEqual((await self.execute())[0], "success")
        self.assertEqual(self.plane.puts, 1)

    async def test_config_loads_offline_example_factory_without_api_credentials(self):
        config = ROOT.parent / "examples" / "managed_openai_agents" / "langgraph.json"
        await self.adapter.load_config(str(config))
        self.assertIn("managed_openai_agents", self.adapter.factories)

    async def test_native_text_chunk_tool_outputs_preserve_public_history(self):
        def make_factory(output, model):
            @function_tool
            async def lookup(query: str):
                return output

            return lambda context: Agent(name="text chunks", model=model, tools=[lookup])

        variants = [ToolOutputText(text="fact"), [ToolOutputText(text="one"), ToolOutputText(text="two")], []]
        for index, output in enumerate(variants):
            with self.subTest(output=output):
                self.model = OfflineModel(
                    [
                        Step([tool_call(identity=f"chunk-{index}")]),
                        Step([message("answer", identity=f"answer-{index}")]),
                    ]
                )
                self.adapter.factories["agent"] = make_factory(output, self.model)
                status, _ = await self.execute(f"run-{index}")
                self.assertEqual(status, "success")
                native_output = [
                    item["output"]
                    for item in self.saved()["history"]
                    if item.get("call_id") == f"chunk-{index}" and item.get("type") == "function_call_output"
                ][0]
                expected = output if isinstance(output, list) else [output]
                self.assertEqual(
                    native_output,
                    [{"type": "input_text", "text": item.text} for item in expected] if expected else "[]",
                )

    async def test_binary_tool_output_fails_before_another_model_or_state_commit(self):
        @function_tool
        async def lookup(query: str):
            return ToolOutputImage(image_url="data:image/png;base64,offline")

        self.model = OfflineModel([Step([tool_call()]), Step([message("unsupported")])])
        self.adapter.factories["agent"] = lambda context: Agent(name="image result", model=self.model, tools=[lookup])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertIn("text only", self.error(events))
        self.assertEqual(len(self.model.calls), 1)
        self.assertFalse(self.plane.blobs)

    async def test_giant_tool_output_fails_before_next_model_and_without_recoverable_commit_error(self):
        @function_tool
        async def lookup(query: str) -> str:
            return "x" * (3 << 20)

        self.model = OfflineModel([Step([tool_call()]), Step([message("unsupported")])])
        self.adapter.factories["agent"] = lambda context: Agent(name="large result", model=self.model, tools=[lookup])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertIn("3 MiB", self.error(events))
        self.assertEqual(len(self.model.calls), 1)
        self.assertEqual(self.plane.puts, 0)

    async def test_native_state_limit_is_a_deterministic_error_before_put(self):
        reasoning = ResponseReasoningItem(
            id="reasoning-large", type="reasoning", summary=[], encrypted_content="x" * 600, status="completed"
        )
        self.model = OfflineModel([Step([reasoning, message()])])
        with patch.object(managed, "MAX_STATE_BYTES", 400):
            status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertIn("16 MiB", self.error(events))
        self.assertEqual(self.plane.puts, 0)

    async def test_max_turns_preserves_aggregate_usage_without_committing_partial_tools(self):
        @function_tool
        async def lookup(query: str) -> str:
            return "continue"

        self.model = OfflineModel([Step([tool_call(identity=f"loop-{index}")]) for index in range(10)])
        self.adapter.factories["agent"] = lambda context: Agent(name="loop", model=self.model, tools=[lookup])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        error_type = next(event["data"]["type"] for event in events if event["method"] == "error")
        self.assertEqual(error_type, "MaxTurnsExceeded")
        self.assertEqual(
            self.values(events)["usage"],
            {"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120, "model": "offline-model"},
        )
        self.assertEqual((self.model.closed, self.model.streams_closed), (1, 10))
        self.assertFalse(self.plane.blobs)

    async def test_output_guardrail_error_preserves_observed_usage_and_committed_history(self):
        await self.execute()
        prior = dict(self.plane.blobs)

        @output_guardrail
        async def reject(context, agent, output):
            return GuardrailFunctionOutput(output_info="offline rejection", tripwire_triggered=True)

        self.model = OfflineModel([Step([message("blocked")])])
        self.adapter.factories["agent"] = lambda context: Agent(
            name="guarded", model=self.model, output_guardrails=[reject]
        )
        status, events = await self.execute("run-guardrail")
        self.assertEqual(status, "error")
        self.assertEqual(self.values(events)["usage"]["prompt_tokens"], 10)
        self.assertEqual(self.plane.blobs, prior)
        self.assertEqual(self.model.closed, 1)

    async def test_cancel_during_tool_body_awaits_cooperative_cleanup(self):
        entered, closed, cancel = asyncio.Event(), asyncio.Event(), asyncio.Event()

        @function_tool
        async def lookup(query: str) -> str:
            entered.set()
            try:
                await asyncio.Event().wait()
            finally:
                closed.set()
            return "unreachable"

        self.model = OfflineModel([Step([tool_call()])])
        self.adapter.factories["agent"] = lambda context: Agent(name="tool cancel", model=self.model, tools=[lookup])
        task = asyncio.create_task(self.execute(cancel=cancel))
        await asyncio.wait_for(entered.wait(), 3)
        cancel.set()
        status, events = await asyncio.wait_for(task, 3)
        self.assertEqual(status, "interrupted")
        self.assertTrue(closed.is_set())
        self.assertEqual(self.values(events)["usage"]["prompt_tokens"], 10)
        self.assertFalse(self.plane.blobs)


if __name__ == "__main__":
    unittest.main()
