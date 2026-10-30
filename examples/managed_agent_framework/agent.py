"""Offline Microsoft Agent Framework factory for managed Runkite runs."""

from __future__ import annotations

from typing import Any

from agent_framework import (
    Agent,
    BaseChatClient,
    ChatResponse,
    ChatResponseUpdate,
    Content,
    FunctionInvocationLayer,
    FunctionTool,
    Message,
    UsageDetails,
)

from agent_framework_adapter import AgentFrameworkRunContext


class ScriptedChatClient(FunctionInvocationLayer[Any], BaseChatClient[Any]):
    """Deterministic chat client: text replies, one local echo tool, real usage."""

    async def _inner_get_response(self, *, messages, stream=False, options=None, **kwargs):  # noqa: ANN001
        text = ""
        for message in messages:
            if getattr(message, "role", None) == "user":
                for part in message.contents or []:
                    if getattr(part, "text", None):
                        text = part.text
        has_tool_result = any(
            getattr(part, "type", None) in {"function_result", "tool_result"}
            or (getattr(part, "call_id", None) and getattr(part, "result", None) is not None)
            for message in messages
            for part in (message.contents or [])
        )
        if has_tool_result:
            reply = "Tool finished."
            usage = UsageDetails(input_token_count=2, output_token_count=1, total_token_count=3)
            if stream:

                async def _done():
                    yield ChatResponseUpdate(
                        contents=[Content.from_text(reply), Content.from_usage(usage)],
                        role="assistant",
                    )

                return self._build_response_stream(_done())
            return ChatResponse(
                messages=[Message(role="assistant", contents=[Content.from_text(reply)])],
                usage_details=usage,
            )
        if "echo" in text.lower():
            call = Content.from_function_call(call_id="echo-1", name="echo", arguments={"text": text})
            if stream:

                async def _tool():
                    yield ChatResponseUpdate(contents=[call], role="assistant")

                return self._build_response_stream(_tool())
            return ChatResponse(messages=[Message(role="assistant", contents=[call])])
        reply = f"Remembered. {text}"
        usage = UsageDetails(input_token_count=10, output_token_count=4, total_token_count=14)
        if stream:

            async def _text():
                mid = max(1, len(reply) // 2)
                yield ChatResponseUpdate(contents=[Content.from_text(reply[:mid])], role="assistant")
                yield ChatResponseUpdate(
                    contents=[Content.from_text(reply[mid:]), Content.from_usage(usage)],
                    role="assistant",
                )

            return self._build_response_stream(_text())
        return ChatResponse(
            messages=[Message(role="assistant", contents=[Content.from_text(reply)])],
            usage_details=usage,
        )


def _echo(text: str) -> dict[str, str]:
    """Return the supplied text without external effects."""
    return {"text": text}


echo = FunctionTool(name="echo", description="Return the supplied text without external effects.", func=_echo)


def create_agent(_context: AgentFrameworkRunContext) -> Agent:
    """Per-run factory. Swap ScriptedChatClient for a provider client in production."""
    return Agent(
        client=ScriptedChatClient(),
        instructions="Answer briefly. Use echo only when the user asks to echo.",
        tools=[echo],
    )
