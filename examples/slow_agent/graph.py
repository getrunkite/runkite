"""Slow agent for VG-002 (cancel) validation.

A multi-step agent where each node takes 2 seconds. This gives enough
time to send a cancel request mid-execution and verify the runner stops.
"""

import asyncio
from typing import Annotated, TypedDict

from langgraph.graph import StateGraph, START, END


class State(TypedDict):
    messages: Annotated[list[dict], lambda a, b: a + b]
    step: int


async def step_1(state: State) -> State:
    """First step -- takes 2 seconds."""
    await asyncio.sleep(2)
    return {
        "messages": [{"role": "ai", "content": "Step 1 complete"}],
        "step": 1,
    }


async def step_2(state: State) -> State:
    """Second step -- takes 2 seconds."""
    await asyncio.sleep(2)
    return {
        "messages": [{"role": "ai", "content": "Step 2 complete"}],
        "step": 2,
    }


async def step_3(state: State) -> State:
    """Third step -- takes 2 seconds."""
    await asyncio.sleep(2)
    return {
        "messages": [{"role": "ai", "content": "Step 3 complete"}],
        "step": 3,
    }


# Build a 3-step sequential graph. Total time: ~6 seconds.
# Cancel should arrive during step 1 or 2.
builder = StateGraph(State)
builder.add_node("step_1", step_1)
builder.add_node("step_2", step_2)
builder.add_node("step_3", step_3)

builder.add_edge(START, "step_1")
builder.add_edge("step_1", "step_2")
builder.add_edge("step_2", "step_3")
builder.add_edge("step_3", END)

graph = builder.compile()
