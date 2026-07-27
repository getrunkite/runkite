"""Trivial agent for the cron scheduler example. Every cron fire runs this
graph exactly like any client-triggered run would -- the scheduler is just
another way to create a run, not a special execution path.
"""

from typing import Annotated, TypedDict

from langgraph.graph import StateGraph, START, END


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]


def report_node(state: State) -> State:
    return {
        "messages": [{"role": "ai", "content": "scheduled report generated"}]
    }


builder = StateGraph(State)
builder.add_node("report", report_node)
builder.add_edge(START, "report")
builder.add_edge("report", END)

graph = builder.compile()
