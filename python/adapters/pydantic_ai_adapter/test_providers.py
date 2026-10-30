"""Actual Pydantic AI provider codecs over synthetic HTTP/SSE transports."""

from __future__ import annotations

import asyncio
import json
import os
import sys
import unittest
from pathlib import Path

import httpx
import httpx2
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
from pydantic import BaseModel, Field
from pydantic_ai import Agent
from pydantic_ai.models.openai import OpenAIChatModel, OpenAIResponsesModel
from pydantic_ai.providers.openai import OpenAIProvider

os.environ.setdefault("PYDANTIC_AI_NO_BANNER", "1")
ROOT = Path(__file__).resolve().parents[2]
sys.path[:0] = [str(ROOT), str(ROOT / "adapters")]

from runkite_runner.tenant_ctx import bind_run, bind_tenant, reset_run, reset_tenant  # noqa: E402

from pydantic_ai_adapter.adapter import PydanticAIAdapter  # noqa: E402

MODEL = "gpt-4.1-mini"


def wire(
    kind: str, *, text: str | None = None, tool: str | None = None, args: dict | None = None, identity: str = "call-one"
) -> bytes:
    arguments = json.dumps(args or {}, separators=(",", ":"))
    if kind == "chat":
        delta = {"role": "assistant"}
        if tool:
            delta["tool_calls"] = [
                {"index": 0, "id": identity, "type": "function", "function": {"name": tool, "arguments": arguments}}
            ]
        else:
            delta["content"] = text
        common = {"id": "wire-response", "object": "chat.completion.chunk", "created": 0, "model": MODEL}
        events = [
            ChatCompletionChunk(**common, choices=[{"index": 0, "delta": delta, "finish_reason": None}]),
            ChatCompletionChunk(
                **common, choices=[{"index": 0, "delta": {}, "finish_reason": "tool_calls" if tool else "stop"}]
            ),
            ChatCompletionChunk(
                **common, choices=[], usage={"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14}
            ),
        ]
    else:
        item = (
            ResponseFunctionToolCall(
                id=identity + "-item",
                type="function_call",
                call_id=identity,
                name=tool,
                arguments=arguments,
                status="completed",
            )
            if tool
            else ResponseOutputMessage(
                id="wire-message",
                type="message",
                role="assistant",
                status="completed",
                content=[ResponseOutputText(type="output_text", text=text, annotations=[], logprobs=[])],
            )
        )
        response = Response(
            id="wire-response",
            object="response",
            created_at=0,
            model=MODEL,
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
        events = [
            ResponseCreatedEvent(
                type="response.created",
                response=response.model_copy(update={"output": [], "status": "in_progress", "usage": None}),
                sequence_number=0,
            ),
            ResponseOutputItemAddedEvent(
                type="response.output_item.added",
                item=item.model_copy(
                    update={"status": "in_progress", **({"arguments": ""} if tool else {"content": []})}
                ),
                output_index=0,
                sequence_number=1,
            ),
        ]
        if tool:
            events.extend(
                [
                    ResponseFunctionCallArgumentsDeltaEvent(
                        type="response.function_call_arguments.delta",
                        item_id=item.id,
                        output_index=0,
                        delta=arguments,
                        sequence_number=2,
                    ),
                    ResponseFunctionCallArgumentsDoneEvent(
                        type="response.function_call_arguments.done",
                        item_id=item.id,
                        output_index=0,
                        arguments=arguments,
                        sequence_number=3,
                    ),
                ]
            )
        else:
            events.extend(
                [
                    ResponseTextDeltaEvent(
                        type="response.output_text.delta",
                        item_id=item.id,
                        output_index=0,
                        content_index=0,
                        delta=text,
                        sequence_number=2,
                        logprobs=[],
                    ),
                    ResponseTextDoneEvent(
                        type="response.output_text.done",
                        item_id=item.id,
                        output_index=0,
                        content_index=0,
                        text=text,
                        sequence_number=3,
                        logprobs=[],
                    ),
                ]
            )
        events.extend(
            [
                ResponseOutputItemDoneEvent(
                    type="response.output_item.done", item=item, output_index=0, sequence_number=4
                ),
                ResponseCompletedEvent(type="response.completed", response=response, sequence_number=5),
            ]
        )
    frames = []
    for event in events:
        value = event.model_dump(mode="json", exclude_unset=True)
        prefix = f"event: {value['type']}\n" if kind != "chat" else ""
        frames.append(prefix + "data: " + json.dumps(value, separators=(",", ":")) + "\n\n")
    return ("".join(frames) + "data: [DONE]\n\n").encode()


class Answer(BaseModel):
    count: int = Field(strict=True)
    note: str = "validated"


class WaitingStream(httpx2.AsyncByteStream):
    def __init__(self):
        self.entered, self.closed = asyncio.Event(), asyncio.Event()

    async def __aiter__(self):
        self.entered.set()
        await asyncio.Event().wait()
        yield b""

    async def aclose(self):
        self.closed.set()


class ProviderWireTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.adapter = PydanticAIAdapter()
        self.adapter.configure(http_address="http://plane", runner_token="synthetic-runner-token")
        self.blobs, self.versions = {}, {}
        self.puts = 0
        self.requests, self.tool_usage = [], []
        self.latest_usage = None
        state = self.adapter._state

        def plane(request):
            key = (request.headers["x-runkite-tenant-id"], request.url.path)
            if request.method == "GET":
                if key not in self.blobs:
                    return httpx.Response(404)
                return httpx.Response(
                    200,
                    content=self.blobs[key],
                    headers={"ETag": f'"{self.versions[key]}"', "X-Runkite-Checkpoint-Framework": "pydantic-ai"},
                )
            if request.headers.get("if-none-match") == "*" and key in self.blobs:
                return httpx.Response(412)
            if "if-match" in request.headers and request.headers["if-match"] != f'"{self.versions.get(key)}"':
                return httpx.Response(412)
            self.puts += 1
            self.blobs[key] = request.content
            self.versions[key] = self.versions.get(key, 0) + 1
            return httpx.Response(204)

        def make_state():
            store = state()
            store._client._transport = httpx.MockTransport(plane)
            self.addAsyncCleanup(store.aclose)
            return store

        self.adapter._state = make_state

    def install(self, kind: str, replies: list, *, typed: bool = False):
        def respond(request):
            self.assertEqual(request.url.host, "provider.invalid")
            self.assertEqual(request.url.path, "/v1/chat/completions" if kind == "chat" else "/v1/responses")
            self.assertEqual(request.headers["authorization"], "Bearer synthetic-provider-key")
            body = json.loads(request.content)
            self.assertTrue(body["stream"])
            index = len(self.requests)
            self.requests.append(body)
            self.assertLess(index, len(replies), "unexpected native model request/retry")
            item = replies[index]
            if isinstance(item, int):
                return httpx2.Response(
                    item, json={"error": {"message": "synthetic provider failure", "type": "test_error"}}
                )
            if isinstance(item, WaitingStream):
                return httpx2.Response(200, stream=item, headers={"content-type": "text/event-stream"})
            return httpx2.Response(200, content=item, headers={"content-type": "text/event-stream"})

        client = AsyncOpenAI(
            api_key="synthetic-provider-key",
            base_url="https://provider.invalid/v1",
            max_retries=0,
            http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(respond), trust_env=False),
        )
        self.addAsyncCleanup(client.close)

        def factory(context):
            model_type = OpenAIChatModel if kind == "chat" else OpenAIResponsesModel
            agent = Agent(
                model_type(MODEL, provider=OpenAIProvider(openai_client=client)),
                output_type=Answer if typed else str,
                model_settings={"openai_store": False},
            )

            @agent.tool_plain
            def lookup(query: str) -> str:
                self.tool_usage.append(dict(self.latest_usage) if self.latest_usage is not None else None)
                return "local synthetic fact"

            return agent

        self.adapter.factories["wire-agent"] = factory

    async def execute(self, run_id="wire-run", *, text="question", cancel=None, allowed=None):
        events = []

        async def send(event):
            events.append(event)
            if event["method"] == "values" and "usage" in event["data"]:
                self.latest_usage = event["data"]["usage"]
            if event["method"] == "values" and "result" in event["data"]:
                self.assertGreater(self.puts, 0, "typed result must not publish before its durable commit")

        tenant, run = bind_tenant("wire-tenant"), bind_run(run_id, 1)
        try:
            assignment = {
                "run_id": run_id,
                "thread_id": "wire-thread",
                "graph_id": "wire-agent",
                "tenant_id": "wire-tenant",
                "generation": 1,
                "input": {"messages": [{"role": "user", "content": text}]},
            }
            if allowed is not None:
                assignment["allowed_tools"] = allowed
            status = await self.adapter.execute(assignment, send, cancel if cancel is not None else asyncio.Event())
        finally:
            reset_run(run)
            reset_tenant(tenant)
        return status, events

    @staticmethod
    def final(events):
        return [event["data"] for event in events if event["method"] == "values"][-1]

    async def test_chat_tools_and_two_turn_history_meter_only_the_current_run(self):
        self.install(
            "chat",
            [
                wire("chat", tool="lookup", args={"query": "Runkite"}),
                wire("chat", text="First answer"),
                wire("chat", text="Second answer"),
            ],
        )
        status, first = await self.execute()
        self.assertEqual(status, "success")
        self.assertEqual(
            self.tool_usage, [{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14, "model": MODEL}]
        )
        self.assertEqual(self.final(first)["usage"]["total_tokens"], 28)
        status, second = await self.execute("next-run", text="next question")
        self.assertEqual(status, "success")
        self.assertEqual(self.final(second)["usage"]["total_tokens"], 14)
        self.assertIn("First answer", json.dumps(self.requests[-1]["messages"]))
        self.assertIn("local synthetic fact", json.dumps(self.requests[-1]["messages"]))
        saved = json.loads(next(iter(self.blobs.values())))
        self.assertEqual(saved["history"]["usage"]["input_tokens"], 30)
        self.assertEqual(self.puts, 2)

    async def test_responses_typed_validation_retry_commits_only_the_completed_json(self):
        self.install(
            "responses",
            [
                wire("responses", tool="lookup", args={"query": "Runkite"}),
                wire("responses", tool="final_result", args={"count": "invalid"}, identity="invalid-output"),
                wire("responses", tool="final_result", args={"count": 7}, identity="valid-output"),
            ],
            typed=True,
        )
        status, events = await self.execute(allowed=["lookup"])
        self.assertEqual(status, "success")
        self.assertEqual(self.final(events)["result"], {"count": 7, "note": "validated"})
        self.assertEqual(self.final(events)["usage"]["total_tokens"], 42)
        self.assertEqual(len(self.tool_usage), 1)
        self.assertEqual(self.puts, 1)
        self.assertEqual(sum(event["method"] == "values" and "result" in event["data"] for event in events), 1)
        self.assertFalse(
            any(event["method"] == "messages" for event in events), "tool argument JSON must not become assistant text"
        )

    async def test_empty_ordinary_allowlist_does_not_deny_synthetic_output_schema_tools(self):
        self.install(
            "chat",
            [
                wire("chat", tool="final_result", args={"count": "invalid"}),
                wire("chat", tool="final_result", args={"count": 7}, identity="valid-output"),
            ],
            typed=True,
        )
        status, events = await self.execute(allowed=[])
        self.assertEqual(status, "success")
        self.assertEqual(self.final(events)["result"]["count"], 7)
        self.assertEqual(self.final(events)["usage"]["total_tokens"], 28)
        self.assertFalse(self.tool_usage)

    async def test_http_error_after_tool_retains_observed_usage_without_false_commit(self):
        self.install("responses", [wire("responses", tool="lookup", args={"query": "Runkite"}), 503])
        with self.assertLogs(level="ERROR"):
            status, events = await self.execute()
        self.assertEqual(status, "error")
        self.assertEqual(self.final(events)["usage"]["total_tokens"], 14)
        self.assertEqual(self.final(events)["usage"]["model"], MODEL)
        self.assertEqual(self.puts, 0)

    async def test_cancel_next_model_stream_preserves_first_usage_and_closes_transport(self):
        waiting = WaitingStream()
        self.install("responses", [wire("responses", tool="lookup", args={"query": "Runkite"}), waiting])
        cancel = asyncio.Event()
        task = asyncio.create_task(self.execute(cancel=cancel))

        async def cleanup():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)

        self.addAsyncCleanup(cleanup)
        await asyncio.wait_for(waiting.entered.wait(), 3)
        cancel.set()
        status, events = await asyncio.wait_for(task, 3)
        self.assertEqual(status, "interrupted")
        self.assertEqual(self.final(events)["usage"]["total_tokens"], 14)
        self.assertTrue(waiting.closed.is_set())
        self.assertEqual(self.puts, 0)


if __name__ == "__main__":
    unittest.main()
