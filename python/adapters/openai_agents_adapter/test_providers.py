"""Exercise native provider HTTP/SSE codecs without any external requests."""

from __future__ import annotations

import asyncio
import json
import sys
import unittest
from pathlib import Path

import httpx2
from agents import Agent, OpenAIChatCompletionsModel, OpenAIResponsesModel, function_tool
from openai import AsyncOpenAI
from openai.types.chat import ChatCompletionChunk
from openai.types.responses import (
    Response,
    ResponseCompletedEvent,
    ResponseCreatedEvent,
    ResponseOutputItemAddedEvent,
    ResponseOutputItemDoneEvent,
    ResponseTextDeltaEvent,
    ResponseUsage,
)

ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402
from test_adapter import FakePlane, message, tool_call  # noqa: E402

from openai_agents_adapter.adapter import OpenAIAgentsAdapter  # noqa: E402


def _sse(values: list[dict], *, typed_events: bool) -> bytes:
    frames = []
    for value in values:
        prefix = f"event: {value['type']}\n" if typed_events else ""
        frames.append(prefix + "data: " + json.dumps(value, separators=(",", ":")) + "\n\n")
    frames.append("data: [DONE]\n\n")
    return "".join(frames).encode()


def _responses_stream(*, identity: str, text: str | None = None, call_id: str | None = None) -> bytes:
    item = tool_call(identity=call_id) if call_id else message(text, identity=f"{identity}-message")
    response = Response(
        id=identity,
        object="response",
        created_at=0,
        model="wire-model",
        status="completed",
        output=[item],
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
        ResponseOutputItemAddedEvent(type="response.output_item.added", item=item, output_index=0, sequence_number=1),
    ]
    if text is not None:
        events.append(
            ResponseTextDeltaEvent(
                type="response.output_text.delta",
                item_id=item.id,
                output_index=0,
                content_index=0,
                delta=text,
                sequence_number=2,
                logprobs=[],
            )
        )
    events.extend(
        [
            ResponseOutputItemDoneEvent(type="response.output_item.done", item=item, output_index=0, sequence_number=3),
            ResponseCompletedEvent(type="response.completed", response=response, sequence_number=4),
        ]
    )
    return _sse([event.model_dump(mode="json", exclude_unset=True) for event in events], typed_events=True)


