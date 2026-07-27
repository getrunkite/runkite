"""Simple echo agent for spike validation (VG-001).

Receives a user message and echoes it back. Uses LangGraph StateGraph
to prove the bridge works with real LangGraph graphs.
"""

from typing import Annotated, TypedDict

from langgraph.graph import StateGraph, START, END


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]


def echo_node(state: State) -> State:
    """Echo the last user message back."""
    last_message = state["messages"][-1]
    return {
        "messages": [{"role": "ai", "content": last_message["content"]}]
    }


# Build the graph
builder = StateGraph(State)
builder.add_node("echo", echo_node)
builder.add_edge(START, "echo")
builder.add_edge("echo", END)

graph = builder.compile()
