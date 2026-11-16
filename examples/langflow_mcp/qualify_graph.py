"""Provider-free graph used by the native Langflow MCP qualification."""

from typing import Annotated, TypedDict

from langgraph.graph import END, START, StateGraph


class State(TypedDict):
    messages: Annotated[list[dict], lambda previous, incoming: previous + incoming]


def reply(state: State) -> State:
    message = state["messages"][-1]["content"]
    if message == "__langflow_qualification_error__":
        raise RuntimeError("Scripted qualification failure; no provider was called")
    return {"messages": [{"role": "ai", "content": message}]}


builder = StateGraph(State)
builder.add_node("reply", reply)
builder.add_edge(START, "reply")
builder.add_edge("reply", END)
graph = builder.compile()
