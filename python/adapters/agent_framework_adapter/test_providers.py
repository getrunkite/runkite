"""Native Microsoft provider HTTP/SSE tests with no external requests."""

from __future__ import annotations

import asyncio
import json
import sys
import unittest
from pathlib import Path

import httpx
from agent_framework import Agent, FunctionTool
from agent_framework.openai import OpenAIChatClient, OpenAIChatCompletionClient
from openai import AsyncOpenAI
from openai.types.chat import ChatCompletionChunk
from openai.types.responses import (
    Response,
    ResponseCompletedEvent,
    ResponseCreatedEvent,
    ResponseFunctionCallArgumentsDeltaEvent,
    ResponseFunctionCallArgumentsDoneEvent,
    ResponseFunctionToolCall,
    ResponseOutputItemAddedEvent,
    ResponseOutputItemDoneEvent,
    ResponseOutputMessage,
    ResponseOutputText,
    ResponseTextDeltaEvent,
    ResponseTextDoneEvent,
    ResponseUsage,
)

try:
    import httpx2 as provider_http
except ImportError:
    # OpenAI 2 uses httpx; OpenAI 3 uses its separately distributed httpx2.
    provider_http = httpx

ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402
from test_adapter import FakePlane  # noqa: E402

from agent_framework_adapter.adapter import AgentFrameworkAdapter  # noqa: E402


def _sse(values: list[dict], *, typed_events: bool) -> bytes:
    frames = []
    for value in values:
        prefix = f"event: {value['type']}\n" if typed_events else ""
        frames.append(prefix + "data: " + json.dumps(value, separators=(",", ":")) + "\n\n")
    return ("".join(frames) + "data: [DONE]\n\n").encode()


def _responses_stream(*, model: str, text: str | None = None, call_id: str | None = None) -> bytes:
    output = []
    if text is not None:
        output.append(
            ResponseOutputMessage(
                id="wire-message",
                type="message",
                role="assistant",
                status="completed",
                content=[ResponseOutputText(type="output_text", text=text, annotations=[], logprobs=[])],
            )
        )
    if call_id:
        output.append(
            ResponseFunctionToolCall(
                id=f"{call_id}-item",
                type="function_call",
                call_id=call_id,
                name="lookup",
                arguments='{"query":"Runkite"}',
                status="completed",
            )
        )
    response = Response(
        id="wire-response",
        object="response",
        created_at=0,
        model=model,
        status="completed",
        output=output,
        tool_choice="auto",
        tools=[],
        parallel_tool_calls=False,
        usage=ResponseUsage(
            input_tokens=10,
            output_tokens=4,
            total_tokens=14,
            input_tokens_details={"cached_tokens": 0, "cache_write_tokens": 0},
            output_tokens_details={"reasoning_tokens": 0},
        ),
    )
    created = response.model_copy(update={"output": [], "status": "in_progress", "usage": None})
    events = [
        ResponseCreatedEvent(type="response.created", response=created, sequence_number=0),
    ]
    sequence = 1
    for output_index, item in enumerate(output):
        function = isinstance(item, ResponseFunctionToolCall)
        added_item = item.model_copy(
            update={"status": "in_progress", **({"arguments": ""} if function else {"content": []})}
        )
        events.append(
            ResponseOutputItemAddedEvent(
                type="response.output_item.added",
                item=added_item,
                output_index=output_index,
                sequence_number=sequence,
            )
        )
        if function:
            events.extend(
                [
                    ResponseFunctionCallArgumentsDeltaEvent(
                        type="response.function_call_arguments.delta",
                        item_id=item.id,
                        output_index=output_index,
                        delta=item.arguments,
                        sequence_number=sequence + 1,
                    ),
                    ResponseFunctionCallArgumentsDoneEvent(
                        type="response.function_call_arguments.done",
                        item_id=item.id,
                        output_index=output_index,
                        arguments=item.arguments,
                        sequence_number=sequence + 2,
                    ),
                ]
            )
        else:
            events.extend(
                [
                    ResponseTextDeltaEvent(
                        type="response.output_text.delta",
                        item_id=item.id,
                        output_index=output_index,
                        content_index=0,
                        delta=text,
                        sequence_number=sequence + 1,
                        logprobs=[],
                    ),
                    ResponseTextDoneEvent(
                        type="response.output_text.done",
                        item_id=item.id,
                        output_index=output_index,
                        content_index=0,
                        text=text,
                        sequence_number=sequence + 2,
                        logprobs=[],
                    ),
                ]
            )
        events.append(
            ResponseOutputItemDoneEvent(
                type="response.output_item.done",
                item=item,
                output_index=output_index,
                sequence_number=sequence + 3,
            )
        )
        sequence += 4
    events.append(ResponseCompletedEvent(type="response.completed", response=response, sequence_number=sequence))
    return _sse([event.model_dump(mode="json", exclude_unset=True) for event in events], typed_events=True)


