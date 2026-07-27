"""Factory graph example -- proves LangGraph SDK-compatible per-request
graph factories work end-to-end: a fresh graph instance built per run,
with runtime.user populated from whatever the control plane's auth
provider resolved for the request that created the run.

Mirrors the shape of a real production graph.py using this pattern
(`async def graph(config, runtime) -> AsyncContextManager[CompiledStateGraph]`,
authored against the LangGraph SDK's own documented ServerRuntime API --
see docs.langchain.com/langsmith/graph-rebuild) without any real business
logic -- just enough to prove the mechanism: a
module-level counter that would leak across concurrent runs if the graph
were shared (the bug this feature exists to avoid), and the authenticated
identity echoed back in the response.
"""

from typing import Annotated, TypedDict

from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages
from langgraph_sdk.runtime import ServerRuntime


class State(TypedDict):
    messages: Annotated[list, add_messages]


_instance_counter = 0


async def graph(config: dict, runtime: ServerRuntime):
    """Per-request factory (any parameter order/subset of
    config/runtime is supported -- see factory_graph.py)."""
    global _instance_counter
    _instance_counter += 1
    my_instance_id = _instance_counter

    user = runtime.user
    identity = user.identity if user else "anonymous"

    def respond(state: State) -> dict:
        return {
            "messages": [
                {"role": "ai", "content": f"instance={my_instance_id} identity={identity}"},
            ]
        }

    builder = StateGraph(State)
    builder.add_node("respond", respond)
    builder.add_edge(START, "respond")
    builder.add_edge("respond", END)
    return builder.compile()
