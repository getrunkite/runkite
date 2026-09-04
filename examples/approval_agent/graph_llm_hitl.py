"""Real-LLM HITL probe (dogfood-only, not part of the shipped example set).

Combines a genuine Gemini call with interrupt()/resume in one graph, so we
can prove -- with real provider tokens, not a hand-built AIMessage -- that
FinOps still meters the tokens spent on the turn that ran *before* the
human-in-the-loop pause, exactly like examples/gemini/langgraph_agent but
with a mandatory approval step wedged in the middle.
"""

from __future__ import annotations

import sys
from pathlib import Path
from typing import Annotated, TypedDict

from langchain_core.messages import BaseMessage
from langgraph.checkpoint.memory import MemorySaver
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages
from langgraph.types import interrupt
from langchain_google_genai import ChatGoogleGenerativeAI

sys.path.insert(0, str(Path(__file__).resolve().parents[1]) + "/gemini")
from _env import gemini_model, gemini_temperature, require_google_api_key  # noqa: E402


class State(TypedDict):
    messages: Annotated[list[BaseMessage], add_messages]
    approved: bool


_llm = ChatGoogleGenerativeAI(
    model=gemini_model(),
    google_api_key=require_google_api_key(),
    temperature=gemini_temperature(),
)


def draft(state: State) -> State:
    """Real Gemini call -- this is the turn whose tokens must survive the
    interrupt below even though the run as a whole ends in "interrupted"."""
    reply = _llm.invoke(state["messages"])
    return {"messages": [reply]}


def gate(state: State) -> State:
    approval = interrupt({"question": "Send this reply?", "draft": state["messages"][-1].content})
    return {"approved": bool(approval)}


builder = StateGraph(State)
builder.add_node("draft", draft)
builder.add_node("gate", gate)
builder.add_edge(START, "draft")
builder.add_edge("draft", "gate")
builder.add_edge("gate", END)

graph = builder.compile(checkpointer=MemorySaver())
