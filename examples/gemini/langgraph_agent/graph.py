"""LangGraph ReAct agent powered by Gemini (real LLM).

Requires GOOGLE_API_KEY (see repo-root .env.llm / .env.llm.example).
Deterministic tools; non-deterministic model text — use for live N×N
validation, not for golden event fixtures.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Annotated, TypedDict

from langchain_core.messages import BaseMessage
from langchain_core.tools import tool
from langchain_google_genai import ChatGoogleGenerativeAI
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages
from langgraph.prebuilt import ToolNode, tools_condition

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from _env import gemini_model, gemini_temperature, require_google_api_key  # noqa: E402


@tool
def search(query: str) -> str:
    """Search for factual information about a topic."""
    return f"Result for '{query}': Runkite is an Agent Protocol control plane with pluggable runners."


class State(TypedDict):
    messages: Annotated[list[BaseMessage], add_messages]


_llm = ChatGoogleGenerativeAI(
    model=gemini_model(),
    google_api_key=require_google_api_key(),
    temperature=gemini_temperature(),
).bind_tools([search])

_tools = ToolNode([search])


def agent_node(state: State):
    return {"messages": [_llm.invoke(state["messages"])]}


builder = StateGraph(State)
builder.add_node("agent", agent_node)
builder.add_node("tools", _tools)
builder.add_edge(START, "agent")
builder.add_conditional_edges("agent", tools_condition)
builder.add_edge("tools", "agent")
builder.add_edge("agent", END)

graph = builder.compile()
