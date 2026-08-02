"""Plain LangChain chain powered by Gemini (real LLM)."""

from __future__ import annotations

import sys
from pathlib import Path

from langchain_core.output_parsers import StrOutputParser
from langchain_core.prompts import ChatPromptTemplate
from langchain_google_genai import ChatGoogleGenerativeAI

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from _env import gemini_model, gemini_temperature, require_google_api_key  # noqa: E402

_prompt = ChatPromptTemplate.from_messages(
    [
        ("system", "You are a terse, helpful assistant. Reply in one short sentence."),
        ("human", "{input}"),
    ]
)
_llm = ChatGoogleGenerativeAI(
    model=gemini_model(),
    google_api_key=require_google_api_key(),
    temperature=gemini_temperature(),
)

chain = _prompt | _llm | StrOutputParser()