def _chat_stream(*, model: str, text: str | None = None, call_id: str | None = None) -> bytes:
    delta = {"role": "assistant"}
    if call_id:
        delta["tool_calls"] = [
            {
                "index": 0,
                "id": call_id,
                "type": "function",
                "function": {"name": "lookup", "arguments": '{"query":"Runkite"}'},
            }
        ]
    else:
        delta["content"] = text
    common = {"id": "wire-response", "object": "chat.completion.chunk", "created": 0, "model": model}
    chunks = [
        ChatCompletionChunk(**common, choices=[{"index": 0, "delta": delta, "finish_reason": None}]),
        ChatCompletionChunk(
            **common, choices=[{"index": 0, "delta": {}, "finish_reason": "tool_calls" if call_id else "stop"}]
        ),
        ChatCompletionChunk(
            **common, choices=[], usage={"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14}
        ),
    ]
    return _sse([chunk.model_dump(mode="json", exclude_unset=True) for chunk in chunks], typed_events=False)


class WaitingProviderStream(provider_http.AsyncByteStream):
    def __init__(self) -> None:
        self.entered = asyncio.Event()
        self.closed = asyncio.Event()

    async def __aiter__(self):
        self.entered.set()
        await asyncio.Event().wait()
        yield b""

    async def aclose(self) -> None:
        self.closed.set()


class MicrosoftProviderWireTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.plane = FakePlane()
        self.adapter = AgentFrameworkAdapter()
        self.adapter.configure(http_address="http://plane", runner_token="synthetic-runner-token")
        state = self.adapter._state

        def make_state():
            store = state()
            store._client._transport = self.plane.transport()
            self.addAsyncCleanup(store.aclose)
            return store

        self.adapter._state = make_state
        self.provider_requests: list[dict] = []
        self.tool_usage: list[dict | None] = []
        self.latest_usage: dict | None = None

    def install_provider(self, kind: str, replies: list[bytes | int | WaitingProviderStream]) -> None:
        expected_path = "/v1/responses" if kind == "responses" else "/v1/chat/completions"

        def respond(request):
            self.assertEqual(request.method, "POST")
            self.assertEqual(request.url.host, "provider.invalid")
            self.assertEqual(request.url.path, expected_path)
            self.assertEqual(request.headers["authorization"], "Bearer synthetic-provider-key")
            body = json.loads(request.content)
            self.assertTrue(body["stream"])
            self.assertEqual(body["model"], "configured-alias")
            index = len(self.provider_requests)
            self.provider_requests.append(body)
            self.assertLess(index, len(replies), "unexpected native model retry")
            reply = replies[index]
            if isinstance(reply, int):
                return provider_http.Response(
                    reply, json={"error": {"message": "synthetic provider failure", "type": "test_error"}}
                )
            if isinstance(reply, WaitingProviderStream):
                return provider_http.Response(200, stream=reply, headers={"content-type": "text/event-stream"})
            return provider_http.Response(200, content=reply, headers={"content-type": "text/event-stream"})

        client = AsyncOpenAI(
            api_key="synthetic-provider-key",
            base_url="https://provider.invalid/v1",
            max_retries=0,
            http_client=provider_http.AsyncClient(transport=provider_http.MockTransport(respond), trust_env=False),
        )
        self.addAsyncCleanup(client.close)

        def lookup(query: str) -> str:
            self.tool_usage.append(dict(self.latest_usage) if self.latest_usage is not None else None)
            return "a synthetic local tool fact"

        def factory(context):
            provider = OpenAIChatClient if kind == "responses" else OpenAIChatCompletionClient
            return Agent(
                client=provider(model="configured-alias", async_client=client),
                tools=[FunctionTool(func=lookup, name="lookup", description="Return a local synthetic fact")],
                default_options={"store": False},
            )

        self.adapter.factories["wire-agent"] = factory

    async def execute(
        self, *, cancel: asyncio.Event | None = None, extra: dict | None = None
    ) -> tuple[str, list[dict]]:
        events = []

        async def send(event):
            events.append(event)
            if event["method"] == "values" and "usage" in event["data"]:
                self.latest_usage = event["data"]["usage"]

        tenant = bind_tenant("wire-tenant")
        run = bind_run("wire-run", 1)
        try:
            status = await self.adapter.execute(
                {
                    "run_id": "wire-run",
                    "thread_id": "wire-thread",
                    "graph_id": "wire-agent",
                    "tenant_id": "wire-tenant",
                    "generation": 1,
                    "input": {"messages": [{"role": "user", "content": "question"}]},
                    **(extra or {}),
                },
                send,
                cancel if cancel is not None else asyncio.Event(),
            )
        finally:
            reset_run(run)
            reset_tenant(tenant)
        return status, events

    @staticmethod
    def final_usage(events: list[dict]) -> dict:
        return [event["data"]["usage"] for event in events if event["method"] == "values"][-1]

    async def assert_known_native_model(self, kind: str) -> None:
        wire = _responses_stream if kind == "responses" else _chat_stream
        self.install_provider(
            kind, [wire(model="native-model", call_id="call-one"), wire(model="native-model", text="native answer")]
        )
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(
            self.tool_usage,
            [{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "native-model"}],
        )
        self.assertEqual(
            self.final_usage(events),
            {"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28, "model": "native-model"},
        )
        self.assertEqual(self.plane.puts, 1)
        saved = json.loads(next(iter(self.plane.blobs.values())))
        self.assertEqual(saved["completed"]["output"]["usage"], self.final_usage(events))
        self.assertEqual(len(self.provider_requests), 2)

    async def test_responses_native_model_prices_each_tool_step_before_its_body(self) -> None:
        await self.assert_known_native_model("responses")

    async def test_chat_completion_native_model_prices_each_tool_step_before_its_body(self) -> None:
        await self.assert_known_native_model("chat")

    async def assert_native_tool_allowlist(self, kind: str) -> None:
        wire = _responses_stream if kind == "responses" else _chat_stream
        self.install_provider(
            kind, [wire(model="native-model", call_id="call-one"), wire(model="native-model", text="native answer")]
        )
        status, events = await self.execute(extra={"allowed_tools": ["lookup"]})
        self.assertEqual(status, "success")
        self.assertEqual(
            self.tool_usage,
            [{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "native-model"}],
        )
        self.assertEqual(
            self.final_usage(events),
            {"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28, "model": "native-model"},
        )
        self.assertEqual(len(self.provider_requests), 2)
        self.assertEqual(self.plane.puts, 1)
        saved = json.loads(next(iter(self.plane.blobs.values())))
        self.assertEqual(saved["completed"]["output"]["usage"], self.final_usage(events))
        self.assertFalse(any(event["method"] == "tool_auth" for event in events))

    async def test_responses_configured_allowlist_runs_permitted_native_function(self) -> None:
        await self.assert_native_tool_allowlist("responses")

    async def test_chat_completion_configured_allowlist_runs_permitted_native_function(self) -> None:
        await self.assert_native_tool_allowlist("chat")

    async def assert_native_tool_denied(self, kind: str) -> None:
        wire = _responses_stream if kind == "responses" else _chat_stream
        for allowed in ([], ["other"]):
            with self.subTest(allowed=allowed):
                self.provider_requests.clear()
                self.tool_usage.clear()
                self.latest_usage = None
                self.install_provider(kind, [wire(model="native-model", call_id="call-one")])
                with self.assertLogs(level="ERROR"):
                    status, events = await self.execute(extra={"allowed_tools": allowed})
                self.assertEqual(status, "error")
                self.assertFalse(self.tool_usage, "A rejected native function must never execute")
                self.assertEqual(len(self.provider_requests), 1, "A denial must stop further provider calls")
                self.assertEqual(
                    self.final_usage(events),
                    {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "native-model"},
                )
                denials = [event["data"] for event in events if event["method"] == "tool_auth"]
                self.assertEqual(len(denials), 1)
                self.assertEqual(denials[0]["effect"], "deny")
                self.assertEqual(denials[0]["reason_code"], "tool_not_allowed")
                self.assertEqual(denials[0]["tool"], "lookup")
                self.assertEqual(denials[0]["generation"], 1)
                self.assertNotIn("args", denials[0])
                self.assertEqual(self.plane.puts, 0)
                self.assertFalse(self.plane.blobs)

    async def test_responses_configured_allowlist_rejects_native_function_before_body(self) -> None:
        await self.assert_native_tool_denied("responses")

    async def test_chat_completion_configured_allowlist_rejects_native_function_before_body(self) -> None:
        await self.assert_native_tool_denied("chat")

    async def test_responses_preamble_confirmation_and_next_request_reused_ids_do_not_duplicate_text(self) -> None:
        self.install_provider(
            "responses",
            [
                _responses_stream(model="native-model", text="Checking.", call_id="call-one"),
                _responses_stream(model="native-model", text="native answer"),
            ],
        )
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(self.plane.puts, 1)
        saved = json.loads(next(iter(self.plane.blobs.values())))
        text = saved["completed"]["output"]["messages"][-1]["content"]
        self.assertEqual(text.count("Checking."), 1)
        self.assertEqual(text.count("native answer"), 1)
        self.assertLess(text.index("Checking."), text.index("native answer"))
        self.assertEqual(
            self.tool_usage,
            [{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "native-model"}],
        )
        self.assertEqual(
            self.final_usage(events),
            {"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28, "model": "native-model"},
        )

    async def test_mixed_models_with_reused_response_ids_keep_aggregate_unpriced(self) -> None:
        self.install_provider(
            "responses",
            [
                _responses_stream(model="first-model", call_id="call-one"),
                _responses_stream(model="second-model", text="native answer"),
            ],
        )
        status, events = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(
            self.tool_usage, [{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "first-model"}]
        )
        self.assertEqual(self.final_usage(events), {"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28})
        saved = json.loads(next(iter(self.plane.blobs.values())))
        self.assertNotIn("model", saved["completed"]["output"]["usage"])

    async def test_provider_failure_after_tool_retains_only_known_completed_usage(self) -> None:
        self.install_provider("responses", [_responses_stream(model="native-model", call_id="call-one"), 503])
        with self.assertLogs(level="ERROR"):
            status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertEqual(
            self.final_usage(events),
            {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "native-model"},
        )
        self.assertEqual(len(self.tool_usage), 1)
        self.assertEqual(self.plane.puts, 0)
        self.assertFalse(self.plane.blobs)

    async def test_cancel_during_next_provider_stream_retains_first_priced_usage_and_closes_transport(self) -> None:
        waiting = WaitingProviderStream()
        self.install_provider("responses", [_responses_stream(model="native-model", call_id="call-one"), waiting])
        cancel = asyncio.Event()
        task = asyncio.create_task(self.execute(cancel=cancel))

        async def cleanup_task():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)

        self.addAsyncCleanup(cleanup_task)
        await asyncio.wait_for(waiting.entered.wait(), 3)
        cancel.set()
        status, events = await asyncio.wait_for(task, 3)
        self.assertEqual(status, "interrupted")
        self.assertEqual(
            self.final_usage(events),
            {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "native-model"},
        )
        self.assertTrue(waiting.closed.is_set())
        self.assertEqual(len(self.tool_usage), 1)
        self.assertEqual(self.plane.puts, 0)


if __name__ == "__main__":
    unittest.main()
