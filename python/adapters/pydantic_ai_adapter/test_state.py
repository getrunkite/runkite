"""Native conversations must be safe to restore before model or tool execution."""

from __future__ import annotations

import copy
import json
import math
import os
import sys
import unittest
from datetime import date
from decimal import Decimal
from pathlib import Path
from typing import Literal

import httpx
from pydantic import BaseModel, TypeAdapter, field_validator
from pydantic_ai import Agent
from pydantic_ai.conversation import Conversation, ConversationTypeAdapter
from pydantic_ai.messages import (
    ModelRequest,
    ModelResponse,
    SystemPromptPart,
    ThinkingPart,
    ToolCallPart,
    UserPromptPart,
)
from pydantic_ai.models.function import FunctionModel
from pydantic_ai.models.test import TestModel
from pydantic_ai.usage import RequestUsage

os.environ.setdefault("PYDANTIC_AI_NO_BANNER", "1")
ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

from runkite_runner.completion import CompletionDeliveryError  # noqa: E402
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient  # noqa: E402
from runkite_runner.tenant_ctx import bind_run, bind_runner, bind_tenant, reset_run, reset_runner, reset_tenant  # noqa: E402

from pydantic_ai_adapter.state import FRAMEWORK, PydanticAIState, dump_conversation, encode_state, state_json  # noqa: E402


class Answer(BaseModel):
    label: str
    day: date

    @field_validator("label")
    @classmethod
    def normalize(cls, value: str) -> str:
        return value.strip().upper()


async def envelope(*, typed: bool = False, tools: bool = False, prior: bool = False, retry: bool = False) -> dict:
    incoming = [{"role": "user", "content": "hello"}]
    messages = []
    conversation = Conversation()
    if prior:
        first = await Agent(TestModel(custom_output_text="First")).run(
            "old question", conversation=conversation, run_id="old-run"
        )
        conversation = first.conversation
        messages = [{"role": "user", "content": "old question"}, {"role": "assistant", "content": "First"}]
    before = copy.copy(conversation.usage)
    if retry:
        calls = 0

        def respond(_messages, _info):
            nonlocal calls
            calls += 1
            args = {"label": " hello ", "day": "not-a-date" if calls == 1 else "2026-10-01"}
            return ModelResponse(
                parts=[ToolCallPart("final_result", args, tool_call_id="reused-output")],
                usage=RequestUsage(input_tokens=10, output_tokens=4),
            )

        model = FunctionModel(respond)
        typed = True
    else:
        model = (
            TestModel(custom_output_args={"label": " hello ", "day": "2026-10-01"})
            if typed
            else TestModel(custom_output_text="Hello")
        )
    agent = Agent(model, output_type=Answer if typed else str, system_prompt="factory instructions")
    if tools:

        @agent.tool_plain
        def lookup() -> dict:
            return {"type": "approval", "description": "business JSON"}

    result = await agent.run("hello", conversation=conversation, run_id="run-one")
    text = "".join(
        part.content
        for item in result.new_messages()
        if isinstance(item, ModelResponse)
        for part in item.parts
        if getattr(part, "part_kind", None) == "text"
    )
    messages += incoming
    if text:
        messages.append({"role": "assistant", "content": text})
    output = {
        "messages": copy.deepcopy(messages),
        "pydantic_ai": {"framework": FRAMEWORK, "generation": 1},
        "usage": {
            "prompt_tokens": result.usage.input_tokens - before.input_tokens,
            "completion_tokens": result.usage.output_tokens - before.output_tokens,
            "total_tokens": result.usage.input_tokens
            - before.input_tokens
            + result.usage.output_tokens
            - before.output_tokens,
        },
    }
    if typed:
        output["result"] = TypeAdapter(Answer).dump_python(result.output, mode="json")
    return {
        "v": 1,
        "framework": FRAMEWORK,
        "graph_id": "agent-one",
        "history": dump_conversation(result.conversation),
        "messages": messages,
        "completed": {
            "run_id": "run-one",
            "status": "success",
            "input": state_json(incoming),
            "output": output,
            "text_events": [text[:2], text[2:]] if text else [],
        },
    }


class NativeStateTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.requests: list[httpx.Request] = []
        self.clients = []
        self.value = await envelope()

    async def asyncTearDown(self) -> None:
        for client in self.clients:
            await client.aclose()

    def store(
        self, body: bytes, *, headers: dict | None = None, status: int = 200, put_status: int = 204
    ) -> PydanticAIState:
        def respond(request):
            self.requests.append(request)
            if request.method == "GET":
                return httpx.Response(
                    status,
                    content=body,
                    headers=headers
                    if headers is not None
                    else {"ETag": '"7"', "X-Runkite-Checkpoint-Framework": FRAMEWORK},
                )
            return httpx.Response(put_status)

        client = OpaqueCheckpointClient(
            http_base_url="http://plane", framework=FRAMEWORK, transport=httpx.MockTransport(respond)
        )
        self.clients.append(client)
        return PydanticAIState(client)

    async def reject(self, value, *, reason: str | None = None) -> None:
        store = self.store(json.dumps(value, ensure_ascii=True).encode())
        with self.assertRaises(CompletionDeliveryError) as loaded:
            await store.load("thread-one", graph_id="agent-one")
        if reason:
            self.assertIn(reason, str(loaded.exception.__cause__))
        before = len(self.requests)
        with self.assertRaises(CompletionDeliveryError):
            await store.save("thread-one", value, '"7"', graph_id="agent-one")
        self.assertEqual(len(self.requests), before)

    async def test_real_sdk_text_tools_and_typed_conversations_restore_losslessly(self) -> None:
        for args in ({}, {"tools": True}, {"typed": True}, {"typed": True, "retry": True}):
            with self.subTest(args=args):
                value = await envelope(**args)
                store = self.store(state_json(value).encode())
                loaded, etag = await store.load("thread-one", graph_id="agent-one")
                self.assertEqual(loaded, value)
                self.assertEqual(etag, '"7"')
                native = ConversationTypeAdapter.validate_json(state_json(loaded["history"]), strict=True)
                self.assertEqual(dump_conversation(native), value["history"])
                await store.save("thread-one", loaded, etag, graph_id="agent-one")
                self.assertEqual(self.requests[-1].headers["if-match"], '"7"')
                self.assertEqual(json.loads(self.requests[-1].content), value)

    async def test_validated_typed_result_keeps_date_and_validator_changes_without_reexecution(self) -> None:
        value = await envelope(typed=True)
        self.assertEqual(value["completed"]["output"]["result"], {"label": "HELLO", "day": "2026-10-01"})
        native_args = [
            part["args"]
            for item in value["history"]["messages"]
            for part in item["parts"]
            if part["part_kind"] == "tool-call"
        ][-1]
        self.assertNotEqual(native_args, value["completed"]["output"]["result"])
        self.assertEqual(json.loads(encode_state(value, graph_id="agent-one")), value)

    async def test_default_output_names_and_false_zero_null_or_string_results_keep_their_json_types(self) -> None:
        for scalar in (False, 0, None, "yes"):
            with self.subTest(scalar=scalar):

                def respond(_messages, info, scalar=scalar):
                    output = (
                        next((item for item in info.output_tools if item.name.endswith("_None")), info.output_tools[0])
                        if scalar is None
                        else info.output_tools[0]
                    )
                    return ModelResponse(
                        parts=[ToolCallPart(output.name, {"response": scalar}, tool_call_id="scalar-output")],
                        usage=RequestUsage(input_tokens=1, output_tokens=1),
                    )

                output_type = (
                    [int, type(None)] if scalar is None else Literal["yes"] if isinstance(scalar, str) else type(scalar)
                )
                result = await Agent(FunctionModel(respond), output_type=output_type).run("hello", run_id="run-one")
                value = await envelope(typed=True)
                value["history"] = dump_conversation(result.conversation)
                value["completed"]["output"]["result"] = result.output
                value["completed"]["output"]["usage"] = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
                encoded = json.loads(encode_state(value, graph_id="agent-one"))
                self.assertIs(type(encoded["completed"]["output"]["result"]), type(scalar))

    async def test_schema_retry_pairs_same_call_id_before_reuse_and_retains_all_usage(self) -> None:
        value = await envelope(retry=True)
        parts = [part for message in value["history"]["messages"] for part in message["parts"]]
        self.assertTrue(any(part.get("outcome") == "retried" for part in parts))
        self.assertEqual(sum(part["part_kind"] == "tool-call" for part in parts), 2)
        self.assertEqual(value["completed"]["output"]["usage"]["total_tokens"], 28)
        encode_state(value, graph_id="agent-one")

    async def test_conversation_usage_is_cumulative_but_receipt_usage_is_current_run_only(self) -> None:
        value = await envelope(prior=True)
        self.assertGreater(
            value["history"]["usage"]["input_tokens"], value["completed"]["output"]["usage"]["prompt_tokens"]
        )
        encode_state(value, graph_id="agent-one")
        broken = copy.deepcopy(value)
        native = broken["history"]["usage"]
        broken["completed"]["output"]["usage"] = {
            "prompt_tokens": native["input_tokens"],
            "completion_tokens": native["output_tokens"],
            "total_tokens": native["input_tokens"] + native["output_tokens"],
        }
        await self.reject(broken, reason="only its native run")
        broken = copy.deepcopy(value)
        broken["history"]["usage"]["input_tokens"] = value["completed"]["output"]["usage"]["prompt_tokens"]
        await self.reject(broken, reason="lost its cumulative usage")

    async def test_native_estimated_decimal_cost_is_preserved_without_overriding_plane_prices(self) -> None:
        native = ConversationTypeAdapter.validate_json(state_json(self.value["history"]), strict=True)
        native.usage.cost = Decimal("0.00125")
        self.value["history"] = dump_conversation(native)
        self.assertEqual(self.value["history"]["usage"]["cost"], "0.00125")
        encode_state(self.value, graph_id="agent-one")
        self.value["completed"]["output"]["usage"]["cost_usd"] = 0.00125
        await self.reject(self.value)

    async def test_only_missing_checkpoint_starts_without_a_version(self) -> None:
        store = self.store(b"", status=404, headers={})
        self.assertEqual(await store.load("thread-one", graph_id="agent-one"), (None, None))
        await store.save("thread-one", self.value, None, graph_id="agent-one")
        self.assertEqual(self.requests[-1].headers["if-none-match"], "*")

    async def test_strong_framework_and_etag_are_required_before_restoring_native_state(self) -> None:
        for etag in (None, "", "7", 'W/"7"', '"0"', '"07"', '"-7"', '"9223372036854775808"'):
            headers = {"X-Runkite-Checkpoint-Framework": FRAMEWORK}
            if etag is not None:
                headers["ETag"] = etag
            with self.assertRaises(CompletionDeliveryError):
                await self.store(state_json(self.value).encode(), headers=headers).load(
                    "thread-one", graph_id="agent-one"
                )
        for framework in (None, "openai-agents", "Pydantic-AI"):
            headers = {"ETag": '"7"'}
            if framework is not None:
                headers["X-Runkite-Checkpoint-Framework"] = framework
            with self.assertRaises(CompletionDeliveryError):
                await self.store(state_json(self.value).encode(), headers=headers).load(
                    "thread-one", graph_id="agent-one"
                )

        class InvalidVersionClient:
            async def get_versioned(_self, _thread):
                return state_json(self.value).encode(), '"٧"'

        with self.assertRaises(CompletionDeliveryError):
            await PydanticAIState(InvalidVersionClient()).load("thread-one", graph_id="agent-one")

    async def test_wrong_graph_framework_version_and_legacy_state_fail_closed(self) -> None:
        for key, values in {
            "v": [True, "1", 0, 2],
            "framework": [None, "openai-agents"],
            "graph_id": [None, "other-agent"],
        }.items():
            for invalid in values:
                value = copy.deepcopy(self.value)
                value[key] = invalid
                await self.reject(value)
        for key in ("history", "completed", "messages"):
            value = copy.deepcopy(self.value)
            del value[key]
            await self.reject(value)
        await self.reject({"v": 1, "messages": [{"role": "user", "content": "old generic state"}]})

    async def test_native_codec_cannot_drop_unknown_fields_or_change_numeric_types(self) -> None:
        for mutate in (
            lambda v: v["history"].update(unknown="lost"),
            lambda v: v["history"]["messages"][0].update(unknown="lost"),
            lambda v: v["history"]["messages"][0]["parts"][0].update(unknown="lost"),
            lambda v: v["history"]["usage"].update(input_tokens=True),
            lambda v: v["history"]["usage"].update(input_tokens=1.0),
        ):
            value = copy.deepcopy(self.value)
            mutate(value)
            await self.reject(value)

    async def test_partial_wrong_conversation_and_unmatched_native_tools_are_rejected(self) -> None:
        for mutate in (
            lambda v: v["history"]["messages"][-1].update(state="interrupted"),
            lambda v: v["history"]["messages"][-1].update(conversation_id="other-native-conversation"),
            lambda v: v["history"]["messages"][-1].update(run_id=None),
            lambda v: v["history"]["messages"][0]["parts"][0].update(content=["multimodal-shaped input"]),
        ):
            value = copy.deepcopy(self.value)
            mutate(value)
            await self.reject(value)
        value = await envelope(typed=True)
        value["history"]["messages"].pop()
        await self.reject(value)
        value = await envelope(typed=True)
        value["history"]["messages"][-1]["parts"][0]["tool_call_id"] = "wrong-call"
        await self.reject(value)
        value = await envelope(typed=True)
        value["history"]["messages"][-1]["parts"][0]["outcome"] = "interrupted"
        await self.reject(value)

    async def test_latest_and_within_turn_native_run_ids_cannot_borrow_another_receipt(self) -> None:
        value = await envelope(retry=True)
        responses = [message for message in value["history"]["messages"] if message["kind"] == "response"]
        responses[0]["run_id"] = "foreign-earlier-response"
        await self.reject(value)
        value = copy.deepcopy(self.value)
        value["history"]["messages"][-1]["run_id"] = "another-run"
        await self.reject(value)

    async def test_new_native_user_input_cannot_reuse_a_previous_typed_acknowledgment(self) -> None:
        value = await envelope(typed=True)
        native = ConversationTypeAdapter.validate_json(state_json(value["history"]))
        native.messages.append(ModelRequest(parts=[UserPromptPart("next input")]))
        value["history"] = dump_conversation(native)
        value["messages"].append({"role": "user", "content": "next input"})
        value["completed"]["input"] = state_json([value["messages"][-1]])
        value["completed"]["output"]["messages"] = copy.deepcopy(value["messages"])
        await self.reject(value)

    async def test_deferred_requests_and_unsupported_native_parts_cannot_hide_behind_success(self) -> None:
        from pydantic_ai import DeferredToolRequests

        native = ConversationTypeAdapter.validate_json(state_json(self.value["history"]), strict=True)
        native.deferred_tool_requests = DeferredToolRequests(
            calls=[ToolCallPart("external", {}, tool_call_id="deferred")]
        )
        self.value["history"] = dump_conversation(native)
        await self.reject(self.value)
        value = await envelope()
        value["history"]["messages"][-1]["parts"] = [
            {"part_kind": "native-tool-call", "tool_name": "web_search", "args": {}, "tool_call_id": "hosted"}
        ]
        await self.reject(value)

    async def test_native_text_and_caller_inputs_must_match_the_durable_plain_history(self) -> None:
        for mutate in (
            lambda v: v["history"]["messages"][-1]["parts"][0].update(content="other native answer"),
            lambda v: v["messages"][-1].update(content="other plain answer"),
            lambda v: v["completed"].update(text_events=["other replay text"]),
            lambda v: v["completed"].update(input='[{"role":"user","content":"other input"}]'),
            lambda v: v["completed"]["output"].update(messages=[]),
        ):
            value = copy.deepcopy(self.value)
            mutate(value)
            await self.reject(value)

    async def test_system_inputs_are_preserved_alongside_extra_factory_system_prompts(self) -> None:
        native = ConversationTypeAdapter.validate_json(state_json(self.value["history"]), strict=True)
        native.messages[0].parts.insert(1, SystemPromptPart("caller rule"))
        self.value["history"] = dump_conversation(native)
        self.value["messages"].insert(0, {"role": "system", "content": "caller rule"})
        self.value["completed"]["input"] = state_json(self.value["messages"][:-1])
        self.value["completed"]["output"]["messages"] = copy.deepcopy(self.value["messages"])
        encode_state(self.value, graph_id="agent-one")
        self.value["history"]["messages"][0]["parts"][1]["content"] = "wrong caller rule"
        await self.reject(self.value)

    async def test_thinking_and_business_metadata_are_retained_without_becoming_visible_text(self) -> None:
        native = ConversationTypeAdapter.validate_json(state_json(self.value["history"]), strict=True)
        native.messages[-1].parts.insert(
            0, ThinkingPart("native reasoning", signature="opaque-signature", provider_name="test")
        )
        native.messages[-1].metadata = {"document": {"type": "approval_required", "description": "business JSON"}}
        self.value["history"] = dump_conversation(native)
        encode_state(self.value, graph_id="agent-one")

    async def test_receipt_success_input_metadata_and_typed_result_ack_are_consistent(self) -> None:
        for key, values in {
            "status": [None, "error", "pending"],
            "run_id": [None, ""],
            "input": [None, "[]", '[{"role": "user", "content": "hello"}]'],
            "text_events": [[1], None],
        }.items():
            for invalid in values:
                value = copy.deepcopy(self.value)
                value["completed"][key] = invalid
                await self.reject(value)
        for key, invalid in (("generation", True), ("generation", -1), ("generation", 1 << 63), ("framework", "other")):
            value = copy.deepcopy(self.value)
            value["completed"]["output"]["pydantic_ai"][key] = invalid
            await self.reject(value)
        value = await envelope(typed=True)
        del value["completed"]["output"]["result"]
        await self.reject(value)
        self.value["completed"]["output"]["result"] = {"not": "native typed completion"}
        await self.reject(self.value)

    async def test_invalid_usage_and_missing_metering_never_become_success_receipts(self) -> None:
        for mutate in (
            lambda v: v["completed"]["output"].pop("usage"),
            lambda v: v["completed"]["output"]["usage"].update(prompt_tokens=True),
            lambda v: v["completed"]["output"]["usage"].update(prompt_tokens=-1),
            lambda v: v["completed"]["output"]["usage"].update(total_tokens=1),
            lambda v: v["completed"]["output"]["usage"].update(model=123),
            lambda v: v["completed"]["output"]["usage"].update(unmetered="yes"),
            lambda v: v["history"]["usage"].update(output_tokens=-1),
            lambda v: v["history"]["usage"].update(cost="NaN"),
        ):
            value = copy.deepcopy(self.value)
            mutate(value)
            await self.reject(value)

    async def test_duplicate_keys_nonfinite_values_invalid_unicode_and_lossy_python_are_rejected(self) -> None:
        body = state_json(self.value).encode()
        for raw in (
            body.replace(b'"v":1', b'"v":1,"v":1', 1),
            body.replace(b'"input_tokens":', b'"input_tokens":NaN,"other":', 1),
            body.replace(b'"Hello"', b'"\\ud800"', 1),
            b"\xff",
        ):
            with self.assertRaises(CompletionDeliveryError):
                await self.store(raw).load("thread-one", graph_id="agent-one")
        for invalid in ({1: "key"}, (1,), math.inf, "\ud800"):
            with self.assertRaises((ValueError, UnicodeError)):
                state_json(invalid)
        native = ConversationTypeAdapter.validate_json(state_json(self.value["history"]), strict=True)
        native.messages[-1].metadata = {"nonfinite": math.nan}
        with self.assertRaises(ValueError):
            dump_conversation(native)

    async def test_local_preflight_and_full_checkpoint_limit_prevent_a_recoverable_format_loop(self) -> None:
        value = copy.deepcopy(self.value)
        value["history"]["messages"][-1]["provider_details"] = {"large": "x" * (16 << 20)}
        with self.assertRaisesRegex(ValueError, "16 MiB"):
            encode_state(value, graph_id="agent-one")
        with self.assertRaises(CompletionDeliveryError):
            await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        value = copy.deepcopy(self.value)
        value["completed"]["text_events"] = ["x" * (1 << 20)] * 9
        await self.reject(value, reason="8 MiB")

    async def test_result_payload_and_replayed_message_limits_count_real_utf8_bytes(self) -> None:
        value = await envelope(typed=True)
        value["completed"]["output"]["result"] = {"large": "é" * 1_600_000}
        with self.assertRaisesRegex(ValueError, "output exceeds the 3 MiB"):
            encode_state(value, graph_id="agent-one")
        value = copy.deepcopy(self.value)
        text = "x" * (3 << 20)
        value["history"]["messages"][-1]["parts"][0]["content"] = text
        value["messages"][-1]["content"] = text
        value["completed"]["output"]["messages"] = copy.deepcopy(value["messages"])
        value["completed"]["text_events"] = [text]
        with self.assertRaisesRegex(ValueError, "message exceeds the 3 MiB"):
            encode_state(value, graph_id="agent-one")

    async def test_receipt_model_must_name_all_current_native_charged_responses(self) -> None:
        value = copy.deepcopy(self.value)
        value["completed"]["output"]["usage"]["model"] = "test"
        encode_state(value, graph_id="agent-one")
        value["completed"]["output"]["usage"]["model"] = "another-model"
        await self.reject(value)

    async def test_typed_application_fields_cannot_replace_control_plane_metadata(self) -> None:
        value = await envelope(typed=True)
        value["completed"]["output"]["result"] = {
            "usage": {"total_tokens": 0},
            "messages": [],
            "pydantic_ai": {"framework": "foreign"},
        }
        stored = json.loads(encode_state(value, graph_id="agent-one"))
        self.assertGreater(stored["completed"]["output"]["usage"]["total_tokens"], 0)
        self.assertEqual(stored["completed"]["output"]["pydantic_ai"]["framework"], FRAMEWORK)

    async def test_http_and_cas_failures_withhold_confirmation_and_close_releases_http_client(self) -> None:
        for status in (401, 403, 412, 500):
            with self.assertRaises(CompletionDeliveryError):
                await self.store(b"", status=status).load("thread-one", graph_id="agent-one")
            with self.assertRaises(CompletionDeliveryError):
                await self.store(b"", put_status=status).save("thread-one", self.value, None, graph_id="agent-one")
        store = self.store(state_json(self.value).encode())
        await store.load("thread-one", graph_id="agent-one")
        client = store._client._client
        await store.aclose()
        self.assertTrue(client.is_closed)
        await store.aclose()

    async def test_trusted_tenant_runner_and_attempt_binding_reach_the_checkpoint(self) -> None:
        tenant = bind_tenant("tenant-one")
        run = bind_run("run-bound", 9)
        runner = bind_runner("python-pydantic-ai", "synthetic-token")
        try:
            store = self.store(state_json(self.value).encode())
            value, etag = await store.load("thread/escaped", graph_id="agent-one")
            await store.save("thread/escaped", value, etag, graph_id="agent-one")
        finally:
            reset_runner(runner)
            reset_run(run)
            reset_tenant(tenant)
        for request in self.requests:
            self.assertEqual(request.headers["x-runkite-tenant-id"], "tenant-one")
            self.assertEqual(request.headers["x-runkite-generation"], "9")
            self.assertEqual(request.headers["x-runner-token"], "synthetic-token")
            self.assertIn("thread%2Fescaped/adapter-state", str(request.url))


if __name__ == "__main__":
    unittest.main()
