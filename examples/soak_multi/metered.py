"""Offline cache-accounting fixture with explicitly synthetic token usage."""

from typing import Annotated, TypedDict

from langchain_core.messages import AIMessage, AnyMessage
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages


class State(TypedDict):
    messages: Annotated[list[AnyMessage], add_messages]


def reply(state: State) -> State:
    return {
        "messages": [
            AIMessage(
                content="Synthetic HA accounting reply",
                usage_metadata={"input_tokens": 20, "output_tokens": 4, "total_tokens": 24},
                response_metadata={"model_name": "ha-offline-synthetic"},
            )
        ]
    }


builder = StateGraph(State)
builder.add_node("reply", reply)
builder.add_edge(START, "reply")
builder.add_edge("reply", END)
graph = builder.compile()
