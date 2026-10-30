"""Native offline model/tool fixture used only by the managed runner E2E suite."""

from __future__ import annotations

import asyncio
import json
import os
from typing import Any

from agent_framework import (
    Agent,
    BaseChatClient,
    ChatMiddlewareLayer,
    ChatResponseUpdate,
    Content,
    FunctionInvocationLayer,
    FunctionTool,
    UsageDetails,
)

from agent_framework_adapter import AgentFrameworkRunContext


def record(context: AgentFrameworkRunContext, kind: str, **details: Any) -> None:
    with open(os.environ["RUNKITE_MS_E2E_COUNTERS"], "a", encoding="utf-8") as handle:
        handle.write(
            json.dumps(
                {
                    "kind": kind,
                    "run_id": context.run_id,
                    "tenant_id": context.tenant_id,
                    "generation": context.generation,
                    **details,
                }
            )
            + "\n"
        )


def build_agent(context: AgentFrameworkRunContext, *, mode: str = "normal") -> Agent:
    record(context, "factory")
    step = 0

    def record_effect(text: str) -> dict[str, str]:
        """Record one test-only local effect and return its text."""
        record(context, "tool", text=text)
        return {"recorded": text}

    class ScriptedClient(FunctionInvocationLayer[Any], ChatMiddlewareLayer[Any], BaseChatClient[Any]):
        async def _inner_get_response(self, *, messages, stream=False, options=None, **kwargs):  # noqa: ANN001
            nonlocal step
            users = [
                "".join(getattr(content, "text", "") or "" for content in (message.contents or []))
                for message in messages
                if message.role == "user"
            ]
            tool_results = sum(
                getattr(content, "type", None) == "function_result"
                for message in messages
                for content in (message.contents or [])
            )
            record(context, "model", users=users, tool_results=tool_results, step=step)
            current_step = step
            step += 1
            if not stream:
                raise AssertionError("The managed adapter must use the native streaming path")
            response_id = f"response-{context.run_id}-{current_step}"
            if current_step == 0:
                usage = UsageDetails(input_token_count=10, output_token_count=4 if mode in {"slow", "error"} else 2)

                async def tool_call():
                    yield ChatResponseUpdate(
                        role="assistant",
                        model="offline-managed-ms-e2e",
                        response_id=response_id,
                        contents=[
                            Content.from_function_call(
                                call_id=f"effect-{context.run_id}",
                                name="record_effect",
                                arguments={"text": users[-1]},
                            ),
                            Content.from_usage(usage),
                        ],
                    )

                return self._build_response_stream(tool_call())
            if mode == "error":
                record(context, "failed")
                raise ValueError("synthetic Microsoft model failure after observed usage")
            if mode == "slow":

                async def held_turn():
                    try:
                        record(context, "stalled")
                        yield ChatResponseUpdate(
                            role="assistant",
                            model="offline-managed-ms-e2e",
                            response_id=response_id,
                            contents=[Content.from_text("Waiting")],
                        )
                        while True:
                            await asyncio.sleep(0.02)
                    finally:
                        if context.cancel_event.is_set():
                            record(context, "canceled")

                return self._build_response_stream(held_turn())

            async def answer():
                yield ChatResponseUpdate(
                    role="assistant",
                    model="offline-managed-ms-e2e",
                    response_id=response_id,
                    contents=[Content.from_text(f"users={len(users)}; tool_results={tool_results}")],
                )
                yield ChatResponseUpdate(
                    role="assistant",
                    model="offline-managed-ms-e2e",
                    response_id=response_id,
                    contents=[Content.from_usage(UsageDetails(input_token_count=10, output_token_count=2))],
                )

            return self._build_response_stream(answer())

    tool = FunctionTool(name="record_effect", description="Record the synthetic E2E effect.", func=record_effect)
    return Agent(client=ScriptedClient(), instructions="Use record_effect once, then answer.", tools=[tool])


def agent(context: AgentFrameworkRunContext) -> Agent:
    return build_agent(context)


def slow_agent(context: AgentFrameworkRunContext) -> Agent:
    return build_agent(context, mode="slow")


def error_agent(context: AgentFrameworkRunContext) -> Agent:
    return build_agent(context, mode="error")


def budget_agent(context: AgentFrameworkRunContext) -> Agent:
    return build_agent(context)
