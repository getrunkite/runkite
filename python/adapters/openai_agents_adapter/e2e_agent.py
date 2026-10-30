"""Native offline model/tool fixture for installed managed OpenAI runner tests."""

from __future__ import annotations

import asyncio
import json
import os
from typing import Any

from agents import Agent, Model, function_tool
from openai.types.responses import (
    Response,
    ResponseCompletedEvent,
    ResponseFunctionToolCall,
    ResponseOutputItemDoneEvent,
    ResponseOutputMessage,
    ResponseOutputText,
    ResponseTextDeltaEvent,
    ResponseUsage,
)
from openai.types.responses.response_usage import InputTokensDetails, OutputTokensDetails

from openai_agents_adapter import OpenAIAgentsRunContext


def record(context: OpenAIAgentsRunContext, kind: str, **details: Any) -> None:
    with open(os.environ["RUNKITE_OPENAI_E2E_COUNTERS"], "a", encoding="utf-8") as handle:
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


def build_agent(context: OpenAIAgentsRunContext, *, mode: str = "normal") -> Agent:
    record(context, "factory", mode=mode)
    step = 0

    @function_tool
    def record_effect(text: str) -> dict[str, str]:
        """Record one synthetic local effect and return its text."""
        record(context, "tool", text=text)
        return {"recorded": text}

    class ScriptedModel(Model):
        model = "offline-managed-e2e"

        async def get_response(self, *args, **kwargs):
            raise AssertionError("Managed runner must use the public streaming API")

        async def stream_response(self, system_instructions, input, *args, **kwargs):  # noqa: ANN001
            nonlocal step
            users = []
            tool_results = 0
            for item in input:
                if item.get("role") == "user":
                    content = item["content"]
                    users.append(
                        content if isinstance(content, str) else "".join(part.get("text", "") for part in content)
                    )
                if item.get("type") == "function_call_output":
                    tool_results += 1
            current_step = step
            step += 1
            record(context, "model", users=users, tool_results=tool_results, step=current_step)
            if current_step == 1 and mode == "error":
                record(context, "failed")
                raise ValueError("synthetic model failure after observed usage")
            if current_step == 1 and mode == "slow":
                try:
                    record(context, "stalled")
                    yield ResponseTextDeltaEvent(
                        type="response.output_text.delta",
                        item_id=f"waiting-{context.run_id}",
                        output_index=0,
                        content_index=0,
                        sequence_number=0,
                        delta="Waiting",
                        logprobs=[],
                    )
                    while True:
                        await asyncio.sleep(0.02)
                finally:
                    if context.cancel_event.is_set():
                        record(context, "canceled")
            if current_step == 0:
                output = [
                    ResponseFunctionToolCall(
                        id=f"function-{context.run_id}",
                        call_id=f"effect-{context.run_id}",
                        name="record_effect",
                        arguments=json.dumps({"text": users[-1]}),
                        type="function_call",
                        status="completed",
                    )
                ]
                tokens_out = 4 if mode in {"slow", "error"} else 2
            else:
                text = "界" * 900_000 if mode == "unicode" else f"users={len(users)}; tool_results={tool_results}"
                output = [
                    ResponseOutputMessage(
                        id=f"message-{context.run_id}",
                        role="assistant",
                        type="message",
                        status="completed",
                        content=[ResponseOutputText(text=text, annotations=[], type="output_text")],
                    )
                ]
                yield ResponseTextDeltaEvent(
                    type="response.output_text.delta",
                    item_id=output[0].id,
                    output_index=0,
                    content_index=0,
                    sequence_number=0,
                    delta=text,
                    logprobs=[],
                )
                tokens_out = 2
            yield ResponseOutputItemDoneEvent(
                type="response.output_item.done", item=output[0], output_index=0, sequence_number=1
            )
            yield ResponseCompletedEvent(
                type="response.completed",
                sequence_number=2,
                response=Response(
                    id=f"response-{context.run_id}-{current_step}",
                    created_at=123,
                    model="offline-managed-e2e",
                    object="response",
                    output=output,
                    tool_choice="auto",
                    tools=[],
                    parallel_tool_calls=False,
                    top_p=None,
                    usage=ResponseUsage(
                        input_tokens=10,
                        output_tokens=tokens_out,
                        total_tokens=10 + tokens_out,
                        input_tokens_details=InputTokensDetails(cached_tokens=0, cache_write_tokens=0),
                        output_tokens_details=OutputTokensDetails(reasoning_tokens=0),
                    ),
                ),
            )

    return Agent(
        name="offline-managed-e2e",
        model=ScriptedModel(),
        instructions="Use record_effect once, then answer.",
        tools=[record_effect],
    )


def agent(context: OpenAIAgentsRunContext) -> Agent:
    return build_agent(context)


def slow_agent(context: OpenAIAgentsRunContext) -> Agent:
    return build_agent(context, mode="slow")


def error_agent(context: OpenAIAgentsRunContext) -> Agent:
    return build_agent(context, mode="error")


def unsupported_agent(context: OpenAIAgentsRunContext) -> Agent:
    result = build_agent(context)
    result.handoffs = [Agent(name="unsupported-handoff", model=result.model)]
    return result


def blocked_agent(context: OpenAIAgentsRunContext) -> Agent:
    return build_agent(context)


def deny_all_agent(context: OpenAIAgentsRunContext) -> Agent:
    return build_agent(context)


def unicode_agent(context: OpenAIAgentsRunContext) -> Agent:
    return build_agent(context, mode="unicode")
