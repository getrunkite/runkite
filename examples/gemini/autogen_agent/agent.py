"""AutoGen AssistantAgent powered by Gemini (real LLM)."""

from __future__ import annotations

import sys
from pathlib import Path

from autogen_agentchat.agents import AssistantAgent
from autogen_ext.models.openai import OpenAIChatCompletionClient

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from _env import gemini_model, gemini_temperature, require_google_api_key  # noqa: E402

# AutoGen's Google client varies by version; the OpenAI-compatible
# Gemini endpoint keeps the example small and dependency-stable.
_model_client = OpenAIChatCompletionClient(
    model=gemini_model(),
    api_key=require_google_api_key(),
    base_url="https://generativelanguage.googleapis.com/v1beta/openai/",
    temperature=gemini_temperature(),
    model_info={
        "vision": False,
        "function_calling": False,
        "json_output": False,
        "family": "unknown",
        "structured_output": False,
    },
)

agent = AssistantAgent(
    name="greeter",
    model_client=_model_client,
    system_message="You are a friendly greeter. Reply in one short sentence.",
)
