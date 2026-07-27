"""Approval agent for VG-003 (HITL) validation.

A two-step agent that:
1. Proposes an action and interrupts for human approval
2. After approval, completes the action

Uses LangGraph's interrupt() for HITL and MemorySaver for checkpoint
persistence across the two runs (interrupt + resume).
"""

from typing import Annotated, TypedDict

from langgraph.checkpoint.memory import MemorySaver
from langgraph.graph import StateGraph, START, END
from langgraph.types import interrupt, Command


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]
    approved: bool


def propose_action(state: State) -> State:
    """Propose an action and wait for human approval."""
    proposal = {
        "action": "send_email",
        "to": "alice@example.com",
        "subject": "Hello from agent",
    }

    # This will interrupt the graph and wait for human input
    approval = interrupt(proposal)

    # When resumed, approval contains the human's response
    return {
        "messages": [{"role": "ai", "content": f"Action approved: {approval}"}],
        "approved": bool(approval),
    }


def execute_action(state: State) -> State:
    """Execute the approved action."""
    if state.get("approved"):
        return {
            "messages": [{"role": "ai", "content": "Email sent to alice@example.com successfully!"}],
        }
    else:
        return {
            "messages": [{"role": "ai", "content": "Action was not approved. Cancelled."}],
        }


# Build the graph with a checkpointer (required for HITL)
builder = StateGraph(State)
builder.add_node("propose", propose_action)
builder.add_node("execute", execute_action)

builder.add_edge(START, "propose")
builder.add_edge("propose", "execute")
builder.add_edge("execute", END)

# Compile with MemorySaver so state persists between interrupt and resume
checkpointer = MemorySaver()
graph = builder.compile(checkpointer=checkpointer)
