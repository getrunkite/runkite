"""Provider-free LangGraph used only by the integration qualifier.

Ordinary messages echo through the real worker and its proxy checkpoints.
The two named sentinels exercise delayed delivery and a terminal graph error.
"""

import time
from typing import Annotated, TypedDict

from langgraph.graph import END, START, StateGraph

ERROR_MESSAGE = "__runkite_qualification_error__"
DELAY_MESSAGE = "__runkite_qualification_delay__"


class State(TypedDict):
    messages: Annotated[list[dict], lambda previous, incoming: previous + incoming]


def echo(state: State) -> State:
    message = state["messages"][-1]["content"]
    if message == ERROR_MESSAGE:
        raise RuntimeError("Scripted qualification error; no provider was called")
    if message == DELAY_MESSAGE:
        time.sleep(0.3)
    return {"messages": [{"role": "ai", "content": message}]}


builder = StateGraph(State)
builder.add_node("echo", echo)
builder.add_edge(START, "echo")
builder.add_edge("echo", END)
graph = builder.compile()