def _chat_stream(*, identity: str, text: str | None = None, call_id: str | None = None) -> bytes:
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
    common = {"id": identity, "object": "chat.completion.chunk", "created": 0, "model": "wire-model"}
    chunks = [
        ChatCompletionChunk(
            **common,
            choices=[{"index": 0, "delta": delta, "finish_reason": None}],
        ),
        ChatCompletionChunk(
            **common,
            choices=[{"index": 0, "delta": {}, "finish_reason": "tool_calls" if call_id else "stop"}],
        ),
        ChatCompletionChunk(
            **common,
            choices=[],
            usage={"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14},
        ),
    ]
    return _sse([chunk.model_dump(mode="json", exclude_unset=True) for chunk in chunks], typed_events=False)


class ProviderWireTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.plane = FakePlane()
        self.adapter = OpenAIAgentsAdapter()
        self.adapter.configure(http_address="http://plane", runner_token="synthetic-runner-token")
        state = self.adapter._state

        def make_state():
            store = state()
            store._client._transport = self.plane.transport()
            self.addAsyncCleanup(store.aclose)
            return store

        self.adapter._state = make_state
        self.provider_requests: list[dict] = []
        self.tool_calls: list[str] = []
        self.tool_observed_usage: list[dict | None] = []
        self.latest_usage: dict | None = None

    def install_provider(self, kind: str, replies: list[bytes | int]) -> None:
        expected_path = "/v1/responses" if kind == "responses" else "/v1/chat/completions"

        def respond(request: httpx2.Request) -> httpx2.Response:
            self.assertEqual(request.method, "POST")
            self.assertEqual(request.url.host, "provider.invalid")
            self.assertEqual(request.url.path, expected_path)
            self.assertEqual(request.headers["authorization"], "Bearer synthetic-provider-key")
            body = json.loads(request.content)
            self.assertTrue(body["stream"])
            self.assertEqual(body["model"], "wire-model")
            index = len(self.provider_requests)
            self.provider_requests.append(body)
            self.assertLess(index, len(replies), "unexpected native model retry or extra request")
            reply = replies[index]
            if isinstance(reply, int):
                return httpx2.Response(
                    reply, json={"error": {"message": "synthetic provider failure", "type": "test_error"}}
                )
            return httpx2.Response(200, content=reply, headers={"content-type": "text/event-stream"})

        client = AsyncOpenAI(
            api_key="synthetic-provider-key",
            base_url="https://provider.invalid/v1",
            max_retries=0,
            http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(respond), trust_env=False),
        )
        self.addAsyncCleanup(client.close)

        @function_tool
        async def lookup(query: str) -> str:
            self.tool_calls.append(query)
            self.tool_observed_usage.append(dict(self.latest_usage) if self.latest_usage is not None else None)
            return "a synthetic local tool fact"

        def factory(context):
            provider = OpenAIResponsesModel if kind == "responses" else OpenAIChatCompletionsModel
            return Agent(
                name="native HTTP codec", model=provider(model="wire-model", openai_client=client), tools=[lookup]
            )

        self.adapter.factories["wire-agent"] = factory

    async def execute(self, run_id: str, text: str, *, generation: int = 1) -> tuple[str, list[dict]]:
        events = []
        self.latest_usage = None

        async def send(event):
            events.append(event)
            if event["method"] == "values" and "usage" in event["data"]:
                self.latest_usage = event["data"]["usage"]

        tenant = bind_tenant("wire-tenant")
        run = bind_run(run_id, generation)
        try:
            status = await self.adapter.execute(
                {
                    "run_id": run_id,
                    "thread_id": "wire-thread",
                    "graph_id": "wire-agent",
                    "tenant_id": "wire-tenant",
                    "generation": generation,
                    "input": {"messages": [{"role": "user", "content": text}]},
                },
                send,
                asyncio.Event(),
            )
        finally:
            reset_run(run)
            reset_tenant(tenant)
        return status, events

    @staticmethod
    def values(events: list[dict]) -> dict:
        return [event["data"] for event in events if event["method"] == "values"][-1]

    async def assert_two_turns(self, kind: str) -> None:
        wire = _responses_stream if kind == "responses" else _chat_stream
        self.install_provider(
            kind,
            [
                wire(identity="provider-tool", call_id="wire-call-one"),
                wire(identity="provider-answer-one", text="First native answer"),
                wire(identity="provider-answer-two", text="Second native answer"),
            ],
        )
        first_status, first_events = await self.execute("wire-run-one", "first question")
        self.assertEqual(first_status, "success")
        self.assertEqual(self.tool_calls, ["Runkite"])
        self.assertEqual(
            self.tool_observed_usage,
            [{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "wire-model"}],
        )
        self.assertEqual(
            self.values(first_events)["usage"],
            {"prompt_tokens": 20, "completion_tokens": 8, "total_tokens": 28, "model": "wire-model"},
        )
        first_state = json.loads(next(iter(self.plane.blobs.values())))
        self.assertEqual(first_state["completed"]["run_id"], "wire-run-one")
        self.assertTrue(any(item.get("type") == "function_call_output" for item in first_state["history"]))
        if kind == "chat":
            generated = [item for item in first_state["history"] if item.get("type") in ("message", "function_call")]
            self.assertTrue(generated)
            self.assertTrue(all(item["provider_data"]["model"] == "wire-model" for item in generated))
        second_status, second_events = await self.execute("wire-run-two", "second question")
        self.assertEqual(second_status, "success")
        self.assertEqual(
            self.values(second_events)["messages"],
            [
                {"role": "user", "content": "first question"},
                {"role": "assistant", "content": "First native answer"},
                {"role": "user", "content": "second question"},
                {"role": "assistant", "content": "Second native answer"},
            ],
        )
        self.assertEqual(
            self.values(second_events)["usage"],
            {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "wire-model"},
        )
        self.assertEqual(len(self.provider_requests), 3)
        self.assertEqual(self.tool_calls, ["Runkite"])
        second_body = self.provider_requests[-1]
        native_input = second_body["input"] if kind == "responses" else second_body["messages"]
        self.assertIn("first question", json.dumps(native_input))
        self.assertIn("First native answer", json.dumps(native_input))
        self.assertIn("second question", json.dumps(native_input))
        self.assertIn("a synthetic local tool fact", json.dumps(native_input))
        committed = json.loads(next(iter(self.plane.blobs.values())))
        self.assertEqual(committed["completed"]["run_id"], "wire-run-two")
        self.assertEqual(self.plane.puts, 2)

    async def test_responses_provider_http_stream_supports_tools_and_native_two_turn_history(self) -> None:
        await self.assert_two_turns("responses")

    async def test_chat_completions_provider_http_stream_supports_tools_and_native_two_turn_history(self) -> None:
        await self.assert_two_turns("chat")

    async def test_native_http_auth_and_second_step_failures_never_commit_false_success(self) -> None:
        self.install_provider("responses", [401])
        with self.assertLogs(level="ERROR"):
            status, events = await self.execute("wire-auth-failure", "first question")
        self.assertEqual(status, "error")
        self.assertTrue(any(event["method"] == "error" for event in events))
        self.assertFalse(self.plane.blobs)
        self.assertTrue(self.values(events)["usage"]["unmetered"])
        self.assertFalse(self.tool_calls)

        self.provider_requests.clear()
        self.install_provider("responses", [_responses_stream(identity="provider-tool", call_id="wire-call-one"), 503])
        with self.assertLogs(level="ERROR"):
            status, events = await self.execute("wire-second-step-failure", "second question")
        self.assertEqual(status, "error")
        self.assertTrue(any(event["method"] == "error" for event in events))
        self.assertEqual(
            self.values(events)["usage"],
            {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "wire-model"},
        )
        self.assertFalse(self.plane.blobs)
        self.assertEqual(self.plane.puts, 0)
        self.assertEqual(self.tool_calls, ["Runkite"])
        self.assertEqual(
            self.tool_observed_usage,
            [{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": "wire-model"}],
        )


if __name__ == "__main__":
    unittest.main()
