"""Simulated "future/unrecognized provider" probe (dogfood-only).

Makes a genuine Gemini call, then deliberately strips usage_metadata and
response_metadata before returning -- exactly what a brand-new LLM
provider's LangChain integration would look like on day one, before it
adopts LangChain's standardized usage_metadata contract (or an
integration this codebase has simply never been tested against). Proves
the usage_unmetered alert fires on a REAL model turn, not just a
hand-built test fixture.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Annotated, TypedDict

from langchain_core.messages import AIMessage, BaseMessage
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages
from langchain_google_genai import ChatGoogleGenerativeAI

sys.path.insert(0, str(Path(__file__).resolve().parents[1]) + "/gemini")
from _env import gemini_model, gemini_temperature, require_google_api_key  # noqa: E402


class State(TypedDict):
    messages: Annotated[list[BaseMessage], add_messages]


_llm = ChatGoogleGenerativeAI(
    model=gemini_model(),
    google_api_key=require_google_api_key(),
    temperature=gemini_temperature(),
)


def agent_from_unknown_provider(state: State) -> State:
    real_reply = _llm.invoke(state["messages"])
    # Simulates an integration that has not adopted (or predates) the
    # usage_metadata standard -- the reply is completely real, the
    # tokens were genuinely spent, but nothing in this codebase's
    # recognized extraction paths can see them.
    stripped = AIMessage(content=real_reply.content)
    return {"messages": [stripped]}


builder = StateGraph(State)
builder.add_node("agent", agent_from_unknown_provider)
builder.add_edge(START, "agent")
builder.add_edge("agent", END)

graph = builder.compile()
