"""Trivial agent for the custom_routes_agent example -- the point of this
example is app.py (the custom ASGI app), not this graph. Reuses the same
echo shape as examples/echo_agent so it needs no explanation of its own.
"""

from typing import Annotated, TypedDict

from langgraph.graph import END, START, StateGraph


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]


def echo_node(state: State) -> State:
    last_message = state["messages"][-1]
    return {"messages": [{"role": "ai", "content": last_message["content"]}]}


builder = StateGraph(State)
builder.add_node("echo", echo_node)
builder.add_edge(START, "echo")
builder.add_edge("echo", END)

graph = builder.compile()
