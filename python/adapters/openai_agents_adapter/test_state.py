"""Native OpenAI history and receipts must be safe before execution or replay."""

from __future__ import annotations

import copy
import json
import math
import sys
import unittest
from pathlib import Path

import httpx

ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

from agents import Agent, ToolOutputText  # noqa: E402
from agents.items import ItemHelpers, MessageOutputItem, ReasoningItem, ToolCallItem, ToolCallOutputItem  # noqa: E402
from openai.types.responses import (  # noqa: E402
    ResponseFunctionToolCall,
    ResponseOutputMessage,
    ResponseOutputRefusal,
    ResponseOutputText,
    ResponseReasoningItem,
)
from runkite_runner.completion import CompletionDeliveryError  # noqa: E402
from runkite_runner.opaque_checkpoint import OpaqueCheckpointClient  # noqa: E402
from runkite_runner.tenant_ctx import (  # noqa: E402
    bind_run,
    bind_runner,
    bind_tenant,
    reset_run,
    reset_runner,
    reset_tenant,
)

from openai_agents_adapter.state import FRAMEWORK, OpenAIAgentsState, encode_state, state_json  # noqa: E402


def envelope(*, tools: bool = False, reasoning: bool = False, refusal: bool = False) -> dict:
    """Use the public SDK item codecs, including optional native metadata."""
    agent = Agent(name="local")
    text = "I cannot do that" if refusal else "Hello"
    messages = [{"role": "user", "content": "hello"}, {"role": "assistant", "content": text}]
    history = [copy.deepcopy(messages[0])]
    if reasoning:
        history.append(
            ReasoningItem(
                agent=agent,
                raw_item=ResponseReasoningItem(
                    id="rs-one",
                    type="reasoning",
                    summary=[{"type": "summary_text", "text": "Choose the local function"}],
                    encrypted_content="opaque-native-reasoning",
                    status="completed",
                ),
            ).to_input_item()
        )
    if tools:
        history.extend(
            [
                ToolCallItem(
                    agent=agent,
                    raw_item=ResponseFunctionToolCall(
                        id="fc-one",
                        call_id="call-one",
                        name="echo",
                        arguments='{"text":"hello"}',
                        type="function_call",
                        status="completed",
                    ),
                ).to_input_item(),
                ToolCallOutputItem(
                    agent=agent,
                    raw_item={"type": "function_call_output", "call_id": "call-one", "output": "hello"},
                    output="hello",
                ).to_input_item(),
            ]
        )
    content = (
        ResponseOutputRefusal(type="refusal", refusal=text)
        if refusal
        else ResponseOutputText(type="output_text", text=text, annotations=[], logprobs=[])
    )
    history.append(
        MessageOutputItem(
            agent=agent,
            raw_item=ResponseOutputMessage(
                id="msg-one",
                type="message",
                role="assistant",
                status="completed",
                content=[content],
            ),
        ).to_input_item()
    )
    return {
        "v": 1,
        "framework": FRAMEWORK,
        "graph_id": "agent-one",
        "history": history,
        "messages": messages,
        "completed": {
            "run_id": "run-one",
            "status": "success",
            "input": state_json([messages[0]]),
            "text_events": [text[:2], text[2:]],
            "output": {
                "messages": copy.deepcopy(messages),
                "openai_agents": {"framework": FRAMEWORK, "generation": 1},
                "usage": {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14},
            },
        },
    }


class NativeStateTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.requests: list[httpx.Request] = []
        self.clients: list[OpaqueCheckpointClient] = []

    async def asyncTearDown(self) -> None:
        for client in self.clients:
            await client.aclose()

    def store(
        self,
        body: bytes,
        *,
        headers: dict[str, str] | None = None,
        status: int = 200,
        put_status: int = 204,
    ) -> OpenAIAgentsState:
        def respond(request: httpx.Request) -> httpx.Response:
            self.requests.append(request)
            if request.method == "GET":
                response_headers = {"ETag": '"7"', "X-Runkite-Checkpoint-Framework": FRAMEWORK}
                return httpx.Response(status, content=body, headers=response_headers if headers is None else headers)
            return httpx.Response(put_status)

        client = OpaqueCheckpointClient(
            http_base_url="http://plane",
            framework=FRAMEWORK,
            transport=httpx.MockTransport(respond),
        )
        self.clients.append(client)
        return OpenAIAgentsState(client)

    async def reject(self, value: object, *, reason: str | None = None) -> None:
        store = self.store(json.dumps(value, ensure_ascii=True).encode())
        with self.assertRaises(CompletionDeliveryError) as loaded:
            await store.load("thread-one", graph_id="agent-one")
        if reason is not None:
            self.assertIn(reason, str(loaded.exception.__cause__))
        before = len(self.requests)
        with self.assertRaises(CompletionDeliveryError) as saved:
            await store.save("thread-one", value, '"7"', graph_id="agent-one")
        if reason is not None:
            self.assertIn(reason, str(saved.exception.__cause__))
        self.assertEqual(len(self.requests), before, "invalid state must not be committed")

    async def test_native_sdk_text_reasoning_tools_and_refusals_round_trip_losslessly(self) -> None:
        for tools, reasoning, refusal in ((False, False, False), (True, True, False), (False, True, True)):
            with self.subTest(tools=tools, reasoning=reasoning, refusal=refusal):
                value = envelope(tools=tools, reasoning=reasoning, refusal=refusal)
                store = self.store(state_json(value).encode())
                loaded, etag = await store.load("thread-one", graph_id="agent-one")
                self.assertEqual(loaded, value)
                self.assertEqual(etag, '"7"')
                await store.save("thread-one", loaded, etag, graph_id="agent-one")
                self.assertEqual(json.loads(self.requests[-1].content), value)
                self.assertEqual(self.requests[-1].headers["if-match"], '"7"')

    async def test_intermediate_assistant_text_stays_one_plain_turn_with_native_tool_history(self) -> None:
        value = envelope(tools=True)
        first = copy.deepcopy(value["history"][-1])
        first["id"] = "msg-progress"
        first["content"][0]["text"] = "Checking. "
        value["history"].insert(1, first)
        value["messages"][-1]["content"] = "Checking. Hello"
        value["completed"]["text_events"].insert(0, "Checking. ")
        value["completed"]["output"]["messages"] = copy.deepcopy(value["messages"])
        loaded, _ = await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)

    async def test_multiple_turns_and_parallel_tools_preserve_the_full_native_input(self) -> None:
        value = envelope(tools=True)
        old_history = copy.deepcopy(value["history"])
        old_messages = copy.deepcopy(value["messages"])
        call = copy.deepcopy(value["history"][1])
        call["call_id"] = "call-parallel"
        call["id"] = "fc-parallel"
        result = {"type": "function_call_output", "call_id": "call-parallel", "output": "parallel"}
        value["history"].insert(2, call)
        value["history"].insert(4, result)
        for item in value["history"]:
            if item.get("call_id"):
                item["call_id"] += "-turn2"
        value["history"] = old_history + value["history"]
        value["messages"] = old_messages + value["messages"]
        value["completed"]["output"]["messages"] = copy.deepcopy(value["messages"])
        loaded, _ = await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)

    async def test_native_empty_final_assistant_is_distinct_from_a_missing_final_message(self) -> None:
        value = envelope()
        value["history"][-1]["content"] = []
        value["messages"].pop()
        value["completed"]["text_events"] = []
        value["completed"]["output"]["messages"] = copy.deepcopy(value["messages"])
        loaded, _ = await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)
        value["history"].pop()
        await self.reject(value)

    async def test_native_turn_must_start_with_user_input_and_finish_after_local_tools(self) -> None:
        value = envelope(reasoning=True)
        value["history"][0], value["history"][1] = value["history"][1], value["history"][0]
        await self.reject(value)
        value = envelope(tools=True)
        value["history"] = [value["history"][0], value["history"][-1], *value["history"][1:3]]
        await self.reject(value)
        value = envelope(tools=True)
        value["history"] = [*value["history"][1:3], value["history"][0], value["history"][-1]]
        await self.reject(value)

    async def test_only_a_missing_checkpoint_starts_without_an_etag(self) -> None:
        store = self.store(b"", headers={}, status=404)
        self.assertEqual(await store.load("thread-one", graph_id="agent-one"), (None, None))
        value = envelope()
        await store.save("thread-one", value, None, graph_id="agent-one")
        self.assertEqual(self.requests[-1].headers["if-none-match"], "*")
        self.assertNotIn("if-match", self.requests[-1].headers)

    async def test_missing_weak_noncanonical_or_out_of_range_etags_fail_before_work(self) -> None:
        for etag in (None, "", "7", 'W/"7"', '"0"', '"07"', '"-7"', '"+7"', '"9223372036854775808"', ' "7" '):
            with self.subTest(etag=etag):
                headers = {"X-Runkite-Checkpoint-Framework": FRAMEWORK}
                if etag is not None:
                    headers["ETag"] = etag
                store = self.store(state_json(envelope()).encode(), headers=headers)
                with self.assertRaises(CompletionDeliveryError):
                    await store.load("thread-one", graph_id="agent-one")
                before = len(self.requests)
                if etag is not None:
                    with self.assertRaises(CompletionDeliveryError):
                        await store.save("thread-one", envelope(), etag, graph_id="agent-one")
                    self.assertEqual(len(self.requests), before)

        class NonAsciiVersionClient:
            async def get_versioned(self, thread_id):
                return state_json(envelope()).encode(), '"٧"'

        with self.assertRaises(CompletionDeliveryError):
            await OpenAIAgentsState(NonAsciiVersionClient()).load("thread-one", graph_id="agent-one")

    async def test_http_framework_is_checked_before_decoding_an_otherwise_valid_body(self) -> None:
        for framework in (None, "agent-framework", "OpenAI-Agents", "openai-agents "):
            with self.subTest(framework=framework):
                headers = {"ETag": '"7"'}
                if framework is not None:
                    headers["X-Runkite-Checkpoint-Framework"] = framework
                with self.assertRaises(CompletionDeliveryError):
                    await self.store(state_json(envelope()).encode(), headers=headers).load(
                        "thread-one", graph_id="agent-one"
                    )

    async def test_version_framework_graph_and_envelope_shape_are_exact(self) -> None:
        for key, cases in {
            "v": (None, True, 0, 2, "1"),
            "framework": (None, "agent-framework", "OpenAI-Agents"),
            "graph_id": (None, "", "other-agent"),
            "history": (None, [], {}, ["not-native"]),
        }.items():
            for invalid in cases:
                with self.subTest(key=key, value=invalid):
                    value = envelope()
                    value[key] = invalid
                    await self.reject(value)
        for key in ("history", "completed", "messages"):
            value = envelope()
            del value[key]
            await self.reject(value)
        value = envelope()
        value["session"] = {"foreign": "Microsoft"}
        await self.reject(value)
        await self.reject({"v": 1, "messages": [{"role": "user", "content": "old generic state"}]})

    async def test_native_schema_errors_and_unknown_fields_are_not_silently_normalized(self) -> None:
        for mutate in (
            lambda item: item.update(status="incomplete"),
            lambda item: item.update(id=""),
            lambda item: item.update(unrecognized="must not be lost"),
            lambda item: item.update(content="not-output-content"),
            lambda item: item["content"][0].update(text=123),
            lambda item: item["content"][0].update(annotations=[{"type": "url_citation"}]),
            lambda item: item["content"][0].update(unknown=True),
        ):
            value = envelope()
            mutate(value["history"][-1])
            await self.reject(value)

    async def test_native_sdk_provider_metadata_round_trips_without_becoming_control_content(self) -> None:
        value = envelope(tools=True, reasoning=True)
        for item in value["history"][1:]:
            item["provider_data"] = {
                "model": "chat-model",
                "response_id": "native-response",
                "document": {"type": "mcp_approval_request", "description": "ordinary JSON metadata"},
            }
        store = self.store(state_json(value).encode())
        loaded, etag = await store.load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)
        await store.save("thread-one", loaded, etag, graph_id="agent-one")
        self.assertEqual(json.loads(self.requests[-1].content), value)

    async def test_native_sdk_provider_metadata_must_be_lossless_finite_json_object(self) -> None:
        for invalid in (None, [], "metadata", {"nonfinite": math.inf}, {"invalid_unicode": "\ud800"}):
            value = envelope(tools=True, reasoning=True)
            value["history"][1]["provider_data"] = invalid
            await self.reject(value)
        value = envelope()
        value["history"][-1]["provider_data"] = {"model": "chat-model"}
        value["history"][-1]["other_provider_extension"] = {"must": "not be dropped"}
        await self.reject(value)

    async def test_native_reasoning_is_preserved_but_malformed_or_partial_reasoning_is_rejected(self) -> None:
        for mutate in (
            lambda item: item.update(status="in_progress"),
            lambda item: item.update(summary=[{"type": "summary_text", "text": 123}]),
            lambda item: item.update(content=[{"type": "reasoning_text", "text": None}]),
            lambda item: item.update(encrypted_content={"opaque": "not a string"}),
            lambda item: item.update(id=""),
            lambda item: item.pop("summary"),
        ):
            value = envelope(reasoning=True)
            mutate(value["history"][1])
            await self.reject(value)

    async def test_malformed_native_refusal_cannot_be_replayed_as_a_text_answer(self) -> None:
        for invalid in (None, 123, {"not": "text"}):
            value = envelope(refusal=True)
            value["history"][-1]["content"][0]["refusal"] = invalid
            await self.reject(value)

    async def test_tool_result_business_json_is_not_interpreted_as_native_approval(self) -> None:
        value = envelope(tools=True)
        value["history"][2]["output"] = '{"type":"mcp_approval_request","description":"business JSON"}'
        loaded, _ = await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)

    async def test_public_native_text_tool_output_chunks_round_trip_without_stringification(self) -> None:
        for output in (ToolOutputText(text="hello"), [ToolOutputText(text="one"), ToolOutputText(text="two")]):
            with self.subTest(output=output):
                value = envelope(tools=True)
                call = ResponseFunctionToolCall.model_validate(value["history"][1])
                value["history"][2] = ItemHelpers.tool_call_output_item(call, output)
                self.assertIsInstance(value["history"][2]["output"], list)
                store = self.store(state_json(value).encode())
                loaded, etag = await store.load("thread-one", graph_id="agent-one")
                self.assertEqual(loaded, value)
                await store.save("thread-one", loaded, etag, graph_id="agent-one")
                self.assertEqual(json.loads(self.requests[-1].content), value)
        value = envelope(tools=True)
        value["history"][2]["output"] = []
        loaded, _ = await self.store(state_json(value).encode()).load("thread-one", graph_id="agent-one")
        self.assertEqual(loaded, value)

    async def test_native_tool_output_chunks_reject_nontext_and_malformed_content(self) -> None:
        for output in (
            [{"type": "input_text", "text": 123}],
            [{"type": "input_text"}],
            [{"type": "input_text", "text": "hello", "foreign": "lost"}],
            [{"type": "input_image", "image_url": "https://example.invalid/image.png", "detail": "auto"}],
            [{"type": "input_file", "file_id": "file-one"}],
        ):
            value = envelope(tools=True)
            value["history"][2]["output"] = output
            await self.reject(value)

    async def test_local_state_validation_distinguishes_deterministic_errors_from_delivery_failure(self) -> None:
        value = envelope(tools=True, reasoning=True)
        self.assertEqual(json.loads(encode_state(value, graph_id="agent-one")), value)
        value["history"].pop()
        with self.assertRaises(ValueError):
            encode_state(value, graph_id="agent-one")
        value = envelope(reasoning=True)
        value["history"][1]["encrypted_content"] = "x" * (16 << 20)
        with self.assertRaisesRegex(ValueError, "state exceeds the 16 MiB"):
            encode_state(value, graph_id="agent-one")

    async def test_approval_hosted_and_unknown_native_items_cannot_hide_behind_a_success_receipt(self) -> None:
        for native in (
            {
                "type": "mcp_approval_request",
                "id": "approval",
                "name": "tool",
                "arguments": "{}",
                "server_label": "mcp",
            },
            {"type": "mcp_approval_response", "approval_request_id": "approval", "approve": True},
            {"type": "mcp_call", "id": "mcp", "name": "tool", "arguments": "{}", "server_label": "mcp"},
            {"type": "item_reference", "id": "external-item"},
            {"type": "future_native_type", "id": "unknown"},
            {"role": "tool", "content": "foreign protocol"},
            {"role": "developer", "content": "outside the caller input contract"},
            {"role": "user", "content": [{"type": "input_image", "image_url": "https://example.invalid/image.png"}]},
            {"role": "assistant", "content": "not native assistant output"},
        ):
            with self.subTest(native=native):
                value = envelope()
                value["history"].insert(1, native)
                await self.reject(value)

    async def test_tool_calls_must_be_resolved_once_with_matching_ids_before_next_input(self) -> None:
        invalid_histories = []
        base = envelope(tools=True)["history"]
        invalid_histories.append([base[0], base[1], base[-1]])
        invalid_histories.append([base[0], base[2], base[-1]])
        invalid_histories.append([base[0], base[1], base[2], base[2], base[-1]])
        invalid_histories.append([base[0], base[1], base[1], base[2], base[-1]])
        invalid_histories.append([base[0], base[1], base[0], base[2], base[-1]])
        for history in invalid_histories:
            value = envelope(tools=True)
            value["history"] = copy.deepcopy(history)
            await self.reject(value)
        for index, updates in (
            (1, {"call_id": ""}),
            (1, {"name": ""}),
            (1, {"status": "in_progress"}),
            (1, {"async": True}),
            (1, {"arguments": {"invalid": "not a string"}}),
            (1, {"caller": {"type": "program", "tool_id": "provider"}}),
            (2, {"call_id": "another-call"}),
            (2, {"output": [{"type": "input_image", "image_url": "data:"}]}),
            (2, {"status": "incomplete"}),
        ):
            value = envelope(tools=True)
            value["history"][index].update(updates)
            await self.reject(value)

    async def test_plain_native_receipt_input_and_output_histories_must_all_agree(self) -> None:
        cases = []
        for change in (
            lambda value: value["history"][0].update(content="different native input"),
            lambda value: value["history"][-1]["content"][0].update(text="different native result"),
            lambda value: value["messages"][-1].update(content="different plain result"),
            lambda value: value["completed"].update(input='[{"role":"user","content":"other input"}]'),
            lambda value: value["completed"].update(text_events=["different receipt result"]),
            lambda value: value["completed"]["output"].update(messages=[]),
        ):
            value = envelope()
            change(value)
            cases.append(value)
        for value in cases:
            await self.reject(value)

    async def test_receipts_are_successful_complete_canonical_and_bound_to_framework(self) -> None:
        for key, cases in {
            "status": (None, "error", "interrupted", "pending"),
            "run_id": (None, "", 1),
            "input": (
                None,
                "not-json",
                "[]",
                '[{"role":"assistant","content":"Hello"}]',
                '[{"role":"user","content":"hello", "extra":true}]',
                '[{"role":"user","content":"hello","content":"hello"}]',
                '[{"role": "user", "content": "hello"}]',
            ),
            "text_events": (None, [1]),
            "output": (None, []),
        }.items():
            for invalid in cases:
                with self.subTest(key=key, value=invalid):
                    value = envelope()
                    value["completed"][key] = invalid
                    await self.reject(value)
        for key, invalid in (
            ("framework", "agent-framework"),
            ("generation", True),
            ("generation", -1),
            ("generation", 1 << 63),
        ):
            value = envelope()
            value["completed"]["output"]["openai_agents"][key] = invalid
            await self.reject(value)

    async def test_invalid_usage_is_not_replayed_or_committed(self) -> None:
        for usage in (
            {},
            {"prompt_tokens": 1, "completion_tokens": 2},
            {"prompt_tokens": True, "completion_tokens": 2, "total_tokens": 3},
            {"prompt_tokens": 1.0, "completion_tokens": 2, "total_tokens": 3},
            {"prompt_tokens": -1, "completion_tokens": 2, "total_tokens": 1},
            {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 4},
            {"prompt_tokens": 1 << 63, "completion_tokens": 0, "total_tokens": 1 << 63},
            {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3, "model": 123},
            {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3, "unmetered": "yes"},
            {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3, "cost_usd": -1},
            {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3, "cost_usd": True},
            {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3, "cost_usd": math.nan},
        ):
            with self.subTest(usage=usage):
                value = envelope()
                value["completed"]["output"]["usage"] = usage
                await self.reject(value)

    async def test_json_duplicate_keys_invalid_unicode_and_nonfinite_numbers_fail_closed(self) -> None:
        body = state_json(envelope()).encode()
        for raw in (
            body.replace(b'"v":1', b'"v":1,"v":1', 1),
            body.replace(b'"text":"Hello"', b'"text":"Hello","text":"Hello"', 1),
            body.replace(b'"total_tokens":14', b'"total_tokens":NaN', 1),
            body.replace(b'"total_tokens":14', b'"total_tokens":Infinity', 1),
            body.replace(b'"Hello"', b'"\\ud800"', 1),
            b"\xff",
        ):
            with self.assertRaises(CompletionDeliveryError):
                await self.store(raw).load("thread-one", graph_id="agent-one")

    async def test_lossy_python_values_cycles_and_invalid_unicode_never_reach_put(self) -> None:
        cycle: list = []
        cycle.append(cycle)
        for invalid in (
            {1: "integer-key"},
            {"tuple": (1,)},
            {"cycle": cycle},
            {"unicode": "\ud800"},
            {"float": math.inf},
        ):
            value = envelope()
            value["invalid"] = invalid
            store = self.store(b"", status=404)
            before = len(self.requests)
            with self.assertRaises(CompletionDeliveryError):
                await store.save("thread-one", value, None, graph_id="agent-one")
            self.assertEqual(len(self.requests), before)
            with self.assertRaises((ValueError, UnicodeError)):
                state_json(invalid)

    async def test_real_output_message_receipt_and_checkpoint_byte_limits_are_enforced(self) -> None:
        # A single delta includes the message wrapper in its event byte limit.
        value = envelope()
        text = "x" * (3 << 20)
        value["history"][-1]["content"][0]["text"] = text
        value["messages"][-1]["content"] = text
        value["completed"]["output"]["messages"] = copy.deepcopy(value["messages"])
        value["completed"]["text_events"] = [text]
        await self.reject(value, reason="message exceeds the 3 MiB")
        # Small deltas can still produce a values payload larger than the event cap.
        value["completed"]["text_events"] = ["x" * (1 << 20)] * 3
        await self.reject(value, reason="output exceeds the 3 MiB")
        value = envelope()
        value["completed"]["text_events"] = ["x" * (1 << 20)] * 9
        await self.reject(value, reason="receipt exceeds the 8 MiB")
        # Native reasoning is retained but cannot bypass the full checkpoint cap.
        value = envelope(reasoning=True)
        value["history"][1]["encrypted_content"] = "x" * (16 << 20)
        await self.reject(value, reason="state exceeds the 16 MiB")

    async def test_event_limits_count_utf8_bytes_rather_than_unicode_characters(self) -> None:
        value = envelope()
        text = "é" * 1_600_000
        self.assertLess(len(text), 3 << 20)
        value["history"][-1]["content"][0]["text"] = text
        value["messages"][-1]["content"] = text
        value["completed"]["output"]["messages"] = copy.deepcopy(value["messages"])
        value["completed"]["text_events"] = [text]
        # Encode with real UTF-8, matching the native store wire representation.
        store = self.store(state_json(value).encode())
        with self.assertRaises(CompletionDeliveryError) as loaded:
            await store.load("thread-one", graph_id="agent-one")
        self.assertIn("message exceeds the 3 MiB", str(loaded.exception.__cause__))
        with self.assertRaises(CompletionDeliveryError) as saved:
            await store.save("thread-one", value, None, graph_id="agent-one")
        self.assertIn("message exceeds the 3 MiB", str(saved.exception.__cause__))

    async def test_http_and_cas_failures_are_delivery_errors_and_never_false_success(self) -> None:
        for status in (401, 403, 412, 500):
            with self.subTest(status=status):
                store = self.store(state_json(envelope()).encode(), status=status)
                with self.assertRaises(CompletionDeliveryError):
                    await store.load("thread-one", graph_id="agent-one")
                store = self.store(b"", status=404, put_status=status)
                with self.assertRaises(CompletionDeliveryError):
                    await store.save("thread-one", envelope(), None, graph_id="agent-one")

        def fail(request):
            raise httpx.ConnectError("plane unavailable", request=request)

        client = OpaqueCheckpointClient(
            http_base_url="http://plane", framework=FRAMEWORK, transport=httpx.MockTransport(fail)
        )
        self.clients.append(client)
        store = OpenAIAgentsState(client)
        with self.assertRaises(CompletionDeliveryError):
            await store.load("thread-one", graph_id="agent-one")
        with self.assertRaises(CompletionDeliveryError):
            await store.save("thread-one", envelope(), None, graph_id="agent-one")

    async def test_checkpoint_requests_use_trusted_runner_tenant_attempt_and_graph_binding(self) -> None:
        tenant = bind_tenant("tenant-two")
        run = bind_run("run-bound", 9)
        runner = bind_runner("python-openai-agents", "test-runner-token")
        try:
            store = self.store(state_json(envelope()).encode())
            value, etag = await store.load("thread/escaped", graph_id="agent-one")
            await store.save("thread/escaped", value, etag, graph_id="agent-one")
        finally:
            reset_runner(runner)
            reset_run(run)
            reset_tenant(tenant)
        for request in self.requests:
            self.assertEqual(request.headers["x-runkite-tenant-id"], "tenant-two")
            self.assertEqual(request.headers["x-runkite-run-id"], "run-bound")
            self.assertEqual(request.headers["x-runkite-generation"], "9")
            self.assertEqual(request.headers["x-runner-kind"], "python-openai-agents")
            self.assertEqual(request.headers["x-runner-token"], "test-runner-token")
            self.assertEqual(request.headers["x-runkite-checkpoint-framework"], FRAMEWORK)
            self.assertIn("thread%2Fescaped/adapter-state", str(request.url))

    async def test_state_close_releases_and_can_reopen_the_client_without_changing_ownership(self) -> None:
        store = self.store(state_json(envelope()).encode())
        await store.load("thread-one", graph_id="agent-one")
        client = store._client._client
        self.assertFalse(client.is_closed)
        await store.aclose()
        self.assertTrue(client.is_closed)
        await store.aclose()
        value, etag = await store.load("thread-one", graph_id="agent-one")
        self.assertEqual(value, envelope())
        self.assertEqual(etag, '"7"')


if __name__ == "__main__":
    unittest.main()
