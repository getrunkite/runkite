"""Offline managed-run regressions through Pydantic AI's real public agent loop."""

from __future__ import annotations

import asyncio
import json
import os
import sys
import unittest
from dataclasses import FrozenInstanceError
from datetime import date
from pathlib import Path
from typing import Literal
from unittest.mock import patch

import httpx
from pydantic import BaseModel, field_validator
from pydantic_ai import Agent, DeferredToolRequests, NativeOutput, RunContext, Tool, ToolOutput
from pydantic_ai.capabilities import SelectModel
from pydantic_ai.messages import UserPromptPart
from pydantic_ai.models.fallback import FallbackModel
from pydantic_ai.models.function import DeltaToolCall, FunctionModel
from pydantic_ai.profiles import ModelProfile

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
sys.path.insert(0, str(ROOT / "adapters"))
os.environ["PYDANTIC_AI_NO_BANNER"] = "1"

from runkite_runner.completion import CompletionDeliveryError  # noqa: E402
from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402

from pydantic_ai_adapter import PydanticAIAdapter, PydanticAIAgentDefinition  # noqa: E402
from pydantic_ai_adapter import adapter as managed  # noqa: E402
from pydantic_ai_adapter.state import FRAMEWORK  # noqa: E402


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


class NativeModel(FunctionModel):
    def __init__(self, steps, name="offline-pydantic", profile=None):
        self.steps, self.calls, self.closed, self.opened, self.streams_closed = steps, [], 0, 0, 0
        super().__init__(stream_function=self.stream, model_name=name, profile=profile)

    async def __aenter__(self):
        self.opened += 1
        return await super().__aenter__()

    async def __aexit__(self, *args):
        self.closed += 1
        return await super().__aexit__(*args)

    async def stream(self, messages, info):
        step = self.steps[len(self.calls)]
        self.calls.append((list(messages), info))
        try:
            async for item in step(messages, info):
                yield item
        finally:
            self.streams_closed += 1


async def text(messages, info):
    yield "hello "
    yield "world"


class PydanticAIAdapterTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.plane, self.stores = Plane(), []
        self.adapter = PydanticAIAdapter()
        self.adapter.configure(http_address="http://plane", runner_token="test-token")
        original = self.adapter._state

        def state():
            store = original()
            store._client._transport = self.plane.transport()
            self.stores.append(store)
            return store

        self.adapter._state = state
        self.model = NativeModel([text])
        self.adapter.factories["agent"] = lambda context: Agent(self.model)
        logging_patch = patch.object(managed.logger, "exception")
        logging_patch.start()
        self.addCleanup(logging_patch.stop)

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

    async def test_native_text_drains_to_durable_success_with_current_usage(self):
        async def send(event):
            if event["method"] == "values" and "messages" in event["data"]:
                self.assertEqual(self.plane.puts, 1)

        status, events = await self.execute(send_hook=send)
        self.assertEqual(status, "success", events)
        self.assertEqual(self.output(events)["messages"][-1]["content"], "hello world")
        self.assertGreater(self.output(events)["usage"]["total_tokens"], 0)
        self.assertEqual(self.output(events)["usage"]["model"], "offline-pydantic")
        self.assertNotIn("cost_usd", self.output(events)["usage"])
        self.assertEqual(self.saved()["completed"]["text_events"], ["hello ", "world"])
        self.assertEqual((self.model.opened, self.model.closed, self.model.streams_closed), (1, 1, 1))

    async def test_schema_retry_then_final_validated_json_is_committed_once(self):
        class Answer(BaseModel):
            count: int
            day: date

            @field_validator("count")
            @classmethod
            def normalize(cls, value):
                return value * 2

        async def invalid(messages, info):
            yield {
                0: DeltaToolCall(
                    name=info.output_tools[0].name, json_args='{"count":"bad","day":"2026-10-30"}', tool_call_id="out-1"
                )
            }

        async def valid(messages, info):
            yield {
                0: DeltaToolCall(
                    name=info.output_tools[0].name, json_args='{"count":7,"day":"2026-10-30"}', tool_call_id="out-2"
                )
            }

        self.model = NativeModel([invalid, valid])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, output_type=Answer)
        status, events = await self.execute(extra={"allowed_tools": []})
        self.assertEqual(status, "success", events)
        self.assertEqual(self.output(events)["result"], {"count": 14, "day": "2026-10-30"})
        self.assertEqual(len(self.model.calls), 2)
        self.assertEqual(self.plane.puts, 1)
        self.assertFalse(any(event["method"] == "tool_auth" for event in events))

    async def test_typed_scalar_is_not_misclassified_as_plain_text(self):
        async def result(messages, info):
            yield {
                0: DeltaToolCall(name=info.output_tools[0].name, json_args='{"response":"ok"}', tool_call_id="typed-1")
            }

        self.model = NativeModel([result])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, output_type=Literal["ok", "deny"])
        status, events = await self.execute(extra={"allowed_tools": []})
        self.assertEqual(status, "success", events)
        self.assertEqual(self.output(events)["result"], "ok")
        self.assertFalse(any(event["method"] == "messages" for event in events))

    async def test_local_tool_observes_usage_and_allowlist_before_effect(self):
        effects, delivered = [], []

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"Runkite"}', tool_call_id="call-1")}

        def lookup(query: str) -> str:
            self.assertGreater(delivered[-1]["total_tokens"], 0)
            self.assertEqual(delivered[-1]["model"], "offline-pydantic")
            effects.append(query)
            return "fact"

        self.model = NativeModel([first, text])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])

        async def receive(event):
            if event["method"] == "values":
                delivered.append(event["data"]["usage"])

        status, events = await self.execute(extra={"allowed_tools": ["lookup"]}, send_hook=receive)
        self.assertEqual(status, "success", events)
        self.assertEqual(effects, ["Runkite"])

    async def test_deny_all_and_wrong_name_prevent_local_tool_body(self):
        effects = []

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"Runkite"}', tool_call_id="denied")}

        def lookup(query: str) -> str:
            effects.append(query)
            return "fact"

        for index, allowed in enumerate(([], ["other"], ["Lookup"])):
            self.model = NativeModel([first])
            self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])
            status, events = await self.execute(f"deny-{index}", extra={"allowed_tools": allowed})
            self.assertEqual(status, "error")
            self.assertEqual(effects, [])
            self.assertTrue(
                any(
                    event["method"] == "tool_auth" and event["data"]["reason_code"] == "tool_not_allowed"
                    for event in events
                )
            )
        self.assertEqual(self.plane.puts, 0)

    async def test_receipt_replay_skips_factory_and_preserves_usage_with_new_generation(self):
        status, events = await self.execute()
        self.assertEqual(status, "success", events)
        before = self.output(events)["usage"]
        self.adapter.factories["agent"] = lambda context: self.fail("replayed factory")
        status, replay = await self.execute(generation=2)
        self.assertEqual(status, "success")
        self.assertEqual(self.output(replay)["pydantic_ai"]["generation"], 2)
        self.assertEqual(self.output(replay)["usage"], before)
        self.assertEqual((len(self.model.calls), self.plane.puts), (1, 1))

    async def test_next_turn_preserves_native_conversation_without_rebilling_prior_tokens(self):
        status, first = await self.execute()
        self.assertEqual(status, "success", first)
        prior_usage = self.output(first)["usage"]["total_tokens"]
        self.model = NativeModel([text])
        status, second = await self.execute("run-2")
        self.assertEqual(status, "success", second)
        native = self.saved()["history"]["usage"]
        self.assertEqual(
            native["input_tokens"] + native["output_tokens"], prior_usage + self.output(second)["usage"]["total_tokens"]
        )
        prompts = [
            part.content
            for message in self.model.calls[0][0]
            for part in message.parts
            if isinstance(part, UserPromptPart)
        ]
        self.assertEqual(prompts, ["question", "question"])

    async def test_preserves_all_current_user_system_messages(self):
        messages = [
            {"role": "system", "content": "rules"},
            {"role": "human", "content": "one"},
            {"role": "user", "content": [{"type": "text", "text": "two"}]},
        ]
        status, events = await self.execute(extra={"input": {"messages": messages}})
        self.assertEqual(status, "success", events)
        parts = self.model.calls[0][0][-1].parts
        self.assertEqual([getattr(part, "content", None) for part in parts], ["rules", "one", "two"])

    async def test_dependencies_and_frozen_trusted_factory_context(self):
        def factory(context):
            with self.assertRaises(FrozenInstanceError):
                context.generation = 3

            def lookup(ctx: RunContext[dict], query: str) -> str:
                self.assertEqual(ctx.deps["tenant"], context.tenant_id)
                return query

            return PydanticAIAgentDefinition(
                Agent(self.model, tools=[lookup], deps_type=dict), {"tenant": context.tenant_id}
            )

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"safe"}', tool_call_id="deps")}

        self.model = NativeModel([first, text])
        self.adapter.factories["agent"] = factory
        status, events = await self.execute(tenant="tenant-a", generation=7)
        self.assertEqual(status, "success", events)

    async def test_preset_cancel_never_constructs_or_calls_anything(self):
        cancel = asyncio.Event()
        cancel.set()
        self.adapter.factories["agent"] = lambda context: self.fail("canceled factory")
        status, events = await self.execute(cancel=cancel)
        self.assertEqual(status, "interrupted")
        self.assertEqual(self.stores, [])

    async def test_checkpoint_failures_withhold_completion_and_close_clients(self):
        self.plane.fail_get = True
        with self.assertRaises(CompletionDeliveryError):
            await self.execute()
        self.assertFalse(self.model.calls)
        self.plane.fail_get, self.plane.fail_put = False, True
        with self.assertRaises(CompletionDeliveryError):
            await self.execute()
        self.assertEqual(self.model.closed, 1)

    async def test_unsupported_approval_output_function_and_mode_fail_before_model(self):
        def local(value: str) -> str:
            self.fail("unsupported local tool ran")

        for definition in (
            lambda: Agent(self.model, tools=[Tool(local, requires_approval=True)]),
            lambda: Agent(self.model, output_type=[str, DeferredToolRequests]),
            lambda: Agent(self.model, output_type=local),
            lambda: Agent(self.model, output_type=NativeOutput(dict)),
            lambda: Agent(self.model, output_type=ToolOutput(dict, name="custom-output")),
        ):
            self.adapter.factories["agent"] = lambda context, definition=definition: definition()
            status, _ = await self.execute()
            self.assertEqual(status, "error")
            self.assertFalse(self.model.calls)

    async def test_text_branch_of_union_does_not_fabricate_a_typed_result(self):
        class Answer(BaseModel):
            count: int

        self.adapter.factories["agent"] = lambda context: Agent(self.model, output_type=str | Answer)
        status, events = await self.execute()
        self.assertEqual(status, "success", events)
        self.assertNotIn("result", self.output(events))
        self.assertEqual(self.output(events)["messages"][-1]["content"], "hello world")

    async def test_false_zero_empty_and_null_typed_values_survive(self):
        cases = ((bool, False), (int, 0), (list[int], []), (Literal["ok", None], None))
        for index, (output_type, value) in enumerate(cases):

            async def result(messages, info, value=value):
                yield {
                    0: DeltaToolCall(
                        name=info.output_tools[0].name,
                        json_args=json.dumps({"response": value}),
                        tool_call_id="typed-value",
                    )
                }

            self.model = NativeModel([result])
            self.adapter.factories["agent"] = lambda context, output_type=output_type: Agent(
                self.model, output_type=output_type
            )
            status, events = await self.execute(f"typed-{index}", thread=f"thread-{index}", extra={"allowed_tools": []})
            self.assertEqual(status, "success", events)
            self.assertIn("result", self.output(events))
            self.assertEqual(self.output(events)["result"], value)

    async def test_cancel_from_observed_usage_keeps_known_model_and_prevents_tool(self):
        cancel, effects, delivered = asyncio.Event(), [], []

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"safe"}', tool_call_id="cancel-tool")}

        def lookup(query: str) -> str:
            effects.append(query)
            return query

        self.model = NativeModel([first])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])

        async def receive(event):
            if event["method"] == "values" and event["data"]["usage"]["total_tokens"] > 0:
                delivered.append(event["data"]["usage"])
                cancel.set()

        status, events = await asyncio.wait_for(self.execute(cancel=cancel, send_hook=receive), 2)
        self.assertEqual(status, "interrupted")
        self.assertEqual(effects, [])
        self.assertEqual(self.output(events)["usage"], delivered[0])
        self.assertEqual(self.output(events)["usage"]["model"], "offline-pydantic")
        self.assertEqual(self.model.closed, 1)
        self.assertEqual(self.plane.puts, 0)

    async def test_cancel_after_first_tool_preserves_paid_usage_and_closes_stalled_model(self):
        entered, cancel = asyncio.Event(), asyncio.Event()

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"safe"}', tool_call_id="first")}

        async def stall(messages, info):
            entered.set()
            await asyncio.Event().wait()
            yield "unreachable"

        def lookup(query: str) -> str:
            return query

        self.model = NativeModel([first, stall])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])
        task = asyncio.create_task(self.execute(cancel=cancel))
        await asyncio.wait_for(entered.wait(), 2)
        cancel.set()
        status, events = await asyncio.wait_for(task, 2)
        self.assertEqual(status, "interrupted")
        self.assertGreater(self.output(events)["usage"]["total_tokens"], 0)
        self.assertEqual(self.output(events)["usage"]["model"], "offline-pydantic")
        self.assertEqual((self.model.closed, self.model.streams_closed), (1, 2))
        self.assertEqual(self.plane.puts, 0)

    async def test_provider_failure_after_a_paid_tool_step_retains_usage_and_history(self):
        status, initial = await self.execute()
        self.assertEqual(status, "success", initial)
        saved = dict(self.plane.blobs)

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"safe"}', tool_call_id="paid")}

        async def broken(messages, info):
            raise RuntimeError("provider failed")
            yield "unreachable"

        def lookup(query: str) -> str:
            return query

        self.model = NativeModel([first, broken])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])
        status, events = await self.execute("run-error")
        self.assertEqual(status, "error")
        self.assertGreater(self.output(events)["usage"]["total_tokens"], 0)
        self.assertEqual(self.output(events)["usage"]["model"], "offline-pydantic")
        self.assertEqual(self.plane.blobs, saved)

    async def test_validation_retry_exhaustion_never_commits_a_partial_typed_result(self):
        prior_status, prior_events = await self.execute()
        self.assertEqual(prior_status, "success", prior_events)
        previous = dict(self.plane.blobs)

        class Answer(BaseModel):
            count: int

        async def invalid(messages, info):
            yield {
                0: DeltaToolCall(name=info.output_tools[0].name, json_args='{"count":"bad"}', tool_call_id="invalid")
            }

        self.model = NativeModel([invalid, invalid])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, output_type=Answer, retries=1)
        status, events = await self.execute("run-invalid-typed")
        self.assertEqual(status, "error")
        self.assertEqual(len(self.model.calls), 2)
        self.assertGreater(self.output(events)["usage"]["total_tokens"], 0)
        self.assertNotIn("result", self.output(events))
        self.assertEqual(self.plane.puts, 1)
        self.assertEqual(self.plane.blobs, previous)

    async def test_mixed_selected_models_leave_aggregate_unpriced(self):
        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"safe"}', tool_call_id="mixed")}

        def lookup(query: str) -> str:
            return query

        one, two = NativeModel([first], "first-model"), NativeModel([text], "second-model")
        self.adapter.factories["agent"] = lambda context: Agent(
            tools=[lookup],
            capabilities=[
                SelectModel(lambda ctx: one if ctx.usage.requests == 0 else two),
            ],
        )
        status, events = await self.execute()
        self.assertEqual(status, "success", events)
        self.assertGreater(self.output(events)["usage"]["total_tokens"], 0)
        self.assertNotIn("model", self.output(events)["usage"])

    async def test_streaming_tool_return_is_rejected_without_consuming_or_continuing(self):
        class Output:
            closed = False

            def __aiter__(self):
                return self

            async def __anext__(self):
                self.fail("stream was consumed")

            async def aclose(self):
                self.closed = True

        output = Output()

        async def lookup(query: str):
            return output

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"safe"}', tool_call_id="stream")}

        self.model = NativeModel([first])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertTrue(output.closed)
        self.assertEqual(len(self.model.calls), 1)
        self.assertEqual(self.plane.puts, 0)

    async def test_giant_text_and_tool_arguments_fail_before_delivery_or_effects(self):
        async def huge_text(messages, info):
            yield "x" * (3 << 20)

        self.model = NativeModel([huge_text])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertFalse(any(event["method"] == "messages" for event in events))
        effects = []

        def lookup(query: str) -> str:
            effects.append(query)
            return query

        async def huge_tool(messages, info):
            yield {
                0: DeltaToolCall(name="lookup", json_args=json.dumps({"query": "x" * (3 << 20)}), tool_call_id="huge")
            }

        self.model = NativeModel([huge_tool])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])
        status, events = await self.execute("large-tool")
        self.assertEqual(status, "error")
        self.assertEqual(effects, [])
        self.assertFalse(any(event["method"] == "tool_call" for event in events))
        self.assertEqual(self.plane.puts, 0)

    async def test_delivery_failure_after_commit_replays_without_native_work(self):
        async def fail(event):
            if event["method"] == "values" and "messages" in event["data"]:
                raise CompletionDeliveryError("lost final event delivery")

        with self.assertRaises(CompletionDeliveryError):
            await self.execute(send_hook=fail)
        self.assertEqual(self.plane.puts, 1)
        self.adapter.factories["agent"] = lambda context: self.fail("replayed factory")
        status, events = await self.execute(generation=2)
        self.assertEqual(status, "success")
        self.assertEqual((len(self.model.calls), self.plane.puts), (1, 1))

    async def test_receipt_input_mismatch_exposes_no_saved_text(self):
        await self.execute()
        status, events = await self.execute(
            generation=2, extra={"input": {"messages": [{"role": "user", "content": "different"}]}}
        )
        self.assertEqual(status, "error")
        self.assertFalse(any(event["method"] == "messages" for event in events))

    async def test_invalid_input_is_rejected_before_factory(self):
        self.adapter.factories["agent"] = lambda context: self.fail("invalid input reached factory")
        for extra in (
            {"resume_command": {}},
            {"checkpoint_ref": "old"},
            {"input": {"messages": [{"role": "assistant", "content": "old"}]}},
            {"input": {"messages": [{"role": "user", "content": [{"type": "image", "url": "x"}]}]}},
        ):
            status, _ = await self.execute(extra=extra)
            self.assertEqual(status, "error")
        self.assertEqual(self.stores, [])

    async def test_local_tool_delivery_failure_withholds_terminal_completion(self):
        effects = []

        def lookup(query: str) -> str:
            effects.append(query)
            return query

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"safe"}', tool_call_id="delivery")}

        self.model = NativeModel([first])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])

        async def fail(event):
            if event["method"] == "tool_call":
                raise CompletionDeliveryError("tool audit event delivery failed")

        with self.assertRaises(CompletionDeliveryError):
            await self.execute(send_hook=fail)
        self.assertEqual(effects, [])
        self.assertEqual(self.plane.puts, 0)
        self.assertEqual(self.model.closed, 1)

    async def test_native_fallback_rejected_before_a_model_request(self):
        self.adapter.factories["agent"] = lambda context: Agent(FallbackModel(self.model, self.model))
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertFalse(self.model.calls)

    async def test_receipt_and_durable_output_bounds_fail_before_commit(self):
        with patch.object(managed, "MAX_RECEIPT_BYTES", 10):
            status, _ = await self.execute()
        self.assertEqual(status, "error")
        self.assertEqual(self.plane.puts, 0)
        self.model = NativeModel([text])
        with patch.object(managed, "MAX_OUTPUT_BYTES", 170):
            status, _ = await self.execute("run-bounded-output")
        self.assertEqual(status, "error")
        self.assertEqual(self.plane.puts, 0)

    async def test_typed_result_replay_preserves_validated_json_and_usage(self):
        class Answer(BaseModel):
            count: int

        async def result(messages, info):
            yield {
                0: DeltaToolCall(name=info.output_tools[0].name, json_args='{"count":7}', tool_call_id="json-result")
            }

        self.model = NativeModel([result])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, output_type=Answer)
        status, initial = await self.execute()
        self.assertEqual(status, "success", initial)
        self.adapter.factories["agent"] = lambda context: self.fail("replayed typed factory")
        status, replay = await self.execute(generation=2)
        self.assertEqual(status, "success")
        self.assertEqual(self.output(replay)["result"], {"count": 7})
        self.assertEqual(self.output(initial)["usage"], self.output(replay)["usage"])
        self.assertEqual((len(self.model.calls), self.plane.puts), (1, 1))

    async def test_concurrent_tenants_keep_context_usage_and_conversations_separate(self):
        seen = []

        def factory(context):
            seen.append((context.tenant_id, context.generation))

            async def reply(messages, info):
                await asyncio.sleep(0)
                yield context.tenant_id

            return Agent(NativeModel([reply], context.tenant_id))

        self.adapter.factories["agent"] = factory
        results = await asyncio.gather(
            self.execute("a", tenant="tenant-a", generation=1), self.execute("b", tenant="tenant-b", generation=2)
        )
        self.assertTrue(all(status == "success" for status, _ in results), results)
        self.assertEqual(set(seen), {("tenant-a", 1), ("tenant-b", 2)})
        self.assertEqual({key[0] for key in self.plane.blobs}, {"tenant-a", "tenant-b"})
        self.assertEqual([self.output(events)["usage"]["model"] for _, events in results], ["tenant-a", "tenant-b"])

    async def test_native_agent_cleanup_failure_does_not_rewrite_committed_success(self):
        class CleanupAgent(Agent):
            async def __aexit__(self, *args):
                await super().__aexit__(*args)
                raise RuntimeError("cleanup failed")

        self.adapter.factories["agent"] = lambda context: CleanupAgent(self.model)
        status, events = await self.execute()
        self.assertEqual(status, "success", events)
        self.assertEqual(self.plane.puts, 1)
        self.assertEqual(self.model.closed, 1)

    async def test_binary_tool_result_is_rejected_before_another_model(self):
        def lookup(query: str):
            return b"binary data"

        async def first(messages, info):
            yield {0: DeltaToolCall(name="lookup", json_args='{"query":"safe"}', tool_call_id="binary")}

        self.model = NativeModel([first])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, tools=[lookup])
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertEqual(len(self.model.calls), 1)
        self.assertEqual(self.plane.puts, 0)

    async def test_large_validated_json_result_never_reaches_a_success_event_or_commit(self):
        class Answer(BaseModel):
            data: str

        async def result(messages, info):
            yield {
                0: DeltaToolCall(
                    name=info.output_tools[0].name,
                    json_args=json.dumps({"data": "x" * (3 << 20)}),
                    tool_call_id="large-result",
                )
            }

        self.model = NativeModel([result])
        self.adapter.factories["agent"] = lambda context: Agent(self.model, output_type=Answer)
        status, events = await self.execute(extra={"allowed_tools": []})
        self.assertEqual(status, "error")
        self.assertFalse(any(event["method"] == "values" and "result" in event["data"] for event in events))
        self.assertEqual(self.plane.puts, 0)
        self.assertGreater(self.output(events)["usage"]["total_tokens"], 0)

    async def test_default_native_output_mode_is_rejected_before_provider_effects(self):
        class Answer(BaseModel):
            count: int

        self.model = NativeModel(
            [text], profile=ModelProfile(default_structured_output_mode="native", supports_json_schema_output=True)
        )
        self.adapter.factories["agent"] = lambda context: Agent(self.model, output_type=Answer)
        status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertFalse(self.model.calls)
        self.assertEqual(self.plane.puts, 0)


if __name__ == "__main__":
    unittest.main()
