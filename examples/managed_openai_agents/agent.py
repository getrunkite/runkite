"""Offline public SDK model and local tool for managed-run demonstrations."""

from __future__ import annotations

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


class OfflineModel(Model):
    def __init__(self, run_id: str) -> None:
        self.run_id = run_id

    async def get_response(self, *args, **kwargs):
        raise NotImplementedError("This example uses Runner.run_streamed")

    async def stream_response(self, system_instructions, input, *args, **kwargs):
        last_user = max(index for index, item in enumerate(input) if item.get("role") == "user")
        tool_output = next(
            (item["output"] for item in input[last_user + 1 :] if item.get("type") == "function_call_output"),
            None,
        )
        if tool_output is None:
            output = [
                ResponseFunctionToolCall(
                    id=f"{self.run_id}:lookup-item",
                    call_id=f"{self.run_id}:lookup",
                    name="lookup",
                    arguments='{"query":"Runkite"}',
                    type="function_call",
                    status="completed",
                )
            ]
        else:
            text = f"{tool_output}. You asked: {input[last_user]['content']}"
            output = [
                ResponseOutputMessage(
                    id=f"{self.run_id}:answer",
                    type="message",
                    role="assistant",
                    status="completed",
                    content=[ResponseOutputText(type="output_text", text=text, annotations=[], logprobs=[])],
                )
            ]
            yield ResponseTextDeltaEvent(
                type="response.output_text.delta",
                item_id=output[0].id,
                output_index=0,
                content_index=0,
                delta=text,
                logprobs=[],
                sequence_number=0,
            )
        for index, item in enumerate(output):
            yield ResponseOutputItemDoneEvent(
                type="response.output_item.done", item=item, output_index=index, sequence_number=index + 1
            )
        yield ResponseCompletedEvent(
            type="response.completed",
            sequence_number=len(output) + 1,
            response=Response(
                id=f"{self.run_id}:response:{tool_output is not None}",
                object="response",
                created_at=0,
                model="offline-openai-agents",
                output=output,
                tool_choice="auto",
                tools=[],
                parallel_tool_calls=False,
                usage=ResponseUsage(
                    input_tokens=10,
                    output_tokens=2,
                    total_tokens=12,
                    input_tokens_details=InputTokensDetails(cached_tokens=0, cache_write_tokens=0),
                    output_tokens_details=OutputTokensDetails(reasoning_tokens=0),
                ),
            ),
        )


def create_agent(context):
    @function_tool
    async def lookup(query: str) -> str:
        """Return an offline fact without contacting an external service."""
        return f"{query} dispatches this native SDK agent through the control plane"

    return Agent(name="Managed offline OpenAI agent", model=OfflineModel(context.run_id), tools=[lookup])
