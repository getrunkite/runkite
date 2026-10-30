"""Offline native provider fixture for installed Pydantic AI runner tests.

The real public OpenAIChatModel parses synthetic SSE through MockTransport.
Token counts and model names are fictional fixture data; no network is used.
"""

from __future__ import annotations

import asyncio
import json
import os
from dataclasses import dataclass
from typing import Any

import httpx2
from openai import AsyncOpenAI
from pydantic import BaseModel, Field
from pydantic_ai import Agent, RunContext
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.providers.openai import OpenAIProvider

from pydantic_ai_adapter import PydanticAIAgentDefinition, PydanticAIRunContext

MODEL = "offline-pydantic-e2e"


def _text(value: Any) -> str:
    if isinstance(value, str):
        return value
    return "".join(part.get("text", "") for part in value or [] if isinstance(part, dict))


def record(context: PydanticAIRunContext, kind: str, **details: Any) -> None:
    with open(os.environ["RUNKITE_PYDANTIC_E2E_COUNTERS"], "a", encoding="utf-8") as handle:
        handle.write(
            json.dumps(
                {
                    "kind": kind,
                    "run_id": context.run_id,
                    "thread_id": context.thread_id,
                    "tenant_id": context.tenant_id,
                    "generation": context.generation,
                    **details,
                }
            )
            + "\n"
        )


@dataclass(frozen=True)
class FixtureDeps:
    tenant_id: str
    run_id: str


class TypedAnswer(BaseModel):
    count: int = Field(strict=True, ge=0)
    note: str
    tenant: str


def build_agent(context: PydanticAIRunContext, *, mode: str = "normal") -> PydanticAIAgentDefinition:
    record(context, "factory", mode=mode)
    step = 0
    output_only = mode == "typed_output_only"
    typed = mode in {"typed", "typed_output_only"}

    class ProviderStream(httpx2.AsyncByteStream):
        def __init__(self, current_step: int, body: dict[str, Any]):
            self.current_step = current_step
            self.body = body
            self.iterator = self.events()

        async def __aiter__(self):
            async for chunk in self.iterator:
                yield chunk

        async def aclose(self):
            await self.iterator.aclose()

        def chunk(self, delta: dict[str, Any], *, finish: str | None = None, usage: dict | None = None) -> bytes:
            return (
                "data: "
                + json.dumps(
                    {
                        "id": f"chat-{context.run_id}-{self.current_step}",
                        "object": "chat.completion.chunk",
                        "created": 123,
                        "model": MODEL,
                        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}] if usage is None else [],
                        "usage": usage,
                    }
                )
                + "\n\n"
            ).encode()

        async def events(self):
            current = self.current_step
            messages = self.body["messages"]
            users = [_text(item["content"]) for item in messages if item.get("role") == "user"]
            if mode == "slow" and current == 1:
                try:
                    record(context, "stalled")
                    yield self.chunk({"content": "Waiting"})
                    while True:
                        await asyncio.sleep(0.02)
                finally:
                    if context.cancel_event.is_set():
                        record(context, "canceled")
            if current == 0 and not output_only:
                name = "record_effect"
                arguments = {"text": users[-1]}
                identity = f"effect-{context.run_id}"
            elif typed:
                names = [item["function"]["name"] for item in self.body["tools"]]
                name = next(item for item in names if item != "record_effect")
                valid = current == (1 if output_only else 2)
                arguments = {
                    "count": 7 if valid else "not-an-integer",
                    "note": "validated",
                    "tenant": context.tenant_id,
                }
                identity = f"typed-{'valid' if valid else 'invalid'}-{context.run_id}"
            else:
                name = None
                arguments = None
                identity = None
            if name:
                yield self.chunk(
                    {
                        "tool_calls": [
                            {
                                "index": 0,
                                "id": identity,
                                "type": "function",
                                "function": {"name": name, "arguments": json.dumps(arguments)},
                            }
                        ]
                    },
                    finish="tool_calls",
                )
            else:
                results = sum(item.get("role") == "tool" for item in messages)
                yield self.chunk({"content": f"users={len(users)}; tool_results={results}"}, finish="stop")
            tokens_out = 4 if mode in {"slow", "error"} and current == 0 else 2
            yield self.chunk(
                {}, usage={"prompt_tokens": 10, "completion_tokens": tokens_out, "total_tokens": 10 + tokens_out}
            )
            yield b"data: [DONE]\n\n"

    async def respond(request: httpx2.Request) -> httpx2.Response:
        nonlocal step
        body = json.loads(request.content)
        current = step
        step += 1
        users = [_text(item["content"]) for item in body["messages"] if item.get("role") == "user"]
        returns = [item for item in body["messages"] if item.get("role") == "tool"]
        record(
            context,
            "model",
            step=current,
            users=users,
            tool_results=len(returns),
            validation_feedback=any(
                "count" in str(item["content"]) and "valid" in str(item["content"]) for item in returns
            ),
        )
        if mode == "error" and current == 1:
            record(context, "failed")
            return httpx2.Response(500, json={"error": {"message": "synthetic provider failure", "type": "fixture"}})
        if not body.get("stream"):
            raise AssertionError("Managed Pydantic AI must use native streaming")
        return httpx2.Response(200, headers={"content-type": "text/event-stream"}, stream=ProviderStream(current, body))

    client = AsyncOpenAI(
        api_key="synthetic-fixture-key",
        base_url="http://offline-fixture.invalid/v1",
        max_retries=0,
        http_client=httpx2.AsyncClient(transport=httpx2.MockTransport(respond), trust_env=False),
    )
    model = OpenAIChatModel(MODEL, provider=OpenAIProvider(openai_client=client))
    agent = Agent(model, deps_type=FixtureDeps, output_type=TypedAnswer if typed else str, retries=2)
    if not output_only:

        @agent.tool
        def record_effect(ctx: RunContext[FixtureDeps], text: str) -> dict[str, str]:
            """Record one synthetic local effect using server-owned dependencies."""
            if ctx.deps != FixtureDeps(context.tenant_id, context.run_id):
                raise AssertionError("Native tool dependencies crossed run or tenant")
            record(context, "tool", text=text)
            return {"recorded": text}

    return PydanticAIAgentDefinition(agent=agent, deps=FixtureDeps(context.tenant_id, context.run_id))


def agent(context: PydanticAIRunContext) -> PydanticAIAgentDefinition:
    return build_agent(context)


def slow_agent(context: PydanticAIRunContext) -> PydanticAIAgentDefinition:
    return build_agent(context, mode="slow")


def error_agent(context: PydanticAIRunContext) -> PydanticAIAgentDefinition:
    return build_agent(context, mode="error")


def typed_agent(context: PydanticAIRunContext) -> PydanticAIAgentDefinition:
    return build_agent(context, mode="typed")


def typed_output_only(context: PydanticAIRunContext) -> PydanticAIAgentDefinition:
    return build_agent(context, mode="typed_output_only")


def blocked_agent(context: PydanticAIRunContext) -> PydanticAIAgentDefinition:
    return build_agent(context)


def deny_all_agent(context: PydanticAIRunContext) -> PydanticAIAgentDefinition:
    return build_agent(context)


def budget_agent(context: PydanticAIRunContext) -> PydanticAIAgentDefinition:
    return build_agent(context)
