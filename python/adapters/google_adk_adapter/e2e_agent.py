"""Actual ADK model, local tools and state using only synthetic offline data."""

from __future__ import annotations

import asyncio
import json
import os
from typing import Any

from google.adk.agents import LlmAgent, SequentialAgent
from google.adk.models.base_llm import BaseLlm
from google.adk.models.llm_response import LlmResponse
from google.adk.tools import FunctionTool
from google.adk.tools.tool_context import ToolContext
from google.genai import types

from google_adk_adapter import GoogleAdkRunContext

MODEL = "offline-google-adk-e2e"


def record(context: GoogleAdkRunContext, kind: str, **details: Any) -> None:
    with open(os.environ["RUNKITE_GOOGLE_ADK_E2E_COUNTERS"], "a", encoding="utf-8") as handle:
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


def build_agent(context: GoogleAdkRunContext, *, mode: str = "normal") -> LlmAgent:
    record(context, "factory", mode=mode)
    step = 0

    def record_effect(text: str, tool_context: ToolContext) -> dict[str, Any]:
        """Record one local synthetic effect in thread-scoped session state."""
        before = tool_context.state.get("recorded_effects", 0)
        tool_context.state["recorded_effects"] = before + 1
        record(context, "tool", text=text, prior_effects=before)
        return {"recorded": text, "effects": before + 1}

    def unsupported_state(text: str, tool_context: ToolContext) -> dict[str, str]:
        """Attempt unsupported app or user shared state without external effects."""
        record(context, "unsupported_state_attempt")
        tool_context.state[f"{'app' if mode == 'app_state' else 'user'}:unsupported"] = text
        return {"attempted": text}

    class ScriptedModel(BaseLlm):
        async def generate_content_async(self, llm_request, stream=False):
            nonlocal step
            users, tool_results = [], 0
            for content in llm_request.contents:
                for part in content.parts or []:
                    if content.role == "user" and part.text and not part.thought:
                        users.append(part.text)
                    if part.function_response is not None:
                        tool_results += 1
            current_step = step
            step += 1
            record(context, "model", users=users, tool_results=tool_results, step=current_step)
            if current_step == 1 and mode == "error":
                record(context, "failed")
                raise ValueError("synthetic ADK provider failure after observed usage")
            if current_step == 1 and mode == "slow":
                try:
                    record(context, "stalled")
                    yield LlmResponse(
                        content=types.Content(role="model", parts=[types.Part(text="Waiting")]), partial=True
                    )
                    while True:
                        await asyncio.sleep(0.02)
                finally:
                    if context.cancel_event.is_set():
                        record(context, "canceled")
            if current_step == 0:
                name = "unsupported_state" if mode in {"app_state", "user_state"} else "record_effect"
                content = types.Content(
                    role="model",
                    parts=[
                        types.Part(
                            function_call=types.FunctionCall(
                                id=f"effect-{context.run_id}", name=name, args={"text": users[-1]}
                            )
                        )
                    ],
                )
                tokens_out = 4 if mode in {"slow", "error"} else 2
            else:
                text = f"users={len(users)}; tool_results={tool_results}"
                # ADK streams deltas, then repeats the aggregate in its final
                # nonpartial event. The managed receipt must include text once.
                if stream:
                    middle = len(text) // 2
                    for delta in (text[:middle], text[middle:]):
                        yield LlmResponse(
                            content=types.Content(role="model", parts=[types.Part(text=delta)]), partial=True
                        )
                content = types.Content(role="model", parts=[types.Part(text=text)])
                tokens_out = 2
            yield LlmResponse(
                model_version=MODEL,
                content=content,
                partial=False,
                turn_complete=True,
                finish_reason=types.FinishReason.STOP,
                usage_metadata=types.GenerateContentResponseUsageMetadata(
                    prompt_token_count=10,
                    candidates_token_count=tokens_out,
                    total_token_count=10 + tokens_out,
                ),
            )

    tool = unsupported_state if mode in {"app_state", "user_state"} else record_effect
    return LlmAgent(
        name="offline_managed_e2e",
        model=ScriptedModel(model=MODEL),
        instruction="Call the one local function once, then return a short text answer.",
        tools=[FunctionTool(tool, require_confirmation=mode == "confirmation")],
    )


def agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context)


def slow_agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context, mode="slow")


def error_agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context, mode="error")


def budget_agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context)


def blocked_agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context)


def deny_all_agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context)


def workflow_agent(context: GoogleAdkRunContext) -> SequentialAgent:
    return SequentialAgent(name="unsupported_workflow", sub_agents=[build_agent(context)])


def confirmation_agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context, mode="confirmation")


def app_state_agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context, mode="app_state")


def user_state_agent(context: GoogleAdkRunContext) -> LlmAgent:
    return build_agent(context, mode="user_state")
