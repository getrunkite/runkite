"""Minimal AutoGen example -- proves the autogen_adapter runner works
end to end. Implements a hand-written ChatCompletionClient subclass
returning a fixed response (deterministic, no API key/network needed)
so this example runs offline, same convention as
examples/crewai_agent's FakeLLM and examples/langchain_agent's
FakeListChatModel.
"""

from autogen_agentchat.agents import AssistantAgent
from autogen_core.models import (
    ChatCompletionClient,
    CreateResult,
    ModelFamily,
    ModelInfo,
    RequestUsage,
)


class FakeModelClient(ChatCompletionClient):
    """A ChatCompletionClient that returns a fixed response instead of
    calling a real provider. AutoGen's ChatCompletionClient is a
    broader interface than CrewAI's BaseLLM/LangChain's fake chat model
    (streaming, token counting, usage tracking) -- every abstract
    method needs a real, if trivial, implementation, not just the one
    that actually gets called in this example's single-turn use."""

    def __init__(self, response_text: str = "Hello from an AutoGen agent -- no real LLM API call."):
        self._response_text = response_text
        self._total_usage = RequestUsage(prompt_tokens=0, completion_tokens=0)

    async def create(self, messages, *, tools=(), tool_choice="auto", json_output=None, extra_create_args=None, cancellation_token=None) -> CreateResult:
        usage = RequestUsage(prompt_tokens=len(messages), completion_tokens=1)
        self._total_usage = RequestUsage(
            prompt_tokens=self._total_usage.prompt_tokens + usage.prompt_tokens,
            completion_tokens=self._total_usage.completion_tokens + usage.completion_tokens,
        )
        return CreateResult(finish_reason="stop", content=self._response_text, usage=usage, cached=False)

    async def create_stream(self, messages, *, tools=(), tool_choice="auto", json_output=None, extra_create_args=None, cancellation_token=None):
        # Not exercised by AssistantAgent.run() with model_client_stream
        # left at its default (False), but must exist and be a real
        # async generator -- ChatCompletionClient is abstract on this.
        yield await self.create(messages, tools=tools, tool_choice=tool_choice, json_output=json_output, extra_create_args=extra_create_args, cancellation_token=cancellation_token)

    async def close(self) -> None:
        pass

    def actual_usage(self) -> RequestUsage:
        return self._total_usage

    def total_usage(self) -> RequestUsage:
        return self._total_usage

    def count_tokens(self, messages, *, tools=()) -> int:
        return len(messages)

    def remaining_tokens(self, messages, *, tools=()) -> int:
        return 1_000_000

    @property
    def capabilities(self):
        return self.model_info

    @property
    def model_info(self) -> ModelInfo:
        return ModelInfo(vision=False, function_calling=False, json_output=False, family=ModelFamily.UNKNOWN, structured_output=False)


# The runner (autogen_adapter.AutoGenAdapter) loads this exact object
# via "./agent.py:agent" in langgraph.json and calls
# agent.run(task=<last human message text>).
agent = AssistantAgent(
    name="greeter",
    model_client=FakeModelClient(),
    system_message="You are a friendly greeter.",
)
