"""Bare-concat, id-less reducer probe (dogfood-only, Python side).

Deliberately uses ``operator.add`` instead of ``add_messages``/MessagesState
and strips any id LangChain stamps onto the model reply. Without both,
``skip_ids`` alone can still dedup (Gemini ``AIMessage`` objects often carry
an ``lc_run--…`` id even when the reducer never assigned one). This probe
forces the ``skip_prefix`` path that protects truly id-less graphs.
"""

from __future__ import annotations

import operator
import sys
from pathlib import Path
from typing import Annotated, TypedDict

from langchain_core.messages import BaseMessage
from langchain_google_genai import ChatGoogleGenerativeAI
from langgraph.graph import END, START, StateGraph

sys.path.insert(0, str(Path(__file__).resolve().parents[1]) + "/gemini")
from _env import gemini_model, gemini_temperature, require_google_api_key  # noqa: E402


class State(TypedDict):
    # operator.add is plain list concatenation -- no id stamping, no
    # merge-by-id, unlike add_messages. Deliberately not the recommended
    # convention; that is the whole point of this probe.
    messages: Annotated[list[BaseMessage], operator.add]


_llm = ChatGoogleGenerativeAI(
    model=gemini_model(),
    google_api_key=require_google_api_key(),
    temperature=gemini_temperature(),
)


def agent_node(state: State) -> State:
    reply = _llm.invoke(state["messages"])
    # Chat model replies often arrive with an lc_run id even when the
    # reducer never assigned one; clear it so FinOps cannot lean on
    # skip_ids and must use skip_prefix for this agent.
    reply.id = None
    return {"messages": [reply]}


builder = StateGraph(State)
builder.add_node("agent", agent_node)
builder.add_edge(START, "agent")
builder.add_edge("agent", END)

graph = builder.compile()
