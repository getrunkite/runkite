"""Agent-to-Agent (A2A) delegation example: agent calls agent via the same
Agent Protocol API.

Two graphs:
- worker_agent: a plain, deterministic "sub-task" agent (no LLM needed --
  same fake-model-for-testing convention as examples/react_agent).
- coordinator_agent: delegates to worker_agent mid-execution via
  call_agent, then incorporates the sub-agent's result into its own
  final response -- proves the delegation round-trip end to end: a
  request creates a NEW run (with its own run_id, checkpointed
  independently), scoped as a child of the coordinator's run
  (parent_run_id/root_run_id/depth all set), executing with the SAME
  authenticated identity the coordinator itself is running as.
"""

from typing import Annotated, TypedDict

from langchain_core.messages import AIMessage, HumanMessage
from langchain_core.runnables import RunnableConfig
from langgraph.graph import END, START, StateGraph
from langgraph.graph.message import add_messages

from runkite_runner.a2a import call_agent


class State(TypedDict):
    messages: Annotated[list, add_messages]


# --- worker_agent: the sub-task agent being delegated to ---


def worker_node(state: State) -> dict:
    last = state["messages"][-1]
    task = last.content if hasattr(last, "content") else str(last)
    return {"messages": [AIMessage(content=f"worker processed: {task}")]}


_worker_builder = StateGraph(State)
_worker_builder.add_node("work", worker_node)
_worker_builder.add_edge(START, "work")
_worker_builder.add_edge("work", END)
worker_agent = _worker_builder.compile()


# --- coordinator_agent: delegates to worker_agent, then responds ---


async def coordinator_node(state: State, config: RunnableConfig) -> dict:
    last = state["messages"][-1]
    task = last.content if hasattr(last, "content") else str(last)

    result = await call_agent(
        config,
        "worker_agent",
        {"messages": [{"role": "human", "content": task}]},
        wait=True,
    )
    worker_output = (result.get("values") or {}).get("messages", [])
    worker_reply = worker_output[-1]["content"] if worker_output else "(no response from worker)"

    return {"messages": [AIMessage(content=f"coordinator delegated and got: {worker_reply}")]}


_coordinator_builder = StateGraph(State)
_coordinator_builder.add_node("coordinate", coordinator_node)
_coordinator_builder.add_edge(START, "coordinate")
_coordinator_builder.add_edge("coordinate", END)
coordinator_agent = _coordinator_builder.compile()
