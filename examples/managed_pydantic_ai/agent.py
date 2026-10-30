"""Offline Pydantic AI agent with the native public FunctionModel."""

from pydantic import BaseModel
from pydantic_ai import Agent
from pydantic_ai.messages import UserPromptPart
from pydantic_ai.models.function import DeltaToolCall, FunctionModel


def create_agent(context):
    async def stream(messages, info):
        text = next(
            part.content
            for message in reversed(messages)
            for part in reversed(message.parts)
            if isinstance(part, UserPromptPart)
        )
        yield "Managed by Runkite. "
        yield str(text)

    return Agent(FunctionModel(stream_function=stream, model_name="offline-pydantic-ai"), name="managed_pydantic_ai")


class Summary(BaseModel):
    count: int
    note: str


def create_typed_agent(context):
    async def stream(messages, info):
        yield {
            0: DeltaToolCall(
                name=info.output_tools[0].name,
                json_args='{"count":7,"note":"validated"}',
                tool_call_id=f"{context.run_id}:result",
            )
        }

    return Agent(
        FunctionModel(stream_function=stream, model_name="offline-pydantic-ai-json"),
        output_type=Summary,
        name="managed_pydantic_ai_typed",
    )
