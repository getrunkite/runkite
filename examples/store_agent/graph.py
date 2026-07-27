"""Store dual-mode validation agent.

Uses LangGraph's standard get_store() injection to read/increment a
cross-thread visit counter -- proves store.py's RunkiteStore is wired
into the runner correctly, not just unit-testable in isolation. Any
thread hitting this graph increments the SAME counter, since it's stored
under a fixed namespace/key rather than per-thread state.
"""

from typing import Annotated, TypedDict

from langgraph.config import get_store
from langgraph.graph import END, START, StateGraph


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]


def visit_node(state: State) -> State:
    store = get_store()
    item = store.get(("store_agent",), "visit_count")
    count = (item.value.get("count", 0) if item else 0) + 1
    store.put(("store_agent",), "visit_count", {"count": count})
    return {"messages": [{"role": "ai", "content": f"visit_count={count}"}]}


builder = StateGraph(State)
builder.add_node("visit", visit_node)
builder.add_edge(START, "visit")
builder.add_edge("visit", END)

graph = builder.compile()
