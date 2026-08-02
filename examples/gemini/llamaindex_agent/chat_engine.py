"""LlamaIndex SimpleChatEngine powered by Gemini (real LLM)."""

from __future__ import annotations

import sys
from pathlib import Path

from llama_index.core.chat_engine import SimpleChatEngine
from llama_index.llms.google_genai import GoogleGenAI

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from _env import gemini_model, gemini_temperature, require_google_api_key  # noqa: E402

_llm = GoogleGenAI(
    model=gemini_model(),
    api_key=require_google_api_key(),
    temperature=gemini_temperature(),
)

chat_engine = SimpleChatEngine.from_defaults(
    llm=_llm,
    system_prompt="You are a terse, helpful assistant. Reply in one short sentence.",
)
